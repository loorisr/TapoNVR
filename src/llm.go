package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------- llm: vision-LLM description of detections --------------
// Snapshots and/or keyframes sampled from the clip are sent to a multimodal
// model behind an OpenAI-compatible API (such as Ollama), which returns a
// short natural-language description stored on the detection row. Disabled by
// default; always async: it never delays
// the webhook, the snapshot chain, the clip or the alerts.

// llmImage is one image sent to the model, with the caption that precedes it.
type llmImage struct {
	label string
	jpeg  []byte
}

// llmOutcome is the stored result of one analysis.
type llmOutcome struct {
	summary string
	model   string
	source  string
	images  int
	ms      float64
}

// ---------------- runtime switch -------------------------------------------

var llmMtx sync.Mutex
var llmOn = false

func llmEnabled() bool {
	llmMtx.Lock()
	defer llmMtx.Unlock()
	return llmOn
}

func setLLMEnabled(b bool) {
	llmMtx.Lock()
	llmOn = b
	llmMtx.Unlock()
	mqttPublishState()
}

// llmAutoActive reports whether new detections should be analyzed automatically.
func llmAutoActive(app *App) bool {
	return app != nil && app.cfg != nil && app.cfg.LLM.Auto && llmEnabled()
}

// llmUsesSnapshots / llmUsesVideo map llm.source onto the two media kinds.
func llmUsesSnapshots(source string) bool { return source == "snapshots" || source == "both" }
func llmUsesVideo(source string) bool     { return source == "video" || source == "both" }

// ---------------- readiness tracking ---------------------------------------
// A detection is analyzed once everything it will ever have is on disk: the
// DB row, the timed snapshot burst (when snapshots are analyzed) and the
// buffer-mode clip (when video is analyzed). Each producer reports when it is
// done, success or not; a deadline runs the analysis anyway (with whatever
// exists) so a lost signal never leaks an entry.

const (
	llmNeedRow = 1 << iota
	llmNeedSnaps
	llmNeedClip
)

type llmPending struct {
	waiting int
	timer   *time.Timer
}

var llmPendMtx sync.Mutex
var llmPend = map[string]*llmPending{}

// llmTrackDeadline bounds the wait: continuous snapshots last up to 3 min and
// a clip ends post_detection seconds after the camera goes inactive.
var llmTrackDeadline = 15 * time.Minute

// llmTrack registers a detection for automatic analysis once its media is
// ready. needSnaps/needClip say which async producers will report later.
func llmTrack(app *App, id string, needSnaps, needClip bool) {
	var p = &llmPending{waiting: llmNeedRow}
	if needSnaps {
		p.waiting |= llmNeedSnaps
	}
	if needClip {
		p.waiting |= llmNeedClip
	}
	llmPendMtx.Lock()
	llmPend[id] = p
	p.timer = time.AfterFunc(llmTrackDeadline, func() {
		llmPendMtx.Lock()
		cur, ok := llmPend[id]
		if ok && cur == p {
			delete(llmPend, id)
		}
		llmPendMtx.Unlock()
		if ok && cur == p && cur.waiting&llmNeedRow == 0 {
			llmEnqueueAuto(app, id)
		}
	})
	llmPendMtx.Unlock()
}

// llmNoteReady marks one producer as done and starts the analysis when it was
// the last one. No-op for detections that are not tracked.
func llmNoteReady(app *App, id string, what int) {
	llmPendMtx.Lock()
	p, ok := llmPend[id]
	if !ok {
		llmPendMtx.Unlock()
		return
	}
	p.waiting &^= what
	var done = p.waiting == 0
	if done {
		p.timer.Stop()
		delete(llmPend, id)
	}
	llmPendMtx.Unlock()
	if done {
		llmEnqueueAuto(app, id)
	}
}

// llmForget drops a tracked detection (its row could not be inserted).
func llmForget(id string) {
	llmPendMtx.Lock()
	if p, ok := llmPend[id]; ok {
		p.timer.Stop()
		delete(llmPend, id)
	}
	llmPendMtx.Unlock()
}

// llmEventAllowed applies llm.events to the detection's merged tags.
func llmEventAllowed(cfg LLMCfg, d *Detection) bool {
	if len(cfg.Events) == 0 {
		return true
	}
	for _, t := range detectionTags(d) {
		if slices.Contains(cfg.Events, t) {
			return true
		}
	}
	return false
}

