package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------- vision: YOLO snapshot analysis via external API --------
// Snapshot in, detections out. External NVR Vision API:
//   POST {url}/v1/detect?min_conf=0.25&classes=...  (image/jpeg raw bytes)
//   -> {model,width,height,inference_ms,request_id,detections[]}
// Disabled by default; async only (never blocks ONVIF webhook / clip path).

type visionBox struct {
	XMin float64 `json:"x_min"`
	YMin float64 `json:"y_min"`
	XMax float64 `json:"x_max"`
	YMax float64 `json:"y_max"`
}

type visionDetection struct {
	ClassID    int       `json:"class_id"`
	Label      string    `json:"label"`
	Confidence float64   `json:"confidence"`
	Box        visionBox `json:"box"`
	BoxNorm    visionBox `json:"box_norm"`
}

type visionResult struct {
	Model       string            `json:"model"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	InferenceMs float64           `json:"inference_ms"`
	RequestID   string            `json:"request_id"`
	Detections  []visionDetection `json:"detections"`
	// TotalMs is client-side: full POST /v1/detect round-trip (network +
	// server decode/letterbox/ORT/NMS). Set by visionAnalyzeBytes, not by
	// the server. Compare with InferenceMs (Session.Run only) to see the
	// pre/post + network overhead.
	TotalMs float64 `json:"-"`
}

type visionReady struct {
	Ready bool   `json:"ready"`
	Model string `json:"model"`
}

// visionMtx guards the runtime on/off switch (mirrors detection/recording).
var visionMtx sync.Mutex
var visionOn = false

func visionEnabled() bool {
	visionMtx.Lock()
	v := visionOn
	visionMtx.Unlock()
	return v
}

func setVisionEnabled(b bool) {
	visionMtx.Lock()
	visionOn = b
	visionMtx.Unlock()
	mqttPublishState()
}

// visionSem bounds concurrent /v1/detect calls so the ORT pool is never
// saturated by snapshot bursts. Initialized lazily from config.
var visionSemMtx sync.Mutex
var visionSem chan struct{}

// visionAcquire takes one worker slot and reports whether analysis may
// proceed. It returns the semaphore the slot was taken from: releasing against
// visionSem directly would be wrong if initVision replaced the channel in
// between (it would pop another worker's token, admitting more than
// MaxWorkers concurrent calls, or silently drop this one).
func visionAcquire() (chan struct{}, bool) {
	visionSemMtx.Lock()
	s := visionSem
	visionMtx.Lock()
	on := visionOn
	visionMtx.Unlock()
	if s == nil {
		// default before init (main.go sets it right after loadConfig)
		s = make(chan struct{}, 2)
		visionSem = s
	}
	visionSemMtx.Unlock()
	if !on {
		return s, false
	}
	select {
	case s <- struct{}{}:
		return s, true
	default:
		return s, false
	}
}

// visionRelease returns a worker slot to the semaphore it came from.
func visionRelease(s chan struct{}) {
	if s == nil {
		return
	}
	select {
	case <-s:
	default:
	}
}

func initVision(cfg *VisionCfg, enabled bool) {
	n := cfg.MaxWorkers
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	visionSemMtx.Lock()
	visionSem = make(chan struct{}, n)
	visionSemMtx.Unlock()
	visionMtx.Lock()
	visionOn = enabled && cfg.Enabled
	visionMtx.Unlock()
	visionStartWorkers(n)
}

// visionJob is one queued snapshot analysis.
type visionJob struct {
	app       *App
	detID     string
	snapPath  string
	snapIndex int
}

// visionQueue is the bounded analysis backlog: bursts enqueue instead of
// failing with "busy (max workers)". Full queue still degrades (busy error),
// but only under sustained overload, not on every snapshot burst.
var visionQueueMtx sync.Mutex
var visionQueue chan visionJob
var visionQueueGen int

// visionDropped counts analyses dropped under sustained overload (queue full
// or workers busy). Atomic; reported via visionStats for the status API.
var visionDropped int64

// visionStats reports (pending queue length, total dropped, worker slots,
// enabled) for the status API. Queue length is read under the queue lock.
func visionStats() (queueLen int, dropped int64, workers int, enabled bool) {
	visionQueueMtx.Lock()
	q := visionQueue
	if q != nil {
		queueLen = len(q)
	}
	visionQueueMtx.Unlock()
	dropped = atomic.LoadInt64(&visionDropped)
	visionSemMtx.Lock()
	if visionSem != nil {
		workers = cap(visionSem)
	}
	visionSemMtx.Unlock()
	enabled = visionEnabled()
	return queueLen, dropped, workers, enabled
}

func visionStartWorkers(n int) {
	visionQueueMtx.Lock()
	visionQueueGen++
	gen := visionQueueGen
	// Drain + close the previous generation's queue so its workers exit.
	if old := visionQueue; old != nil {
		close(old)
	}
	q := make(chan visionJob, 128)
	visionQueue = q
	visionQueueMtx.Unlock()
	for i := 0; i < n; i++ {
		go func() {
			for job := range q {
				visionQueueMtx.Lock()
				cur := visionQueueGen
				visionQueueMtx.Unlock()
				if cur != gen {
					return
				}
				analyzeAndStoreSnapshot(job.app, job.detID, job.snapPath, job.snapIndex)
			}
		}()
	}
}

// visionEnqueue adds one analysis to the backlog (non-blocking): bursts wait
// their turn on the workers instead of failing immediately.
func visionEnqueue(app *App, detID string, snapPath string, snapIndex int) {
	visionQueueMtx.Lock()
	q := visionQueue
	visionQueueMtx.Unlock()
	if q == nil {
		go analyzeAndStoreSnapshot(app, detID, snapPath, snapIndex)
		return
	}
	defer func() {
		// initVision closes the previous generation's queue: a concurrent
		// re-init must degrade to a busy mark, never panic on send.
		if recover() != nil {
			atomic.AddInt64(&visionDropped, 1)
			_ = dbSetVisionSnapError(app.db, detID, snapIndex, snapPath, "busy (max workers)")
		}
	}()
	select {
	case q <- visionJob{app: app, detID: detID, snapPath: snapPath, snapIndex: snapIndex}:
	default:
		// Sustained overload only: queue full after 128 pending analyses.
		atomic.AddInt64(&visionDropped, 1)
		_ = dbSetVisionSnapError(app.db, detID, snapIndex, snapPath, "busy (max workers)")
	}
}

// visionShouldAnalyze reports whether a snapshot should be sent to YOLO.
func visionShouldAnalyze(app *App) bool {
	if app == nil || app.cfg == nil {
		return false
	}
	if !visionEnabled() {
		return false
	}
	if strings.TrimSpace(app.cfg.Vision.URL) == "" {
		return false
	}
	// Circuit breaker: after repeated failures (sidecar down, DNS/network
	// error) further attempts are paused with exponential backoff instead of
	// logging a warning for every queued snapshot. An explicit re-analyze
	// (POST /vision) bypasses this on purpose.
	return !visionBackoffActive()
}

// visionBackoffMtx guards the vision failure circuit breaker.
var visionBackoffMtx sync.Mutex
var visionFailStreak int
var visionFailUntil time.Time

const visionBackoffBase = 15 * time.Second
const visionBackoffMax = 5 * time.Minute

// visionBackoffActive reports whether yet another analysis attempt should be
// skipped because recent ones failed.
func visionBackoffActive() bool {
	visionBackoffMtx.Lock()
	defer visionBackoffMtx.Unlock()
	return time.Now().Before(visionFailUntil)
}

// visionNoteFailure extends the pause after a failed analysis: 15s, 30s, 1m,
// 2m, 4m, capped at 5m. The first failure of an outage logs a WARN; later ones
// stay at DEBUG so a dead sidecar cannot flood the log.
func visionNoteFailure() {
	visionBackoffMtx.Lock()
	visionFailStreak++
	var wait = visionBackoffBase << min(visionFailStreak-1, 5)
	if wait > visionBackoffMax {
		wait = visionBackoffMax
	}
	visionFailUntil = time.Now().Add(wait)
	var first = visionFailStreak == 1
	visionBackoffMtx.Unlock()
	if first {
		logIt(LWARN, "vision api unreachable; analysis paused with backoff", map[string]string{"retry_in": wait.String()})
	} else {
		logIt(LDEBUG, "vision api still unreachable", map[string]string{"retry_in": wait.String()})
	}
}

// visionNoteSuccess clears the outage state and logs the recovery once.
func visionNoteSuccess() {
	visionBackoffMtx.Lock()
	var wasDown = visionFailStreak > 0
	visionFailStreak = 0
	visionFailUntil = time.Time{}
	visionBackoffMtx.Unlock()
	if wasDown {
		logIt(LINFO, "vision api recovered", nil)
	}
}

// visionBackoffStats reports (active, streak, until) for the status API.
func visionBackoffStats() (bool, int, int64) {
	visionBackoffMtx.Lock()
	defer visionBackoffMtx.Unlock()
	var until int64
	if !visionFailUntil.IsZero() {
		until = visionFailUntil.Unix()
	}
	return time.Now().Before(visionFailUntil), visionFailStreak, until
}

// visionAnalyzeBytes POSTs raw JPEG/PNG bytes to /v1/detect.
func visionAnalyzeBytes(app *App, img []byte) (*visionResult, error) {
	if app == nil || app.cfg == nil {
		return nil, fmt.Errorf("no config")
	}
	var vc = app.cfg.Vision
	base := strings.TrimSuffix(strings.TrimSpace(vc.URL), "/")
	if base == "" {
		return nil, fmt.Errorf("vision url empty")
	}
	if len(img) == 0 {
		return nil, fmt.Errorf("empty image")
	}
	var q = url.Values{}
	q.Set("min_conf", strconv.FormatFloat(vc.MinConf, 'f', -1, 64))
	if strings.TrimSpace(vc.Classes) != "" {
		q.Set("classes", strings.TrimSpace(vc.Classes))
	}
	var endpoint = base + "/v1/detect"
	if enc := q.Encode(); enc != "" {
		endpoint += "?" + enc
	}
	var timeout = vc.TimeoutS
	if timeout < 2 {
		timeout = 2
	}
	if timeout > 30 {
		timeout = 30
	}
	var client = &http.Client{Timeout: time.Duration(timeout) * time.Second}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(img))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "image/jpeg")
	if k := strings.TrimSpace(vc.APIKey); k != "" {
		req.Header.Set("X-API-Key", k)
	}
	var t0 = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// Client-side round-trip: network + full server handler
	// (decode/letterbox/ORT/NMS). See TotalMs.
	var totalMs = float64(time.Since(t0).Microseconds()) / 1000.0
	switch resp.StatusCode {
	case 200:
		// ok
	case 503:
		return nil, fmt.Errorf("model not ready")
	case 401:
		return nil, fmt.Errorf("unauthorized (check vision.api_key)")
	case 413:
		return nil, fmt.Errorf("snapshot too large for vision API")
	default:
		// try to surface the server's {error} message
		var e struct {
			Error string `json:"error"`
		}
		if jerr := json.Unmarshal(body, &e); jerr == nil && e.Error != "" {
			return nil, fmt.Errorf("vision api %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("vision api %d", resp.StatusCode)
	}
	var out visionResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("bad vision response: %w", err)
	}
	if out.Detections == nil {
		out.Detections = []visionDetection{}
	}
	if len(out.Detections) > 300 {
		out.Detections = out.Detections[:300]
	}
	out.TotalMs = totalMs
	return &out, nil
}

// visionAnalyzeFile reads a snapshot from disk (size-capped: camera-supplied
// JPEGs are small; anything huge is rejected instead of buffered) and analyzes it.
func visionAnalyzeFile(app *App, path string) (*visionResult, error) {
	if s, err := os.Stat(path); err != nil || s.Size() == 0 {
		return nil, fmt.Errorf("snapshot missing")
	} else if s.Size() > 20<<20 {
		return nil, fmt.Errorf("snapshot too large (%d bytes)", s.Size())
	}
	img, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(img) > 20<<20 {
		return nil, fmt.Errorf("snapshot too large (%d bytes)", len(img))
	}
	return visionAnalyzeBytes(app, img)
}

// visionFilterExcluded drops boxes whose label is in excluded
// (case-insensitive) and returns the filtered JSON array ("[]" when empty).
// Unparseable input is returned unchanged.
func visionFilterExcluded(jsonStr string, excluded map[string]bool) string {
	if len(excluded) == 0 {
		return jsonStr
	}
	var lower = map[string]bool{}
	for k := range excluded {
		lower[strings.ToLower(k)] = true
	}
	var dets []visionDetection
	if err := json.Unmarshal([]byte(jsonStr), &dets); err != nil {
		return jsonStr
	}
	var kept = []visionDetection{}
	for _, d := range dets {
		if lower[strings.ToLower(strings.TrimSpace(d.Label))] {
			continue
		}
		kept = append(kept, d)
	}
	return visionDetsJSON(kept)
}

// visionTopLabel returns "label:conf" of the highest-confidence detection.
func visionTopLabel(dets []visionDetection) string {
	if len(dets) == 0 {
		return ""
	}
	var best = dets[0]
	for _, d := range dets[1:] {
		if d.Confidence > best.Confidence {
			best = d
		}
	}
	return fmt.Sprintf("%s:%.2f", best.Label, best.Confidence)
}

// visionSummaryForJSON returns count + top label for a vision detections JSON array.
func visionSummaryForJSON(raw string) (int, string) {
	dets := parseVisionDets(raw)
	return len(dets), visionTopLabel(dets)
}

// analyzeAndStoreSnapshot runs YOLO on one snapshot file and persists the
// per-snapshot outcome (snapIndex 0 = primary, 1..N = burst extras).
// Degraded-silent on failure: the detection row is kept with its camera-side
// type and the summary keeps the best successful result, if any.
// Runs on a vision worker (see the bounded queue below), never inline.
func analyzeAndStoreSnapshot(app *App, detID string, snapPath string, snapIndex int) {
	if !visionShouldAnalyze(app) {
		return
	}
	sem, ok := visionAcquire()
	if !ok {
		atomic.AddInt64(&visionDropped, 1)
		_ = dbSetVisionSnapError(app.db, detID, snapIndex, snapPath, "busy (max workers)")
		return
	}
	defer visionRelease(sem)
	res, err := visionAnalyzeFile(app, snapPath)
	var now = time.Now().Unix()
	if err != nil {
		visionNoteFailure()
		_ = dbSetVisionSnapError(app.db, detID, snapIndex, snapPath, err.Error())
		logIt(LWARN, "vision analyze failed", map[string]string{"id": detID, "error": err.Error()})
		return
	}
	visionNoteSuccess()
	if err := dbSetVision(app.db, detID, res, snapPath, snapIndex, now); err != nil {
		logIt(LWARN, "vision store failed", map[string]string{"id": detID, "error": err.Error()})
		return
	}
	logIt(LINFO, "vision analyzed", map[string]string{
		"id": detID, "count": strconv.Itoa(len(res.Detections)),
		"top": visionTopLabel(res.Detections), "model": res.Model,
	})
	// Refresh consumers with the enriched row.
	if det, derr := getDetection(app.db, detID); derr == nil && det != nil {
		mqttPublishDetection(det)
		broadcastVision(det.cameraID, det.camName, det.detType, detID, len(res.Detections), visionTopLabel(res.Detections))
	}
}

// queueVisionForDetection fires the async analysis for a detection's
// snapshots: primary always, extras only when vision.snapshots=all.
func queueVisionForDetection(app *App, detID string, primarySnap string) {
	if !visionShouldAnalyze(app) || detID == "" || primarySnap == "" {
		return
	}
	s, err := os.Stat(primarySnap)
	if err != nil || s.Size() == 0 {
		return
	}
	visionEnqueue(app, detID, primarySnap, 0)
}

// queueVisionForExtras fires analysis for burst extras (snapshots=all only).
// extras[i] is snapshot index i+1 (matching /snapshot/{n}).
func queueVisionForExtras(app *App, detID string, extras []string) {
	if !visionShouldAnalyze(app) || detID == "" {
		return
	}
	if strings.ToLower(strings.TrimSpace(app.cfg.Vision.Snapshots)) != "all" {
		return
	}
	for i, p := range extras {
		if p == "" {
			continue
		}
		if s, err := os.Stat(p); err != nil || s.Size() == 0 {
			continue
		}
		visionEnqueue(app, detID, p, i+1)
	}
}

// visionCheckReady probes GET {url}/ready (never sends the API key: /ready
// never requires auth per the vision API doc).
func visionCheckReady(app *App) (bool, string) {
	if app == nil || app.cfg == nil {
		return false, ""
	}
	base := strings.TrimSuffix(strings.TrimSpace(app.cfg.Vision.URL), "/")
	if base == "" {
		return false, ""
	}
	var timeout = app.cfg.Vision.TimeoutS
	if timeout < 2 {
		timeout = 2
	}
	if timeout > 30 {
		timeout = 30
	}
	var client = &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Get(base + "/ready")
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var r visionReady
	if err := json.Unmarshal(body, &r); err != nil {
		return false, ""
	}
	return r.Ready, r.Model
}
