package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// motion then person: the clip must keep both tags and promote the primary
// type to the more specific one.
func TestDetectionTagsMergeAndPromote(t *testing.T) {
	withToggles(t, func() {
		setDetectionEnabled(true)
		setRecordingEnabled(true)
		setSnapshotsEnabled(false)
		setPushoverEnabled(false)
		app := testGroupingApp(t)

		handleRawOnvifEvent(app, "camT1", "indoor", "motion", "")
		handleRawOnvifEvent(app, "camT1", "indoor", "person", "")

		var gid string
		if err := app.db.db.QueryRow("SELECT detection_id FROM onvif_events WHERE camera_id='camT1' AND det_type='person'").Scan(&gid); err != nil {
			t.Fatal(err)
		}
		got, err := getDetection(app.db, gid)
		if err != nil || got == nil {
			t.Fatalf("getDetection err=%v got=%v", err, got)
		}
		if got.detType != "person" {
			t.Fatalf("primary type=%q want person", got.detType)
		}
		var tags = parseDetTypes(got.detTypes)
		if len(tags) != 2 || tags[0] != "motion" || tags[1] != "person" {
			t.Fatalf("det_types=%q want motion,person", got.detTypes)
		}
		var raw = detJSON(got)
		if !strings.Contains(raw, `"type":"person"`) || !strings.Contains(raw, `"types":["person","motion"]`) {
			t.Fatalf("detJSON missing merged tags: %s", raw)
		}
	})
}

// Two equally specific tags: the first one stays primary, both are listed.
func TestDetectionTagsEqualRankKeepsFirst(t *testing.T) {
	withToggles(t, func() {
		setDetectionEnabled(true)
		setRecordingEnabled(true)
		setSnapshotsEnabled(false)
		setPushoverEnabled(false)
		app := testGroupingApp(t)

		handleRawOnvifEvent(app, "camT2", "indoor", "person", "")
		handleRawOnvifEvent(app, "camT2", "indoor", "vehicle", "")

		var gid string
		if err := app.db.db.QueryRow("SELECT detection_id FROM onvif_events WHERE camera_id='camT2' AND det_type='vehicle'").Scan(&gid); err != nil {
			t.Fatal(err)
		}
		got, _ := getDetection(app.db, gid)
		if got == nil {
			t.Fatal("detection missing")
		}
		if got.detType != "person" {
			t.Fatalf("primary type=%q want person (first specific tag)", got.detType)
		}
		if got.detTypes != "person,vehicle" {
			t.Fatalf("det_types=%q want person,vehicle", got.detTypes)
		}
	})
}

// In-memory reservation merge (the DB row does not exist yet while the first
// event is still capturing its snapshot).
func TestActiveRecTypeMerge(t *testing.T) {
	var camID = "camT3"
	setActiveRec(camID, &activeRec{id: "g1", detType: "motion", detTypes: "motion"})
	defer clearActiveRec(camID, "g1")

	groupMtx.Lock()
	var primary, promoted = addActiveRecType(camID, "g1", "person")
	groupMtx.Unlock()
	if !promoted || primary != "person" {
		t.Fatalf("promoted=%v primary=%q want true/person", promoted, primary)
	}
	var rec = getActiveRec(camID)
	if rec == nil || rec.detTypes != "motion,person" {
		t.Fatalf("slot types=%v want motion,person", rec)
	}
	groupMtx.Lock()
	primary, promoted = addActiveRecType(camID, "g1", "pet")
	groupMtx.Unlock()
	if promoted || primary != "person" {
		t.Fatalf("equal-rank pet must not promote: promoted=%v primary=%q", promoted, primary)
	}
}

// The type filter must match any merged tag, not only the primary one.
func TestDetectionTypeFilterMatchesAnyTag(t *testing.T) {
	store := testDB(t)
	defer store.db.Close()
	d := &Detection{id: "f1", cameraID: "cam0", camName: "c", detType: "person", detTypes: "motion,person", startedAt: 1, endedAt: 2}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	for _, ty := range []string{"motion", "person"} {
		rows, err := listDetectionsFiltered(store, 0, 0, "", ty, "", false, 10, 0)
		if err != nil || len(rows) != 1 {
			t.Fatalf("type=%s rows=%d err=%v want 1", ty, len(rows), err)
		}
		if n := countDetectionsFiltered(store, 0, 0, "", ty, "", false); n != 1 {
			t.Fatalf("type=%s count=%d want 1", ty, n)
		}
	}
	if rows, _ := listDetectionsFiltered(store, 0, 0, "", "pet", "", false, 10, 0); len(rows) != 0 {
		t.Fatalf("type=pet matched %d rows", len(rows))
	}
}

// One-time backfill recovers det_types from raw events without rewriting the
// historical primary type.
func TestBackfillDetectionTypes(t *testing.T) {
	store := testDB(t)
	defer store.db.Close()
	d := &Detection{id: "bf1", cameraID: "cam0", camName: "c", detType: "motion", startedAt: 1, endedAt: 2}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	// Simulate a row created before the det_types column existed.
	if _, err := store.db.Exec("UPDATE detections SET det_types='' WHERE id='bf1'"); err != nil {
		t.Fatal(err)
	}
	for _, ty := range []string{"motion", "person"} {
		ev := OnvifEvent{id: "ev-" + ty, cameraID: "cam0", camName: "c", detType: ty, createdAt: 1, detectionID: "bf1"}
		if err := dbInsertOnvifEvent(store, &ev); err != nil {
			t.Fatal(err)
		}
	}
	backfillDetectionTypes(store)
	got, err := getDetection(store, "bf1")
	if err != nil || got == nil {
		t.Fatalf("getDetection err=%v got=%v", err, got)
	}
	var tags = parseDetTypes(got.detTypes)
	if len(tags) != 2 || tags[0] != "motion" || tags[1] != "person" {
		t.Fatalf("backfilled det_types=%q want motion,person", got.detTypes)
	}
	if got.detType != "motion" {
		t.Fatalf("backfill rewrote historical primary type: %q", got.detType)
	}
}

// CSV export carries the merged types and honours multi-tag filtering.
func TestDetectionsCSVIncludesMergedTypes(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	app := &App{db: db}
	d := Detection{id: "csv1", cameraID: "cam0", camName: "Front", detType: "person", detTypes: "motion,person", confidence: 0.9, startedAt: 1700000000, endedAt: 1700000010}
	if err := dbInsertDetection(db, &d); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/detections?format=csv&type=motion", nil)
	rec := httptest.NewRecorder()
	handleDetections(app, rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "types") || !strings.Contains(body, `"motion,person"`) {
		t.Fatalf("merged types missing from CSV: %q", body)
	}
}