// llmEnqueueAuto queues the automatic analysis of a ready detection, after
// re-checking the switches and the event filter on its final tags.
func llmEnqueueAuto(app *App, id string) {
	if !llmAutoActive(app) {
		return
	}
	if llmBackoffActive() {
		logIt(LDEBUG, "llm analysis skipped (api failing, backoff)", map[string]string{"id": id})
		return
	}
	d, err := getDetection(app.db, id)
	if err != nil || d == nil || !llmEventAllowed(app.cfg.LLM, d) {
		return
	}
	llmEnqueue(app, id, app.cfg.LLM.Source)
}

// ---------------- queue / worker --------------------------------------------

type llmJob struct {
	app    *App
	id     string
	source string
}

var llmQueueOnce sync.Once
var llmQueue chan llmJob
var llmQueuedMtx sync.Mutex
var llmQueued = map[string]bool{}
var llmDropped int64

// llmRunning is the detection currently being analyzed (guarded by llmQueuedMtx).
var llmRunning string

// llmQueueStart starts the single analysis worker: one request at a time
// keeps cost and local-GPU load predictable.
func llmQueueStart() {
	llmQueueOnce.Do(func() {
		llmQueue = make(chan llmJob, 64)
		go func() {
			for job := range llmQueue {
				llmQueuedMtx.Lock()
				delete(llmQueued, job.id)
				llmRunning = job.id
				llmQueuedMtx.Unlock()
				llmAnalyzeAndStore(job.app, job.id, job.source)
				llmQueuedMtx.Lock()
				llmRunning = ""
				llmQueuedMtx.Unlock()
			}
		}()
	})
}

// llmEnqueue queues one analysis. Returns false when the detection is already
// queued or the backlog is full (64 pending analyses).
func llmEnqueue(app *App, id string, source string) bool {
	llmQueueStart()
	llmQueuedMtx.Lock()
	if llmQueued[id] {
		llmQueuedMtx.Unlock()
		return false
	}
	llmQueued[id] = true
	llmQueuedMtx.Unlock()
	select {
	case llmQueue <- llmJob{app: app, id: id, source: source}:
		return true
	default:
		llmQueuedMtx.Lock()
		delete(llmQueued, id)
		llmQueuedMtx.Unlock()
		atomic.AddInt64(&llmDropped, 1)
		logIt(LWARN, "llm analysis dropped (queue full)", map[string]string{"id": id})
		return false
	}
}

// llmIsPending reports whether an analysis of the detection is queued or
// waiting for its media (the UI shows "analysis in progress").
func llmIsPending(id string) bool {
	llmQueuedMtx.Lock()
	var q = llmQueued[id] || llmRunning == id
	llmQueuedMtx.Unlock()
	if q {
		return true
	}
	llmPendMtx.Lock()
	_, w := llmPend[id]
	llmPendMtx.Unlock()
	return w
}

// llmStats reports (queued, tracked-but-not-ready, dropped) for the status API.
func llmStats() (queued int, waiting int, dropped int64) {
	llmQueuedMtx.Lock()
	queued = len(llmQueued)
	llmQueuedMtx.Unlock()
	llmPendMtx.Lock()
	waiting = len(llmPend)
	llmPendMtx.Unlock()
	return queued, waiting, atomic.LoadInt64(&llmDropped)
}

// llmBackoff pauses automatic analyses after repeated failures (API down,
// bad key): 30s, 1m, 2m ... capped at 15m. Manual requests bypass it.
var llmBackoffMtx sync.Mutex
var llmFailStreak int
var llmFailUntil time.Time

func llmBackoffActive() bool {
	llmBackoffMtx.Lock()
	defer llmBackoffMtx.Unlock()
	return time.Now().Before(llmFailUntil)
}

func llmNoteResult(ok bool) {
	llmBackoffMtx.Lock()
	defer llmBackoffMtx.Unlock()
	if ok {
		llmFailStreak = 0
		llmFailUntil = time.Time{}
		return
	}
	llmFailStreak++
	var wait = (30 * time.Second) << min(llmFailStreak-1, 5)
	if wait > 15*time.Minute {
		wait = 15 * time.Minute
	}
	llmFailUntil = time.Now().Add(wait)
}

// ---------------- analysis ---------------------------------------------------

