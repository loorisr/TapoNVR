package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectionsCSVExport(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	app := &App{db: db}
	d := Detection{id: "abc", cameraID: "cam0", camName: "Front, \"door\"", detType: "motion", confidence: 0.9, startedAt: 1700000000, endedAt: 1700000010, snapshotPath: "/s.jpg", clipPath: "/c.mp4", sizeBytes: 123}
	if err := dbInsertDetection(db, &d); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/detections?format=csv", nil)
	rec := httptest.NewRecorder()
	handleDetections(app, rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("bad content-type %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "camera_id") || !strings.Contains(body, "abc") {
		t.Fatalf("missing header/row: %q", body)
	}
	if !strings.Contains(body, `"Front, ""door"""`) {
		t.Fatalf("CSV quoting wrong: %q", body)
	}
}

func TestOnvifEventsCSVExport(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	app := &App{db: db}
	ev := OnvifEvent{id: "e1", cameraID: "cam0", camName: "Front", detType: "motion", createdAt: 1700000000, detectionID: "abc"}
	if err := dbInsertOnvifEvent(db, &ev); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/onvif_events?format=csv&type=motion", nil)
	rec := httptest.NewRecorder()
	handleOnvifEvents(app, rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "detection_id") || !strings.Contains(body, "e1") {
		t.Fatalf("missing header/row: %q", body)
	}
	// filtered-out type returns header only
	req2 := httptest.NewRequest("GET", "/api/onvif_events?format=csv&type=person", nil)
	rec2 := httptest.NewRecorder()
	handleOnvifEvents(app, rec2, req2)
	if strings.Contains(rec2.Body.String(), "e1") {
		t.Fatalf("filter not applied: %q", rec2.Body.String())
	}
}
