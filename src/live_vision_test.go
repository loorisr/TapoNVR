package main

import (
	"os"
	"strings"
	"testing"
)

// TestLiveVisionBestPick exercises the real YOLO API when the test endpoints
// are provided. It is skipped by default; run with:
//
//	TAPONVR_VISION_TEST_URL=http://192.168.10.120:8080 \
//	TAPONVR_VISION_BUS=/tmp/bus.jpg TAPONVR_VISION_EMPTY=/tmp/test.jpg \
//	go test ./src -run TestLiveVisionBestPick -v
func TestLiveVisionBestPick(t *testing.T) {
	var url = os.Getenv("TAPONVR_VISION_TEST_URL")
	if url == "" {
		t.Skip("TAPONVR_VISION_TEST_URL not set")
	}
	var bus = os.Getenv("TAPONVR_VISION_BUS")
	var empty = os.Getenv("TAPONVR_VISION_EMPTY")
	if bus == "" || empty == "" {
		t.Skip("TAPONVR_VISION_BUS / TAPONVR_VISION_EMPTY not set")
	}
	var store = testDB(t)
	defer store.Close()
	var app = &App{db: store, cfg: &Config{}}
	app.cfg.Vision.URL = url
	app.cfg.Vision.TimeoutS = 10
	app.cfg.Vision.MinConf = 0.25
	visionMtx.Lock()
	visionOn = true
	visionMtx.Unlock()
	defer func() {
		visionMtx.Lock()
		visionOn = false
		visionMtx.Unlock()
	}()
	var det = &Detection{id: "live", cameraID: "cam0", camName: "c", detType: "person", detTypes: "person"}
	var cands = []snapCandidate{{path: empty, index: 0}, {path: bus, index: 1}, {path: empty, index: 2}}
	var best, vr = pickBestSnapshotWithVision(app, det, cands, []string{"person"})
	if best.path != bus {
		t.Fatalf("picked %q want %q (vr=%v)", best.path, bus, vr)
	}
	if vr == nil || best.visionLabel == "" || !strings.EqualFold(best.visionLabel, "person") || best.visionConf < 0.5 {
		t.Fatalf("unexpected YOLO pick label=%q conf=%.2f vr=%+v", best.visionLabel, best.visionConf, vr)
	}
	t.Logf("picked %s label=%s conf=%.2f boxes=%d", best.path, best.visionLabel, best.visionConf, best.visionCount)
}