// llmAnalyzeAndStore runs one analysis and persists the outcome. On failure
// the previous summary (if any) is kept and only the error is recorded.
func llmAnalyzeAndStore(app *App, id string, source string) {
	d, err := getDetection(app.db, id)
	if err != nil || d == nil {
		return
	}
	var t0 = time.Now()
	out, aerr := llmAnalyzeDetection(app, d, source)
	var now = time.Now().Unix()
	if aerr != nil {
		// No API call happened without an image: that says nothing about the
		// API, so it neither starts nor lifts the pause for the other cameras.
		if !errors.Is(aerr, errLLMNoImage) {
			llmNoteResult(false)
		}
		_ = dbSetLLMError(app.db, id, source, aerr.Error(), now)
		logIt(LWARN, "llm analysis failed", map[string]string{"id": id, "error": truncateForLog(aerr.Error(), 300)})
		broadcastLLM(d, false)
		return
	}
	llmNoteResult(true)
	out.ms = float64(time.Since(t0).Microseconds()) / 1000.0
	if err := dbSetLLMResult(app.db, id, out, now); err != nil {
		logIt(LWARN, "llm store failed", map[string]string{"id": id, "error": err.Error()})
		return
	}
	logIt(LINFO, "llm analyzed", map[string]string{"id": id, "model": out.model, "source": out.source, "images": strconv.Itoa(out.images), "ms": strconv.FormatFloat(out.ms, 'f', 0, 64)})
	if det, derr := getDetection(app.db, id); derr == nil && det != nil {
		mqttPublishDetection(det)
		broadcastLLM(det, true)
	}
}

// broadcastLLM tells WebUI clients that a detection's LLM result changed.
func broadcastLLM(d *Detection, ok bool) {
	var body = "{" + kv("camera_id", d.cameraID) + "," + kv("camera", d.camName) + "," +
		kv("type", d.detType) + "," + kv("detection_id", d.id) + "," + kv("kind", "llm") + "," +
		kvRaw("ok", boolJSON(ok)) + "}"
	hub.broadcast(body)
}

// errLLMNoImage reports a detection with nothing to send to the model.
var errLLMNoImage = errors.New("no image to analyze (snapshot/clip missing)")

// llmAnalyzeDetection gathers the images for source and asks the model.
func llmAnalyzeDetection(app *App, d *Detection, source string) (*llmOutcome, error) {
	var cfg = app.cfg.LLM
	images, used := llmCollectImages(app, d, source, cfg.MaxImages, cfg.ImageMaxPx)
	if len(images) == 0 {
		return nil, errLLMNoImage
	}
	var system = llmSystemPrompt(cfg)
	var question = llmContextText(d, used, len(images))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutS)*time.Second)
	defer cancel()
	text, model, err := llmCallOpenAI(ctx, cfg, system, images, question)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty answer from model")
	}
	if len(text) > 4000 {
		text = truncateForLog(text, 4000)
	}
	return &llmOutcome{summary: text, model: model, source: used, images: len(images)}, nil
}

// llmCollectImages returns the images for source (snapshots, video or both)
// and the source actually used: video falls back to snapshots when the
// detection has no usable clip, and vice versa. With both, the budget is
// split evenly and whatever one side cannot fill goes to the other.
func llmCollectImages(app *App, d *Detection, source string, maxImages int, maxPx int) ([]llmImage, string) {
	var snaps, frames []llmImage
	switch source {
	case "video":
		frames = llmClipImages(app, d, maxImages, maxPx)
		if len(frames) == 0 {
			snaps = llmPick(llmSnapshotImages(app, d, maxPx), maxImages)
		}
	case "both":
		snaps = llmSnapshotImages(app, d, maxPx)
		frames = llmClipImages(app, d, maxImages-min(len(snaps), (maxImages+1)/2), maxPx)
		snaps = llmPick(snaps, maxImages-len(frames))
	default:
		snaps = llmPick(llmSnapshotImages(app, d, maxPx), maxImages)
		if len(snaps) == 0 {
			frames = llmClipImages(app, d, maxImages, maxPx)
		}
	}
	var used = "snapshots"
	switch {
	case len(snaps) > 0 && len(frames) > 0:
		used = "both"
	case len(frames) > 0:
		used = "video"
	}
	return append(snaps, frames...), used
}

// llmPick keeps at most n items spread evenly over the list (first/last kept).
func llmPick(list []llmImage, n int) []llmImage {
	if n <= 0 {
		return nil
	}
	if len(list) <= n {
		return list
	}
	var out []llmImage
	for _, i := range evenlySpaced(len(list), n) {
		out = append(out, list[i])
	}
	return out
}

