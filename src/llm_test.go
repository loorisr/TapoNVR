package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eyevinn/hi264/pkg/encode"
	"github.com/Eyevinn/hi264/pkg/yuv"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// ---------------- fixtures ----------------------------------------------------

// testIDRAU encodes one flat-color 64x48 IDR access unit (SPS, PPS, slice).
func testIDRAU(t *testing.T, luma uint8) [][]byte {
	t.Helper()
	p := encode.EncodeParams{Width: 64, Height: 48, QP: 26}
	sps, err := encode.GenerateSPS(p)
	if err != nil {
		t.Fatal(err)
	}
	pps, err := encode.GeneratePPS(p)
	if err != nil {
		t.Fatal(err)
	}
	grid, colors := yuv.SolidGrid(64, 48, yuv.Color{Y: luma, Cb: 128, Cr: 128})
	idr, err := encode.GenerateIDR(p, grid, colors, 0)
	if err != nil {
		t.Fatal(err)
	}
	var au h264.AnnexB
	if err := au.Unmarshal(append(append(append([]byte{}, sps...), pps...), idr...)); err != nil {
		t.Fatal(err)
	}
	return au
}

// writeTestClip writes a TS segment of IDR frames (4 per second, luma
// ramping up) and transmuxes it to MP4 like assembleClip does.
func writeTestClip(t *testing.T, dir string, lumas []uint8) (mp4Path, tsPath string) {
	t.Helper()
	tsPath = filepath.Join(dir, "seg.ts")
	first := testIDRAU(t, lumas[0])
	w, err := newTsSegmentWriter(tsPath, first[0], first[1])
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range lumas {
		if err := w.writeH264(testIDRAU(t, l), int64(i)*22500); err != nil {
			t.Fatal(err)
		}
	}
	w.close()
	mp4Path = filepath.Join(dir, "clip.mp4")
	if err := transmuxTsSegmentsToMp4([]string{tsPath}, mp4Path); err != nil {
		t.Fatal(err)
	}
	return mp4Path, tsPath
}

// avgLuma decodes a JPEG and returns the average Y of its center pixel.
func centerLuma(t *testing.T, jb []byte) int {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jb))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Dx()/2, b.Dy()/2).RGBA()
	return int((r + g + bl) / 3 >> 8)
}

func writeTestJPEG(t *testing.T, path string) {
	t.Helper()
	img, ok := decodeH264AUToImage(testIDRAU(t, 120), "test")
	if !ok {
		t.Fatal("decode test frame")
	}
	if err := os.WriteFile(path, encodeJPEGMax(img, 0, 85), 0o640); err != nil {
		t.Fatal(err)
	}
}

// ---------------- keyframes ---------------------------------------------------

func TestEvenlySpaced(t *testing.T) {
	cases := []struct {
		total, n int
		want     string
	}{
		{10, 3, "[0 5 9]"}, {10, 1, "[5]"}, {3, 6, "[0 1 2]"}, {0, 3, "[]"}, {5, 0, "[]"}, {7, 7, "[0 1 2 3 4 5 6]"},
	}
	for _, c := range cases {
		got := evenlySpaced(c.total, c.n)
		if g := fmt.Sprint(got); g != c.want {
			t.Errorf("evenlySpaced(%d,%d) = %s, want %s", c.total, c.n, g, c.want)
		}
	}
}

func TestSplitAVCC(t *testing.T) {
	in := []byte{0, 0, 0, 2, 0xaa, 0xbb, 0, 0, 0, 1, 0xcc, 0, 0, 0, 9}
	got := splitAVCC(in)
	if len(got) != 2 || !bytes.Equal(got[0], []byte{0xaa, 0xbb}) || !bytes.Equal(got[1], []byte{0xcc}) {
		t.Fatalf("splitAVCC = %x", got)
	}
}

