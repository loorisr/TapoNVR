package main

import (
	"strings"
	"testing"
)

func TestDetSourceLabel(t *testing.T) {
	cases := map[string]string{
		"":            "unknown",
		"onvif":       "ONVIF",
		"rtsp":        "RTSP",
		"onvif,rtsp":  "ONVIF + RTSP",
		"rtsp,onvif":  "RTSP + ONVIF",
		"manual":      "Manual",
		"test":        "Test",
		"custom-path": "Custom-path",
	}
	for in, want := range cases {
		if got := detSourceLabel(in); got != want {
			t.Errorf("detSourceLabel(%q)=%q want %q", in, got, want)
		}
	}
}

func TestVisionTagRank(t *testing.T) {
	cases := []struct {
		label string
		tags  []string
		want  int
	}{
		{"person", []string{"person"}, 2},
		{"person", []string{"motion", "person"}, 2},
		{"car", []string{"vehicle"}, 2},
		{"truck", []string{"motion", "vehicle"}, 2},
		{"dog", []string{"pet"}, 2},
		{"person", []string{"motion"}, 1},
		{"car", []string{"other"}, 1},
		{"car", []string{"person"}, 0},
		{"person", nil, 1},
		{"custom", []string{"custom"}, 2},
	}
	for _, c := range cases {
		if got := visionTagRank(c.label, c.tags); got != c.want {
			t.Errorf("visionTagRank(%q,%v)=%d want %d", c.label, c.tags, got, c.want)
		}
	}
}

func TestVisionBestMatch(t *testing.T) {
	dets := []visionDetection{
		{Label: "person", Confidence: 0.80, BoxNorm: visionBox{XMin: .1, YMin: .1, XMax: .3, YMax: .5}},
		{Label: "car", Confidence: 0.95, BoxNorm: visionBox{XMin: .2, YMin: .2, XMax: .8, YMax: .8}},
	}
	if m := visionBestMatch(dets, []string{"person"}); !m.matched || m.label != "person" || m.conf != 0.80 {
		t.Fatalf("person match = %+v", m)
	}
	if m := visionBestMatch(dets, []string{"vehicle"}); !m.matched || m.label != "car" {
		t.Fatalf("vehicle match = %+v", m)
	}
	if m := visionBestMatch(dets, []string{"motion"}); !m.matched || m.label != "car" {
		t.Fatalf("motion match = %+v", m)
	}
	if m := visionBestMatch(dets, []string{"pet"}); m.matched {
		t.Fatalf("pet match should be empty, got %+v", m)
	}
}

func TestPickBestFromVisionResults(t *testing.T) {
	cands := []snapCandidate{
		{path: "/a.jpg", index: 0},
		{path: "/b.jpg", index: 1},
		{path: "/c.jpg", index: 2},
	}
	results := map[string]*visionResult{
		"/a.jpg": {Detections: []visionDetection{{Label: "person", Confidence: 0.70}}},
		"/b.jpg": {Detections: []visionDetection{{Label: "person", Confidence: 0.92}}},
		"/c.jpg": {Detections: []visionDetection{{Label: "car", Confidence: 0.99}}},
	}
	best, vr, ok := pickBestFromVisionResults(cands, []string{"person"}, results)
	if !ok || best.path != "/b.jpg" || vr == nil {
		t.Fatalf("person pick = %q ok=%v", best.path, ok)
	}
	if best.visionLabel != "person" || best.visionConf != 0.92 {
		t.Fatalf("vision fields = %+v", best)
	}
	// A specific person match must beat a higher-confidence generic motion
	// match when both tags are present.
	best, _, ok = pickBestFromVisionResults(cands, []string{"motion", "person"}, results)
	if !ok || best.path != "/b.jpg" {
		t.Fatalf("merged person/motion pick = %q", best.path)
	}
	// Without any relevant match the caller falls back to legacy scoring.
	if _, _, ok := pickBestFromVisionResults(cands, []string{"pet"}, results); ok {
		t.Fatal("pet pick should not match")
	}
}

func TestPushDetailMessage(t *testing.T) {
	d := &Detection{detType: "person", detTypes: "person,motion", detSource: "rtsp"}
	pick := snapCandidate{visionLabel: "person", visionConf: 0.87, visionCount: 3}
	got := pushDetailMessage("person detected on camera-1", d, pick)
	for _, want := range []string{
		"person detected on camera-1",
		"Detection source: RTSP",
		"Identification: camera=person,motion | YOLO=person 87% (3 boxes)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
	// No vision result and an old row without a source still gets a stable,
	// explicit line instead of a blank one.
	got = pushDetailMessage("motion detected", &Detection{detType: "motion"}, snapCandidate{})
	if !strings.Contains(got, "Detection source: unknown") || !strings.Contains(got, "Identification: camera=motion") {
		t.Fatalf("fallback message = %q", got)
	}
}

func TestDBDetectionSourceMerge(t *testing.T) {
	store := testDB(t)
	defer store.Close()
	d := &Detection{id: "src1", cameraID: "cam0", camName: "c", detType: "person", startedAt: 1, endedAt: 2, detSource: "onvif"}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	if found, err := dbMergeDetectionSource(store, "src1", "rtsp"); err != nil || !found {
		t.Fatalf("merge rtsp found=%v err=%v", found, err)
	}
	if found, err := dbMergeDetectionSource(store, "src1", "onvif"); err != nil || !found {
		t.Fatalf("merge onvif found=%v err=%v", found, err)
	}
	got, err := getDetection(store, "src1")
	if err != nil || got == nil {
		t.Fatalf("getDetection err=%v", err)
	}
	if got.detSource != "onvif,rtsp" {
		t.Fatalf("det_source=%q want onvif,rtsp", got.detSource)
	}
	// Missing rows report found=false so the caller can retry after the
	// reserving collectDetection inserts the row.
	if found, err := dbMergeDetectionSource(store, "missing", "rtsp"); err != nil || found {
		t.Fatalf("missing merge found=%v err=%v", found, err)
	}
}

func TestDetectionJSONCarriesSource(t *testing.T) {
	raw := detJSON(&Detection{id: "x", cameraID: "cam0", camName: "c", detType: "person", detSource: "rtsp"})
	if !strings.Contains(raw, `"source":"rtsp"`) {
		t.Fatalf("detJSON missing source: %s", raw)
	}
}

func TestDetectionSourceTrackedThroughGrouping(t *testing.T) {
	var store = testDB(t)
	defer store.Close()
	var app = &App{db: store, cfg: &Config{}}
	app.cfg.Server.DataDir = t.TempDir()
	app.cfg.Record.Mode = "event"
	app.cfg.Record.PostDetection = 10
	setDetectionEnabled(true)
	setSnapshotsEnabled(true)
	defer setSnapshotsEnabled(true)

	// First event: ONVIF creates the detection with source "onvif".
	handleRawOnvifEvent(app, "camS", "front", "motion", "")
	rows, err := listAllDetections(store)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].detSource != "onvif" {
		t.Fatalf("initial source=%q want onvif", rows[0].detSource)
	}
	var id = rows[0].id

	// A second event from RTSP extends the same group and merges the source.
	handleRawDetectionEvent(app, "camS", "front", "person", "", "rtsp", "")
	got, err := getDetection(store, id)
	if err != nil || got == nil {
		t.Fatalf("getDetection err=%v", err)
	}
	if got.detSource != "onvif,rtsp" {
		t.Fatalf("merged source=%q want onvif,rtsp", got.detSource)
	}
	if got.detType != "person" {
		t.Fatalf("primary type=%q want person", got.detType)
	}
}