// llmSnapshotImages loads the detection's real snapshots (primary first, then
// the timed burst), skipping missing files and the NO SIGNAL placeholder.
func llmSnapshotImages(app *App, d *Detection, maxPx int) []llmImage {
	var dataDir = app.cfg.Server.DataDir
	var paths []string
	if d.snapshotPath != "" {
		paths = append(paths, d.snapshotPath)
	}
	for _, p := range strings.Split(d.extraSnap, ",") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	var placeholder = placeholderJpg()
	var out []llmImage
	for i, p := range paths {
		if dataDir != "" && !pathInsideDir(dataDir, p) {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 || len(raw) > 20<<20 || bytes.Equal(raw, placeholder) {
			continue
		}
		var jb = compressSnapshotForPush(raw, maxPx, 80)
		if jb == nil {
			continue
		}
		var label = "Snapshot " + strconv.Itoa(len(out)+1) + " (taken at the trigger)"
		if i > 0 {
			label = "Snapshot " + strconv.Itoa(len(out)+1) + " (taken later during the event)"
		}
		out = append(out, llmImage{label: label, jpeg: jb})
	}
	return out
}

// placeholderJpg caches the NO SIGNAL placeholder bytes (to skip such files).
var placeholderJpg = sync.OnceValue(snapshotJpg)

// llmClipImages decodes up to n keyframes spread over the detection's clip.
func llmClipImages(app *App, d *Detection, n int, maxPx int) []llmImage {
	if n <= 0 || d.clipPath == "" {
		return nil
	}
	if dataDir := app.cfg.Server.DataDir; dataDir != "" && !pathInsideDir(dataDir, d.clipPath) {
		return nil
	}
	frames, err := extractClipKeyframes(d.clipPath, n, maxPx)
	if err != nil {
		logIt(LDEBUG, "llm clip keyframes failed", map[string]string{"id": d.id, "error": err.Error()})
		return nil
	}
	var out []llmImage
	for i, f := range frames {
		out = append(out, llmImage{
			label: fmt.Sprintf("Video frame %d of %d (%.1f s into the clip)", i+1, len(frames), f.at.Seconds()),
			jpeg:  f.jpeg,
		})
	}
	return out
}

// llmDefaultPrompt is the built-in instruction (llm.prompt replaces it).
const llmDefaultPrompt = `You are the analysis module of a home security camera system (NVR). You receive images from one detection event on one camera: still snapshots taken when the camera triggered and/or keyframes sampled in order from the recorded clip.

Describe what happens in one to three short sentences: who or what is present (people, vehicles, animals, packages), what they are doing and where they are going, and anything that looks unusual or security-relevant (someone at a door or window, handling objects, staying a long time, activity at night). State only what is visible in the images. If nothing of interest is visible (empty scene, lighting change, moving foliage, insects, rain), say so plainly in one sentence. Do not comment on image quality or on the camera itself.

Answer in plain text, without markdown, headings or preamble.`

// llmSystemPrompt returns the instructions sent as the system prompt.
func llmSystemPrompt(cfg LLMCfg) string {
	var p = strings.TrimSpace(cfg.Prompt)
	if p == "" {
		p = llmDefaultPrompt
	}
	if lang := strings.TrimSpace(cfg.Language); lang != "" {
		p += "\n\nWrite your answer in " + lang + "."
	}
	return p
}

// llmContextText is the per-detection text that follows the images.
func llmContextText(d *Detection, used string, n int) string {
	var what = "snapshots"
	switch used {
	case "video":
		what = "keyframes from the recorded clip"
	case "both":
		what = "snapshots and keyframes from the recorded clip"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Camera: %s\n", d.camName)
	fmt.Fprintf(&b, "Local time: %s\n", time.Unix(d.startedAt, 0).Local().Format("Monday 2 January 2006 15:04"))
	fmt.Fprintf(&b, "Camera detection tags: %s\n", strings.Join(detectionTags(d), ", "))
	if dur := d.endedAt - d.startedAt; dur > 0 {
		fmt.Fprintf(&b, "Event duration: about %d s\n", dur)
	}
	fmt.Fprintf(&b, "Images: %d %s, in chronological order.\n\n", n, what)
	b.WriteString("Describe this detection event.")
	return b.String()
}

// ---------------- API call: OpenAI-compatible (Ollama, LM Studio...) --------

// llmCallOpenAI posts a chat completion with inline data-URL images to an
// OpenAI-compatible server (POST {url}/chat/completions).
func llmCallOpenAI(ctx context.Context, cfg LLMCfg, system string, images []llmImage, question string) (string, string, error) {
	if cfg.URL == "" {
		return "", "", fmt.Errorf("llm.url is empty (e.g. http://127.0.0.1:11434/v1)")
	}
	if cfg.Model == "" {
		return "", "", fmt.Errorf("llm.model is empty")
	}
	type part map[string]any
	var content []part
	for _, im := range images {
		content = append(content,
			part{"type": "text", "text": im.label},
			part{"type": "image_url", "image_url": map[string]string{"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(im.jpeg)}},
		)
	}
	content = append(content, part{"type": "text", "text": question})
	var body = map[string]any{
		"model":      cfg.Model,
		"max_tokens": cfg.MaxTokens,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": content},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	var endpoint = cfg.URL
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if k := strings.TrimSpace(cfg.APIKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", "", fmt.Errorf("llm api timeout after %ds", cfg.TimeoutS)
		}
		return "", "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode != 200 {
		var msg = openAIErrorMessage(out.Error)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return "", "", fmt.Errorf("llm api %d: %s", resp.StatusCode, msg)
	}
	if len(out.Choices) == 0 {
		if msg := openAIErrorMessage(out.Error); msg != "" {
			return "", "", fmt.Errorf("llm api: %s", msg)
		}
		return "", "", fmt.Errorf("llm api: no choices in response")
	}
	var model = out.Model
	if model == "" {
		model = cfg.Model
	}
	return openAIContentText(out.Choices[0].Message.Content), model, nil
}

// openAIErrorMessage reads {"error":"..."} or {"error":{"message":"..."}}.
func openAIErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Message
	}
	return ""
}

// openAIContentText accepts a string content or an array of text parts.
func openAIContentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b []string
		for _, p := range parts {
			if p.Text != "" {
				b = append(b, p.Text)
			}
		}
		return strings.Join(b, "\n")
	}
	return ""
}