func TestExtractClipKeyframesMP4AndTS(t *testing.T) {
	lumas := []uint8{40, 70, 100, 130, 160, 190, 220}
	mp4Path, tsPath := writeTestClip(t, t.TempDir(), lumas)
	for _, p := range []string{mp4Path, tsPath} {
		frames, err := extractClipKeyframes(p, 3, 32)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
		if len(frames) != 3 {
			t.Fatalf("%s: got %d frames, want 3", filepath.Base(p), len(frames))
		}
		// First, middle and last keyframes, in order (luma ramps up).
		var prev = -1
		for i, f := range frames {
			l := centerLuma(t, f.jpeg)
			if l <= prev {
				t.Fatalf("%s: frame %d not in chronological order (luma %d after %d)", filepath.Base(p), i, l, prev)
			}
			prev = l
			img, _ := jpeg.Decode(bytes.NewReader(f.jpeg))
			if b := img.Bounds(); b.Dx() > 32 || b.Dy() > 32 {
				t.Fatalf("frame not downscaled to 32px: %v", b)
			}
		}
		if frames[0].at != 0 || frames[2].at != 1500*time.Millisecond {
			t.Fatalf("%s: unexpected offsets %v .. %v", filepath.Base(p), frames[0].at, frames[2].at)
		}
	}
}

// ---------------- providers -----------------------------------------------------

func TestLLMCallAnthropicRequestShape(t *testing.T) {
	var got map[string]any
	var beta, apiKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		beta = r.Header.Get("anthropic-beta")
		apiKey = r.Header.Get("x-api-key") + r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"A person walks to the front door."}],"stop_reason":"end_turn","stop_details":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":8}}`)
	}))
	defer srv.Close()
	cfg := Config{}
	configDefaults(&cfg)
	cfg.LLM.URL = srv.URL
	cfg.LLM.APIKey = "test-key"
	imgs := []llmImage{{label: "Snapshot 1", jpeg: []byte{0xff, 0xd8, 0xff, 0xd9}}}
	text, model, err := llmCallAnthropic(t.Context(), cfg.LLM, "sys", imgs, "Describe.")
	if err != nil {
		t.Fatal(err)
	}
	if text != "A person walks to the front door." || model != "claude-opus-5-5" {
		t.Fatalf("got %q / %q", text, model)
	}
	if !strings.Contains(apiKey, "test-key") || !strings.Contains(beta, "server-side-fallback-2026-07-01") {
		t.Fatalf("headers: key=%q beta=%q", apiKey, beta)
	}
	if got["model"] != "claude-opus-5-5" || got["fallbacks"] != "default" {
		t.Fatalf("model/fallbacks: %v / %v", got["model"], got["fallbacks"])
	}
	if oc, _ := got["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Fatalf("effort not sent: %v", got["output_config"])
	}
	if _, has := got["thinking"]; has {
		t.Fatal("thinking must be omitted (always on for this model)")
	}
	msgs, _ := got["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 3 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("content blocks: %v", content)
	}
	src := content[1].(map[string]any)["source"].(map[string]any)
	if src["media_type"] != "image/jpeg" || src["type"] != "base64" || src["data"] == "" {
		t.Fatalf("image source: %v", src)
	}
}

func TestLLMCallAnthropicRefusalAndErrors(t *testing.T) {
	var status = 200
	var body = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"x"},"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	cfg := Config{}
	configDefaults(&cfg)
	cfg.LLM.URL = srv.URL
	cfg.LLM.APIKey = "k"
	imgs := []llmImage{{label: "x", jpeg: []byte{1}}}
	if _, _, err := llmCallAnthropic(t.Context(), cfg.LLM, "s", imgs, "q"); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("refusal not reported: %v", err)
	}
	status, body = 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	if _, _, err := llmCallAnthropic(t.Context(), cfg.LLM, "s", imgs, "q"); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("401 not mapped: %v", err)
	}
}

func TestLLMCallOpenAICompatible(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"model":"qwen2.5vl:7b","choices":[{"message":{"role":"assistant","content":"A cat crosses the yard."},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	cfg := LLMCfg{Provider: "openai", URL: srv.URL + "/v1", Model: "qwen2.5vl:7b", MaxTokens: 500, APIKey: "sk-local"}
	text, model, err := llmCallOpenAI(t.Context(), cfg, "sys", []llmImage{{label: "Snapshot 1", jpeg: []byte{0xff, 0xd8}}}, "Describe.")
	if err != nil {
		t.Fatal(err)
	}
	if text != "A cat crosses the yard." || model != "qwen2.5vl:7b" || auth != "Bearer sk-local" {
		t.Fatalf("got %q %q auth=%q", text, model, auth)
	}
	msgs := got["messages"].([]any)
	user := msgs[1].(map[string]any)["content"].([]any)
	img := user[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(img, "data:image/jpeg;base64,") {
		t.Fatalf("image not sent as data URL: %q", img)
	}
	// Error body is surfaced.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"message":"model \"nope\" not found"}}`)
	}))
	defer srv2.Close()
	cfg.URL = srv2.URL
	if _, _, err := llmCallOpenAI(t.Context(), cfg, "s", []llmImage{{jpeg: []byte{1}}}, "q"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error message not surfaced: %v", err)
	}
}

