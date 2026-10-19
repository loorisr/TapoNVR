package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// dbSetVisionError is a test-only helper (no production callers): records a
// failed legacy single-row analysis summary.
func dbSetVisionError(store *DB, id string, msg string) error {
	if store == nil {
		return fmt.Errorf("nil store")
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	_, e := store.db.Exec("UPDATE detections SET vision_error=?, vision_at=? WHERE id=?", msg, time.Now().Unix(), id)
	return e
}

func testVisionApp(target string, apiKey string) *App {
	var cfg Config
	configDefaults(&cfg)
	cfg.Vision.Enabled = true
	cfg.Vision.URL = target
	cfg.Vision.APIKey = apiKey
	cfg.Vision.TimeoutS = 5
	cfg.Vision.MinConf = 0.25
	var app = &App{cfg: &cfg, db: nil}
	initVision(&cfg.Vision, true)
	return app
}

func TestVisionAnalyzeOK(t *testing.T) {
	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/detect" {
			t.Errorf("bad path %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("bad content-type %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"yolo26n","width":640,"height":480,"inference_ms":23.4,"request_id":"abc","detections":[{"class_id":0,"label":"person","confidence":0.87,"box":{"x_min":10,"y_min":20,"x_max":100,"y_max":200},"box_norm":{"x_min":0.01,"y_min":0.04,"x_max":0.15,"y_max":0.41}}]}`))
	}))
	defer srv.Close()
	var app = testVisionApp(srv.URL, "")
	res, err := visionAnalyzeBytes(app, []byte{0xFF, 0xD8, 0xFF, 0x00})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(res.Detections) != 1 || res.Detections[0].Label != "person" {
		t.Fatalf("bad detections: %+v", res)
	}
	if res.TotalMs <= 0 || res.TotalMs > 5000 {
		t.Fatalf("bad client total ms: %v", res.TotalMs)
	}
	if top := visionTopLabel(res.Detections); top != "person:0.87" {
		t.Fatalf("bad top %q", top)
	}
}

func TestVisionAnalyzeAPIKeyAndErrors(t *testing.T) {
	var gotKey string
	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		if strings.Contains(r.URL.RawQuery, "classes=") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"unknown class \"nope\""}`))
			return
		}
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"model not ready"}`))
	}))
	defer srv.Close()
	var app = testVisionApp(srv.URL, "secret")
	app.cfg.Vision.Classes = "nope"
	_, err := visionAnalyzeBytes(app, []byte{0x01})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400, got %v", err)
	}
	if gotKey != "secret" {
		t.Fatalf("api key not forwarded: %q", gotKey)
	}
	app.cfg.Vision.Classes = ""
	_, err = visionAnalyzeBytes(app, []byte{0x01})
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("expected not ready, got %v", err)
	}
}

func TestVisionStoreRoundtrip(t *testing.T) {
	var dir = t.TempDir()
	store, err := openDB(dir + "/t.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	var d = &Detection{id: "v1", cameraID: "cam0", camName: "c", detType: "motion", confidence: 0.9, startedAt: 1, endedAt: 2}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	var res = &visionResult{Model: "yolo26n", InferenceMs: 12.5, Detections: []visionDetection{
		{ClassID: 0, Label: "person", Confidence: 0.9},
		{ClassID: 2, Label: "car", Confidence: 0.5},
	}}
	if err := dbSetVision(store, "v1", res, "/snap.jpg", 0, 1234); err != nil {
		t.Fatal(err)
	}
	got, err := getDetection(store, "v1")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.visionCount != 2 || got.visionTop != "person:0.90" || got.visionAt != 1234 {
		t.Fatalf("bad vision row: %+v", got)
	}
	if err := dbSetVisionError(store, "v1", "timeout"); err != nil {
		t.Fatal(err)
	}
	got2, _ := getDetection(store, "v1")
	if got2.visionError != "timeout" {
		t.Fatalf("bad vision_error: %+v", got2)
	}
	// vision filter
	lst, err := listDetectionsFiltered(store, 0, 0, "", "", "person", true, 500, 0)
	if err != nil || len(lst) != 1 {
		t.Fatalf("filtered: %v %d", err, len(lst))
	}
	lst2, err := listDetectionsFiltered(store, 0, 0, "", "", "", true, 500, 0)
	if err != nil || len(lst2) != 1 {
		t.Fatalf("has_vision: %v %d", err, len(lst2))
	}
	_ = os.Remove(dir + "/t.sqlite")
}

func TestVisionDisabledNoCall(t *testing.T) {
	var cfg Config
	configDefaults(&cfg)
	cfg.Vision.Enabled = false
	var app = &App{cfg: &cfg}
	initVision(&cfg.Vision, false)
	if visionShouldAnalyze(app) {
		t.Fatal("disabled vision should not analyze")
	}
	// semaphore released path
	cfg.Vision.Enabled = true
	initVision(&cfg.Vision, true)
	sem, ok := visionAcquire()
	if !ok {
		t.Fatal("acquire should succeed")
	}
	visionRelease(sem)
	sem2, ok2 := visionAcquire()
	if !ok2 {
		t.Fatal("re-acquire should succeed")
	}
	// Releasing against a stale channel must not corrupt the current one.
	visionRelease(sem2)
	visionRelease(sem2)
}

func testVisionJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var img = image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{30, 60, 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVisionDrawBoxes(t *testing.T) {
	var raw = testVisionJPEG(t, 640, 480)
	var dets = []visionDetection{
		{ClassID: 0, Label: "person", Confidence: 0.87,
			BoxNorm: visionBox{XMin: 0.1, YMin: 0.1, XMax: 0.4, YMax: 0.8}},
		{ClassID: 2, Label: "car", Confidence: 0.5,
			Box: visionBox{XMin: 400, YMin: 300, XMax: 600, YMax: 450}},
		{ClassID: 9, Label: "bogus", Confidence: 0.1,
			Box: visionBox{XMin: 10, YMin: 10, XMax: 5, YMax: 5}}, // degenerate: skipped
	}
	out, err := drawVisionBoxes(raw, dets)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	dec, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output not a valid image: %v", err)
	}
	if dec.Bounds().Dx() != 640 || dec.Bounds().Dy() != 480 {
		t.Fatalf("bad size: %v", dec.Bounds())
	}
	// pixel on the person box border (norm 0.1*640=64, 0.1*480=48) must be
	// near the label color, not the background (tolerance: JPEG ringing).
	var want = visionLabelColor("person")
	var got = dec.At(65, 50)
	r1, g1, b1, _ := want.RGBA()
	r2, g2, b2, _ := got.RGBA()
	var diff = func(a, b uint32) uint32 {
		if a > b {
			return a - b
		}
		return b - a
	}
	// 8-bit values are RGBA()>>8; tolerance 60/255 per channel.
	if diff(r1, r2) > 60*257 || diff(g1, g2) > 60*257 || diff(b1, b2) > 60*257 {
		t.Fatalf("box border not drawn: got %v want %v", got, want)
	}
	// empty dets -> error (caller serves the plain file instead)
	if _, err := drawVisionBoxes(raw, nil); err == nil {
		t.Fatal("expected error for no detections")
	}
	// rect resolution: norm wins, pixel box clamped
	if x1, y1, x2, y2, ok := visionDetRect(dets[0], 640, 480); !ok || x1 != 64 || y1 != 48 || x2 != 256 || y2 != 384 {
		t.Fatalf("bad norm rect: %d,%d,%d,%d ok=%v", x1, y1, x2, y2, ok)
	}
}

func TestVisionPerSnap(t *testing.T) {
	var dir = t.TempDir()
	store, err := openDB(dir + "/t.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	var d = &Detection{id: "p1", cameraID: "cam0", camName: "c", detType: "motion", confidence: 0.9, startedAt: 1, endedAt: 2}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	// primary finds 1 object, extra finds 3: summary must keep the best.
	var r0 = &visionResult{Model: "yolo26n", InferenceMs: 20, TotalMs: 100, Detections: []visionDetection{
		{ClassID: 0, Label: "person", Confidence: 0.8},
	}}
	var r1 = &visionResult{Model: "yolo26n", InferenceMs: 30, TotalMs: 120, Detections: []visionDetection{
		{ClassID: 0, Label: "person", Confidence: 0.9},
		{ClassID: 2, Label: "car", Confidence: 0.7},
		{ClassID: 16, Label: "dog", Confidence: 0.6},
	}}
	if err := dbSetVision(store, "p1", r1, "/e1.jpg", 1, 100); err != nil {
		t.Fatal(err)
	}
	if err := dbSetVision(store, "p1", r0, "/p.jpg", 0, 200); err != nil {
		t.Fatal(err)
	}
	got, err := getDetection(store, "p1")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	// best wins regardless of order: count 3 from the extra.
	if got.visionCount != 3 || got.visionTop != "person:0.90" {
		t.Fatalf("bad summary: %+v", got)
	}
	// per-file lookup drives ?boxes=1.
	vr, err := dbGetVisionResult(store, "p1", "/p.jpg")
	if err != nil || vr == nil || vr.count != 1 || vr.snapIndex != 0 || vr.totalMs != 100 {
		t.Fatalf("bad primary row: %+v %v", vr, err)
	}
	vr, err = dbGetVisionResult(store, "p1", "/missing.jpg")
	if err != nil || vr != nil {
		t.Fatalf("expected nil for unknown file: %+v %v", vr, err)
	}
	rows, err := dbListVisionResults(store, "p1")
	if err != nil || len(rows) != 2 || rows[0].snapIndex != 0 || rows[1].snapIndex != 1 {
		t.Fatalf("bad list: %+v %v", rows, err)
	}
	// failure on one file keeps the best summary, records the error per-file.
	if err := dbSetVisionSnapError(store, "p1", 0, "/p.jpg", "timeout"); err != nil {
		t.Fatal(err)
	}
	got2, _ := getDetection(store, "p1")
	if got2.visionCount != 3 {
		t.Fatalf("error overwrote best summary: %+v", got2)
	}
	vr, _ = dbGetVisionResult(store, "p1", "/p.jpg")
	// An error must not wipe a previous success for the same snapshot: the
	// boxes stay, the error is recorded alongside.
	if vr == nil || vr.err != "timeout" || vr.count != 1 {
		t.Fatalf("bad error row: %+v", vr)
	}
	// delete purges per-snapshot rows.
	if err := dbDeleteVisionResults(store, "p1"); err != nil {
		t.Fatal(err)
	}
	rows, _ = dbListVisionResults(store, "p1")
	if len(rows) != 0 {
		t.Fatalf("rows not purged: %d", len(rows))
	}
	_ = os.Remove(dir + "/t.sqlite")
}