// ---------------- storage ----------------------------------------------------

func dbSetLLMResult(store *DB, id string, o *llmOutcome, at int64) error {
	_, err := store.db.Exec("UPDATE detections SET llm_summary=?, llm_model=?, llm_source=?, llm_error='', llm_at=?, llm_ms=?, llm_images=? WHERE id=?",
		o.summary, o.model, o.source, at, o.ms, o.images, id)
	return err
}

// dbSetLLMError records a failure without wiping a previous good summary.
func dbSetLLMError(store *DB, id string, source string, msg string, at int64) error {
	_, err := store.db.Exec("UPDATE detections SET llm_error=?, llm_at=?, llm_source=CASE WHEN COALESCE(llm_summary,'')='' THEN ? ELSE llm_source END WHERE id=?",
		truncateForLog(msg, 500), at, source, id)
	return err
}

// llmJSON renders the LLM result of a detection (null when never analyzed).
func llmJSON(d *Detection) string {
	if d == nil || (d.llmAt == 0 && d.llmSummary == "" && d.llmError == "") {
		return `"llm":null`
	}
	return `"llm":{` + kv("summary", d.llmSummary) + "," + kv("model", d.llmModel) + "," +
		kv("source", d.llmSource) + "," + kv("error", d.llmError) + "," +
		`"at":` + strconv.FormatInt(d.llmAt, 10) + "," +
		`"ms":` + strconv.FormatFloat(d.llmMs, 'f', 0, 64) + "," +
		`"images":` + strconv.Itoa(d.llmImages) + "}"
}

// llmStatusJSON describes the LLM integration for /api/system/status (the
// API key is never exposed).
func llmStatusJSON(app *App) string {
	var c = app.cfg.LLM
	q, w, dropped := llmStats()
	return `"llm":{` + kvRaw("enabled", boolJSON(llmEnabled())) + "," +
		kv("model", c.Model) + "," + kv("source", c.Source) + "," + kvRaw("auto", boolJSON(c.Auto)) + "," +
		`"queued":` + strconv.Itoa(q) + `,"waiting":` + strconv.Itoa(w) + `,"dropped":` + strconv.FormatInt(dropped, 10) + "," +
		kvRaw("backoff", boolJSON(llmBackoffActive())) + "}"
}