// ---------------- pipeline -------------------------------------------------------

func TestLLMCollectImagesSources(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = dir
	app := &App{cfg: &cfg}
	mp4Path, _ := writeTestClip(t, dir, []uint8{40, 80, 120, 160, 200})
	snap := filepath.Join(dir, "snap.jpg")
	writeTestJPEG(t, snap)
	ph := filepath.Join(dir, "ph.jpg")
	if err := writePlaceholderJpg(ph); err != nil {
		t.Fatal(err)
	}
	d := &Detection{id: "x", snapshotPath: snap, extraSnap: ph, clipPath: mp4Path}
	imgs, used := llmCollectImages(app, d, "snapshots", 6, 256)
	if used != "snapshots" || len(imgs) != 1 {
		t.Fatalf("snapshots: %d images (%s), placeholder must be skipped", len(imgs), used)
	}
	imgs, used = llmCollectImages(app, d, "video", 4, 256)
	if used != "video" || len(imgs) != 4 || !strings.HasPrefix(imgs[0].label, "Video frame 1 of 4") {
		t.Fatalf("video: %d images (%s) %q", len(imgs), used, imgs[0].label)
	}
	imgs, used = llmCollectImages(app, d, "both", 6, 256)
	if used != "both" || len(imgs) != 6 || !strings.HasPrefix(imgs[0].label, "Snapshot") {
		t.Fatalf("both: %d images (%s)", len(imgs), used)
	}
	// Video without a clip falls back to the snapshots.
	d.clipPath = ""
	if imgs, used = llmCollectImages(app, d, "video", 4, 256); used != "snapshots" || len(imgs) != 1 {
		t.Fatalf("video fallback: %d images (%s)", len(imgs), used)
	}
}

