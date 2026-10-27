package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testJPEG writes a small solid-grey JPEG and returns its path.
func testJPEG(t *testing.T, dir, name string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 100, G: 100, B: 100, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// testSharpJPEG writes a high-contrast checkerboard (high Laplacian variance).
func testSharpJPEG(t *testing.T, dir, name string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			var c = color.RGBA{R: 20, G: 20, B: 20, A: 0xff}
			if (x/4+y/4)%2 == 0 {
				c = color.RGBA{R: 235, G: 235, B: 235, A: 0xff}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func box(t string, x1, y1, x2, y2 float64) detBox {
	return detBox{Type: t, X1: x1, Y1: y1, X2: x2, Y2: y2}
}

func TestBestSEIBoxArea(t *testing.T) {
	// Matching types only, normalized area of the largest box.
	boxes := []detBox{
		box("person", 0.10, 0.10, 0.30, 0.30), // 0.04
		box("person", 0.20, 0.20, 0.80, 0.80), // 0.36
		box("vehicle", 0.0, 0.0, 0.9, 0.9),    // 0.81, but wrong type for the tag
	}
	area, rank := bestSEIBoxArea(boxes, []string{"person"})
	if area < 0.359 || area > 0.361 || rank != 1 {
		t.Fatalf("area=%v rank=%d want 0.36/1", area, rank)
	}
	// Oversized boxes (>85%) are discarded as foreground artefacts.
	area, rank = bestSEIBoxArea([]detBox{box("person", 0, 0, 0.95, 0.95)}, []string{"person"})
	if area != 0 || rank != 0 {
		t.Fatalf("oversized box should be ignored, got %v/%d", area, rank)
	}
	// Tiny boxes are noise.
	area, _ = bestSEIBoxArea([]detBox{box("motion", 0.1, 0.1, 0.11, 0.11)}, []string{"motion"})
	if area != 0 {
		t.Fatalf("tiny box should be ignored, got %v", area)
	}
	// Coordinates are clamped to the frame.
	area, _ = bestSEIBoxArea([]detBox{box("person", -0.5, -0.5, 0.5, 0.5)}, []string{"person"})
	if area != 0.25 {
		t.Fatalf("clamped area=%v want 0.25", area)
	}
}

func TestPickBestSnapshotLargestBox(t *testing.T) {
	dir := t.TempDir()
	cands := []snapCandidate{
		{path: testJPEG(t, dir, "a.jpg"), boxes: []detBox{box("person", 0.1, 0.1, 0.3, 0.3)}},
		{path: testJPEG(t, dir, "b.jpg"), boxes: []detBox{box("person", 0.2, 0.2, 0.7, 0.7)}},
		{path: testJPEG(t, dir, "c.jpg"), boxes: []detBox{box("person", 0.1, 0.1, 0.4, 0.4)}},
	}
	got := pickBestSnapshot(cands, []string{"person"})
	if got.path != cands[1].path {
		t.Fatalf("picked %q, want the largest box %q", got.path, cands[1].path)
	}
}

func TestPickBestSnapshotNearTiePrefersSpecificType(t *testing.T) {
	dir := t.TempDir()
	cands := []snapCandidate{
		// Slightly larger motion box...
		{path: testJPEG(t, dir, "motion.jpg"), boxes: []detBox{box("motion", 0.1, 0.1, 0.55, 0.55)}}, // 0.2025
		// ...but the person box is within the tie window and is more specific.
		{path: testJPEG(t, dir, "person.jpg"), boxes: []detBox{box("person", 0.1, 0.1, 0.54, 0.54)}}, // 0.1936
	}
	got := pickBestSnapshot(cands, []string{"motion", "person"})
	if got.path != cands[1].path {
		t.Fatalf("picked %q, want the specific person frame %q", got.path, cands[1].path)
	}
}

func TestPickBestSnapshotSharpnessFallback(t *testing.T) {
	dir := t.TempDir()
	flat := testJPEG(t, dir, "flat.jpg")
	sharp := testSharpJPEG(t, dir, "sharp.jpg")
	got := pickBestSnapshot([]snapCandidate{{path: flat}, {path: sharp}}, []string{"motion"})
	if got.path != sharp {
		t.Fatalf("picked %q, want the sharp frame %q", got.path, sharp)
	}
	// A missing file is ignored.
	got = pickBestSnapshot([]snapCandidate{{path: filepath.Join(dir, "nope.jpg")}, {path: flat}}, nil)
	if got.path != flat {
		t.Fatalf("missing file should be skipped, got %q", got.path)
	}
}

func TestPickBestSnapshotBoxBeatsNoBox(t *testing.T) {
	dir := t.TempDir()
	// A very sharp empty scene must not beat a frame that actually contains a
	// (small) subject box.
	sharpEmpty := testSharpJPEG(t, dir, "empty.jpg")
	withBox := testJPEG(t, dir, "box.jpg")
	got := pickBestSnapshot([]snapCandidate{
		{path: sharpEmpty},
		{path: withBox, boxes: []detBox{box("person", 0.4, 0.4, 0.52, 0.52)}}, // 1.44%
	}, []string{"person"})
	if got.path != withBox {
		t.Fatalf("picked %q, want the frame with the subject box %q", got.path, withBox)
	}
}

func TestPickBestSnapshotAllMissing(t *testing.T) {
	got := pickBestSnapshot([]snapCandidate{{path: "/nonexistent/a.jpg"}}, []string{"person"})
	if got.path != "" {
		t.Fatalf("expected no pick, got %q", got.path)
	}
}

func TestPickBestSnapshotSkipsPlaceholder(t *testing.T) {
	dir := t.TempDir()
	placeholder := testJPEG(t, dir, "placeholder.jpg")
	real := testJPEG(t, dir, "real.jpg")
	got := pickBestSnapshot([]snapCandidate{
		{path: placeholder, placeholder: true, boxes: []detBox{box("person", 0.1, 0.1, 0.8, 0.8)}},
		{path: real, boxes: nil},
	}, []string{"person"})
	if got.path != real {
		t.Fatalf("placeholder must not win over a real frame, got %q", got.path)
	}
}

func TestDetSnapSetWaitAndSnapshot(t *testing.T) {
	set := newDetSnapSet()
	set.add(snapCandidate{path: "one"})
	start := time.Now()
	set.wait(20 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("wait returned too early (%v)", elapsed)
	}
	if got := set.snapshot(); len(got) != 1 || got[0].path != "one" {
		t.Fatalf("snapshot=%v", got)
	}
	set.add(snapCandidate{path: "two"})
	set.finish()
	start = time.Now()
	set.wait(2 * time.Second)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wait should return immediately after finish (%v)", elapsed)
	}
	if got := set.snapshot(); len(got) != 2 {
		t.Fatalf("snapshot after finish=%v", got)
	}
	// Copies are independent from the internal slice.
	got := set.snapshot()
	got[0].path = "mutated"
	if set.snapshot()[0].path != "one" {
		t.Fatal("snapshot must return a copy")
	}
}

func TestLatestSEIBoxes(t *testing.T) {
	var cfg Config
	configDefaults(&cfg)
	cfg.Cameras = []CameraCfg{{Name: "cam"}}
	var app = &App{cfg: &cfg}
	var d = newRTSPDetector(app, &cfg.Cameras[0], "cam0")
	registerRTSPDetector(d)
	defer unregisterRTSPDetector("cam0")
	d.mu.Lock()
	d.lastBoxes = []detBox{box("person", 0.1, 0.1, 0.5, 0.5)}
	d.lastBoxesAt = time.Now()
	d.mu.Unlock()
	boxes, at, ok := latestSEIBoxes("cam0", time.Second)
	if !ok || len(boxes) != 1 || boxes[0].Type != "person" || at.IsZero() {
		t.Fatalf("latestSEIBoxes=%v %v %v", boxes, at, ok)
	}
	if _, _, ok := latestSEIBoxes("cam0", time.Nanosecond); ok {
		t.Fatal("stale boxes must be rejected")
	}
	if _, _, ok := latestSEIBoxes("nope", time.Second); ok {
		t.Fatal("unknown camera must yield no boxes")
	}
}

func TestPushoverSnapshotPickConfig(t *testing.T) {
	var c Config
	configDefaults(&c)
	if c.Alerts.Pushover.SnapshotPick != "primary" || c.Alerts.Pushover.BestMaxWaitS != 5 {
		t.Fatalf("bad defaults: %+v", c.Alerts.Pushover)
	}
	if !pushoverPickBest(PushoverCfg{SnapshotPick: " BEST "}) {
		t.Fatal("normalization failed")
	}
	if pushoverPickBest(PushoverCfg{}) {
		t.Fatal("zero value must mean primary")
	}
	dir := t.TempDir()
	load := func(name, blob string) *Config {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(blob), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := load("best.yaml", "alerts:\n  pushover:\n    snapshot_pick: best\n    best_max_wait_s: 4\n")
	if cfg.Alerts.Pushover.SnapshotPick != "best" || cfg.Alerts.Pushover.BestMaxWaitS != 4 {
		t.Fatalf("best config not applied: %+v", cfg.Alerts.Pushover)
	}
	cfg = load("bad.yaml", "alerts:\n  pushover:\n    snapshot_pick: bogus\n    best_max_wait_s: 99\n")
	if cfg.Alerts.Pushover.SnapshotPick != "primary" || cfg.Alerts.Pushover.BestMaxWaitS != 5 {
		t.Fatalf("bad values not clamped: %+v", cfg.Alerts.Pushover)
	}
}

func TestSegmentEndTime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seg.ts")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1700000000, 0)
	if err := os.Chtimes(p, want, want); err != nil {
		t.Fatal(err)
	}
	if got := segmentEndTime(p); !got.Equal(want) {
		t.Fatalf("segmentEndTime=%v want %v", got, want)
	}
	if got := segmentEnd(p); got != want.Unix() {
		t.Fatalf("segmentEnd=%d want %d", got, want.Unix())
	}
	// Missing file falls back to now (never a zero time).
	if got := segmentEndTime(filepath.Join(t.TempDir(), "missing.ts")); time.Since(got) > time.Second {
		t.Fatalf("missing segment fallback too old: %v", got)
	}
}

func TestDetectionTagList(t *testing.T) {
	d := &Detection{detType: "motion", detTypes: "motion, person,,vehicle"}
	got := strings.Join(detectionTagList(d), ",")
	if got != "motion,person,vehicle" {
		t.Fatalf("tags=%q", got)
	}
	if tags := detectionTagList(&Detection{detType: "person"}); len(tags) != 1 || tags[0] != "person" {
		t.Fatalf("primary fallback=%v", tags)
	}
}