func TestLLMTrackWaitsForAllProducers(t *testing.T) {
	store := testDB(t)
	dir := t.TempDir()
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Nothing of interest: the scene is empty."}],"stop_reason":"end_turn","stop_details":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = dir
	cfg.LLM.Enabled, cfg.LLM.URL, cfg.LLM.APIKey = true, srv.URL, "k"
	app := &App{cfg: &cfg, db: store}
	setLLMEnabled(true)
	defer setLLMEnabled(false)
	snap := filepath.Join(dir, "snap.jpg")
	writeTestJPEG(t, snap)
	d := Detection{id: newID(), cameraID: "cam0", camName: "Garden", detType: "person", snapshotPath: snap, startedAt: time.Now().Unix(), endedAt: time.Now().Unix() + 10}
	llmTrack(app, d.id, true, true)
	if err := dbInsertDetection(store, &d); err != nil {
		t.Fatal(err)
	}
	llmNoteReady(app, d.id, llmNeedRow)
	llmNoteReady(app, d.id, llmNeedSnaps)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if calls != 0 {
		mu.Unlock()
		t.Fatal("analysis started before the clip was reported ready")
	}
	mu.Unlock()
	if !llmIsPending(d.id) {
		t.Fatal("tracked detection not reported pending")
	}
	llmNoteReady(app, d.id, llmNeedClip)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := getDetection(store, d.id)
		if got != nil && got.llmSummary != "" {
			if got.llmModel != "claude-opus-5-5" || got.llmSource != "snapshots" || got.llmImages != 1 || got.llmError != "" {
				t.Fatalf("stored result: %+v", got)
			}
			if !strings.Contains(detJSON(got), `"summary":"Nothing of interest`) {
				t.Fatalf("detJSON missing llm summary: %s", detJSON(got))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("llm result never stored")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected exactly 1 API call, got %d", calls)
	}
}

func TestLLMEventsFilterAndConfigClamp(t *testing.T) {
	c := LLMCfg{Provider: "Ollama", Source: "clip", Effort: "bogus", MaxImages: 99, Events: []string{"Person", "nope", "person"}}
	clampLLMConfig(&c)
	if c.Provider != "openai" || c.Source != "video" || c.Effort != "low" || c.MaxImages != 6 || len(c.Events) != 1 || c.Events[0] != "person" {
		t.Fatalf("clamp: %+v", c)
	}
	if llmEventAllowed(c, &Detection{detType: "motion", detTypes: "motion"}) {
		t.Fatal("motion allowed despite events=[person]")
	}
	if !llmEventAllowed(c, &Detection{detType: "person", detTypes: "motion,person"}) {
		t.Fatal("merged person tag not allowed")
	}
	var def Config
	configDefaults(&def)
	if def.LLM.Model != "claude-opus-5-5" || def.LLM.Provider != "anthropic" || def.LLM.Enabled {
		t.Fatalf("defaults: %+v", def.LLM)
	}
}

// ---------------- downloads --------------------------------------------------------

func TestDetectionDownloadZipAndAttachment(t *testing.T) {
	store := testDB(t)
	dir := t.TempDir()
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = dir
	cfg.Server.Auth.Password = ""
	cfg.Server.Auth.Disabled = true
	app := &App{cfg: &cfg, db: store}
	snap := filepath.Join(dir, "a-snap.jpg")
	writeTestJPEG(t, snap)
	clip := filepath.Join(dir, "a-person.mp4")
	if err := os.WriteFile(clip, []byte("not really a video"), 0o640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("secret"), 0o640)
	d := Detection{id: newID(), cameraID: "cam0", camName: "Front door", detType: "person", snapshotPath: snap, clipPath: clip, extraSnap: outside, startedAt: 1700000000, endedAt: 1700000010}
	if err := dbInsertDetection(store, &d); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handle(app, rec, httptest.NewRequest("GET", "/api/detections/"+d.id+"/download", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "Front_door-") {
		t.Fatalf("content-disposition: %q", cd)
	}
	zr, err := zipReader(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "a-snap.jpg,a-person.mp4,detection.json" {
		t.Fatalf("zip entries: %v (files outside data_dir must be skipped)", names)
	}
	// Single file download.
	rec = httptest.NewRecorder()
	handle(app, rec, httptest.NewRequest("GET", "/api/detections/"+d.id+"/clip?download=1", nil))
	if cd := rec.Header().Get("Content-Disposition"); rec.Code != 200 || cd != `attachment; filename=a-person.mp4` {
		t.Fatalf("clip download: %d %q", rec.Code, cd)
	}
	rec = httptest.NewRecorder()
	handle(app, rec, httptest.NewRequest("GET", "/api/detections/"+d.id+"/snapshot", nil))
	if cd := rec.Header().Get("Content-Disposition"); rec.Code != 200 || cd != "" {
		t.Fatalf("inline snapshot must not be an attachment: %d %q", rec.Code, cd)
	}
}

func TestDetectionLLMEndpoint(t *testing.T) {
	store := testDB(t)
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.Auth.Password = ""
	cfg.Server.Auth.Disabled = true
	app := &App{cfg: &cfg, db: store}
	d := Detection{id: newID(), cameraID: "cam0", camName: "c", detType: "motion"}
	if err := dbInsertDetection(store, &d); err != nil {
		t.Fatal(err)
	}
	setLLMEnabled(false)
	rec := httptest.NewRecorder()
	handle(app, rec, httptest.NewRequest("POST", "/api/detections/"+d.id+"/llm", nil))
	if rec.Code != 400 {
		t.Fatalf("disabled llm: got %d want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	handle(app, rec, httptest.NewRequest("GET", "/api/detections/"+d.id+"/llm", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"llm":null`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handle(app, rec, httptest.NewRequest("POST", "/api/detections/nope/llm", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown id: %d", rec.Code)
	}
}

// llmBackoffFixture builds an app whose LLM API always answers 500, plus one
// row with a real snapshot and one with only the NO SIGNAL placeholder.
func llmBackoffFixture(t *testing.T) (app *App, healthyID, offlineID string, calls func() int) {
	t.Helper()
	llmNoteResult(true)
	t.Cleanup(func() { llmNoteResult(true) })
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	store := testDB(t)
	dir := t.TempDir()
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = dir
	cfg.LLM.Provider, cfg.LLM.URL, cfg.LLM.Model = "openai", srv.URL, "test-model"
	app = &App{cfg: &cfg, db: store}
	insert := func(name string, write func(path string) error) string {
		path := filepath.Join(dir, name)
		if err := write(path); err != nil {
			t.Fatal(err)
		}
		d := Detection{id: newID(), cameraID: "cam0", camName: "c", detType: "person", snapshotPath: path}
		if err := dbInsertDetection(store, &d); err != nil {
			t.Fatal(err)
		}
		return d.id
	}
	healthyID = insert("good.jpg", func(p string) error { writeTestJPEG(t, p); return nil })
	offlineID = insert("placeholder.jpg", writePlaceholderJpg)
	return app, healthyID, offlineID, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// A detection without a usable image (camera offline, media deleted) says
// nothing about the API: it must not pause the analyses of the other cameras.
func TestLLMMissingImageDoesNotPauseAnalyses(t *testing.T) {
	app, _, offlineID, calls := llmBackoffFixture(t)
	llmAnalyzeAndStore(app, offlineID, "snapshots")
	if llmBackoffActive() {
		t.Error("a detection without image activated the API failure backoff")
	}
	if calls() != 0 {
		t.Errorf("API called %d times without any image", calls())
	}
	got, err := getDetection(app.db, offlineID)
	if err != nil || got == nil {
		t.Fatalf("row: %v %v", got, err)
	}
	if !strings.Contains(got.llmError, "no image") {
		t.Errorf("failure not recorded on the row: %q", got.llmError)
	}
}

// A failing API still pauses automatic analyses, and a detection without image
// arriving meanwhile must not lift that pause (no API call proves it recovered).
func TestLLMAPIFailureStillPausesAnalyses(t *testing.T) {
	app, healthyID, offlineID, calls := llmBackoffFixture(t)
	llmAnalyzeAndStore(app, healthyID, "snapshots")
	if calls() != 1 {
		t.Fatalf("expected 1 API call, got %d", calls())
	}
	if !llmBackoffActive() {
		t.Fatal("API failure did not activate the backoff")
	}
	llmAnalyzeAndStore(app, offlineID, "snapshots")
	if !llmBackoffActive() {
		t.Error("a detection without image lifted the API failure backoff")
	}
}

// The backup descriptor is meant to be shared: it must not carry the LLM
// credentials, and redacting them must not break the running config.
func TestBackupRedactsLLMCredentials(t *testing.T) {
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.LLM.APIKey = "sk-llm-secret"
	cfg.LLM.URL = "https://proxyuser:proxypass@llm.example/v1"
	app := &App{cfg: &cfg, db: testDB(t)}
	rec := httptest.NewRecorder()
	handleSystemBackup(app, rec, httptest.NewRequest("GET", "/api/system/backup", nil))
	if rec.Code != 200 {
		t.Fatalf("backup: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Config struct {
			LLM struct{ APIKey, URL string }
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Config.LLM.APIKey != "***" {
		t.Errorf("llm api key not redacted: %q", out.Config.LLM.APIKey)
	}
	if out.Config.LLM.URL != "https://***@llm.example/v1" {
		t.Errorf("llm url credentials not redacted: %q", out.Config.LLM.URL)
	}
	if cfg.LLM.APIKey != "sk-llm-secret" || !strings.Contains(cfg.LLM.URL, "proxypass") {
		t.Errorf("backup modified the live config: %+v", cfg.LLM)
	}
}

func zipReader(b []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(b), int64(len(b)))
}

func TestSafeFileStem(t *testing.T) {
	for in, want := range map[string]string{"Porte d'entrée": "Porte_d_entrée", "../x": "___x", "": "camera", "cam-1_a": "cam-1_a"} {
		if got := safeFileStem(in); got != want {
			t.Errorf("safeFileStem(%q) = %q, want %q", in, got, want)
		}
	}
}
