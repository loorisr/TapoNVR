package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// withToggles saves global switch state, runs f, then restores it so tests
// don't leak state into each other (or the dev binary).
func withToggles(t *testing.T, f func()) {
	t.Helper()
	d, r, s3, snap, push := detectionEnabled(), recordingEnabled(), s3UploadEnabled(), snapshotsEnabled(), pushoverEnabled()
	defer func() {
		setDetectionEnabled(d)
		setRecordingEnabled(r)
		setS3UploadEnabled(s3)
		setSnapshotsEnabled(snap)
		setPushoverEnabled(push)
	}()
	f()
}

func TestToggleRoundTrip(t *testing.T) {
	withToggles(t, func() {
		setS3UploadEnabled(true)
		setSnapshotsEnabled(false)
		setPushoverEnabled(true)
		setRecordingEnabled(false)
		setDetectionEnabled(false)
		if !s3UploadEnabled() || snapshotsEnabled() || !pushoverEnabled() || recordingEnabled() || detectionEnabled() {
			t.Fatal("toggle getters don't reflect setters")
		}
	})
}

func TestServeBoolToggleGETPOST(t *testing.T) {
	withToggles(t, func() {
		setS3UploadEnabled(false)
		// GET
		req := httptest.NewRequest("GET", "/api/s3/state", nil)
		rec := httptest.NewRecorder()
		handleS3State(&App{}, rec, req)
		var got map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got["enabled"] {
			t.Fatalf("GET expected enabled=false, got %q err=%v", rec.Body.String(), err)
		}
		// POST flip
		req = httptest.NewRequest("POST", "/api/s3/state?enabled=true", nil)
		rec = httptest.NewRecorder()
		handleS3State(&App{}, rec, req)
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got["enabled"] {
			t.Fatalf("POST expected enabled=true, got %q err=%v", rec.Body.String(), err)
		}
		if !s3UploadEnabled() {
			t.Fatal("POST did not flip runtime toggle")
		}
		// bad value
		req = httptest.NewRequest("POST", "/api/s3/state?enabled=bogus", nil)
		rec = httptest.NewRecorder()
		handleS3State(&App{}, rec, req)
		if rec.Code != 400 {
			t.Fatalf("expected 400 for bad value, got %d", rec.Code)
		}
	})
}

func TestDetJSONFlagsAndSize(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "snap.jpg")
	if err := os.WriteFile(snap, []byte("1234567890"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Detection{id: "x", cameraID: "cam0", camName: "c", detType: "motion",
		snapshotPath: snap, uploaded: 1, s3Expected: 1, sizeBytes: 99}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(detJSON(d)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["uploaded"] != true || parsed["s3_expected"] != true {
		t.Fatalf("flags missing: %v", parsed)
	}
	if parsed["size"] != float64(99) {
		t.Fatalf("cached size not used: %v", parsed["size"])
	}
	// sizeBytes=0 -> live compute from the 10-byte file
	d.sizeBytes = 0
	parsed = nil
	if err := json.Unmarshal([]byte(detJSON(d)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["size"] != float64(10) {
		t.Fatalf("live size fallback wrong: %v", parsed["size"])
	}
}

func testDB(t *testing.T) *DB {
	t.Helper()
	store, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDBInsertRoundTrip(t *testing.T) {
	store := testDB(t)
	d := &Detection{id: "abc", cameraID: "cam0", camName: "c", detType: "person",
		confidence: 0.9, startedAt: 1, endedAt: 2, snapshotPath: "s", clipPath: "c",
		s3Expected: 1, sizeBytes: 42}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	got, err := getDetection(store, "abc")
	if err != nil || got == nil {
		t.Fatalf("getDetection err=%v got=%v", err, got)
	}
	if got.s3Expected != 1 || got.sizeBytes != 42 || got.snapshotPath != "s" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.uploaded != 0 {
		t.Fatalf("fresh row should not be uploaded: %+v", got)
	}
	if err := dbSetUploaded(store, "abc"); err != nil {
		t.Fatal(err)
	}
	got, _ = getDetection(store, "abc")
	if got.uploaded != 1 {
		t.Fatal("dbSetUploaded not reflected")
	}
}

func TestDetectionMediaByID(t *testing.T) {
	store := testDB(t)
	dir := t.TempDir()
	snap := filepath.Join(dir, "snap.jpg")
	if err := os.WriteFile(snap, []byte("jpegdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Detection{id: "media1", cameraID: "cam0", camName: "c", detType: "motion",
		startedAt: 1, endedAt: 2, snapshotPath: snap}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	app := &App{db: store, cfg: &Config{}}
	app.cfg.Server.Auth.Password = "" // bypass auth in authOK
	app.cfg.Server.DataDir = dir
	req := httptest.NewRequest("GET", "/api/detections/media1/snapshot", nil)
	rec := httptest.NewRecorder()
	handleDetectionMedia(app, rec, req, "/api/detections/media1/snapshot")
	if rec.Code != http.StatusOK || rec.Body.String() != "jpegdata" {
		t.Fatalf("media by id failed: code=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleDetectionMedia(app, rec, req, "/api/detections/nope/snapshot")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestEscapeJSONOrder(t *testing.T) {
	if got := escapeJSON("a\"b\\c"); got != "a\\\"b\\\\c" {
		t.Fatalf("escape order wrong: %q", got)
	}
}

func TestPushoverDisabledShortCircuits(t *testing.T) {
	withToggles(t, func() {
		setPushoverEnabled(false)
		app := &App{cfg: &Config{}}
		app.cfg.Alerts.Pushover.MinIntervalS = 0      // would send for real if reached
		pushNotify(app, "t", "m", "/nonexistent.jpg") // must return without network
	})
}

func TestDeleteAllUncapped(t *testing.T) {
	store := testDB(t)
	dir := t.TempDir()
	mkfile := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, id := range []string{"d1", "d2", "d3"} {
		d := &Detection{id: id, cameraID: "cam0", camName: "c", detType: "motion",
			startedAt: 1, endedAt: 2, snapshotPath: mkfile(id + ".jpg"), clipPath: mkfile(id + ".mp4")}
		if err := dbInsertDetection(store, d); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{db: store, cfg: &Config{}}
	app.cfg.Server.DataDir = dir
	app.cfg.Server.Auth.Password = ""
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/detections", nil)
	handleDetectionDelete(app, rec, req, req.URL.Path)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["removed"] != float64(3) {
		t.Fatalf("delete-all removed=%v want 3", body["removed"])
	}
	if n := dbCount(store, "SELECT COUNT(*) FROM detections"); n != 0 {
		t.Fatalf("rows left: %d", n)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 0 {
		t.Fatalf("files left on disk: %v", left)
	}
}

func TestOnvifEventDelete(t *testing.T) {
	store := testDB(t)
	app := &App{db: store, cfg: &Config{}}
	seeds := []OnvifEvent{
		{id: "e1", cameraID: "cam0", camName: "c", detType: "motion", createdAt: 1, detectionID: "g1"},
		{id: "e2", cameraID: "cam0", camName: "c", detType: "motion", createdAt: 2, detectionID: "g1"},
		{id: "e3", cameraID: "cam0", camName: "c", detType: "person", createdAt: 3},
	}
	for _, e := range seeds {
		e := e
		if err := dbInsertOnvifEvent(store, &e); err != nil {
			t.Fatal(err)
		}
	}
	// single delete
	rec := httptest.NewRecorder()
	handleOnvifEventDelete(app, rec, httptest.NewRequest("DELETE", "/api/onvif_events/e3", nil), "/api/onvif_events/e3")
	if rec.Code != http.StatusOK {
		t.Fatalf("single delete code=%d body=%q", rec.Code, rec.Body.String())
	}
	// group delete via query
	req := httptest.NewRequest("DELETE", "/api/onvif_events?detection_id=g1", nil)
	rec = httptest.NewRecorder()
	handleOnvifEvents(app, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("group delete code=%d body=%q", rec.Code, rec.Body.String())
	}
	if n := dbCount(store, "SELECT COUNT(*) FROM onvif_events"); n != 0 {
		t.Fatalf("events left: %d", n)
	}
	// unknown id -> 404
	rec = httptest.NewRecorder()
	handleOnvifEventDelete(app, rec, httptest.NewRequest("DELETE", "/api/onvif_events/nope", nil), "/api/onvif_events/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func testGroupingApp(t *testing.T) *App {
	t.Helper()
	store := testDB(t)
	app := &App{db: store, cfg: &Config{}}
	app.cfg.Server.DataDir = t.TempDir()
	app.cfg.Server.Auth.Password = ""
	app.cfg.Record.Mode = "buffer"
	app.cfg.Record.PreDetection = 10
	app.cfg.Record.PostDetection = 10
	return app
}

func TestRapidEventsSingleGroup(t *testing.T) {
	withToggles(t, func() {
		setDetectionEnabled(true)
		setRecordingEnabled(true)
		setSnapshotsEnabled(false) // keep it fast: no snapshot chain
		setPushoverEnabled(false)
		app := testGroupingApp(t)
		// 4 events in quick succession must form ONE group, not 4 recordings.
		for i := 0; i < 4; i++ {
			handleRawOnvifEvent(app, "camG1", "indoor", "other", "")
		}
		if n := dbCount(app.db, "SELECT COUNT(*) FROM detections WHERE camera_id='camG1'"); n != 1 {
			t.Fatalf("detections=%d want 1 (double-group bug)", n)
		}
		if n := dbCount(app.db, "SELECT COUNT(DISTINCT detection_id) FROM onvif_events WHERE camera_id='camG1' AND detection_id != ''"); n != 1 {
			t.Fatalf("distinct groups=%d want 1", n)
		}
		if n := dbCount(app.db, "SELECT COUNT(*) FROM onvif_events WHERE camera_id='camG1'"); n != 4 {
			t.Fatalf("raw events=%d want 4", n)
		}
	})
}

func TestConcurrentEventsSingleGroup(t *testing.T) {
	withToggles(t, func() {
		setDetectionEnabled(true)
		setRecordingEnabled(true)
		setSnapshotsEnabled(false)
		setPushoverEnabled(false)
		app := testGroupingApp(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				handleRawOnvifEvent(app, "camG2", "indoor", "other", "")
			}()
		}
		wg.Wait()
		if n := dbCount(app.db, "SELECT COUNT(*) FROM detections WHERE camera_id='camG2'"); n != 1 {
			t.Fatalf("detections=%d want 1 (concurrent double-group)", n)
		}
	})
}

func TestFailedDetectionClearsSlot(t *testing.T) {
	withToggles(t, func() {
		setDetectionEnabled(true)
		setPushoverEnabled(false)
		// Closed DB: every insert fails, so collectDetection fails and the
		// reserved slot must be cleared instead of swallowing future events.
		store := testDB(t)
		store.db.Close()
		app := &App{db: store, cfg: &Config{}}
		app.cfg.Server.DataDir = t.TempDir()
		app.cfg.Record.PostDetection = 10
		handleRawOnvifEvent(app, "camG3", "indoor", "other", "")
		if r := getActiveRec("camG3"); r != nil {
			t.Fatal("phantom active rec left after failed detection")
		}
		// Second event must also start fresh (ungrouped), not attach to a ghost.
		handleRawOnvifEvent(app, "camG3", "indoor", "other", "")
		if r := getActiveRec("camG3"); r != nil {
			t.Fatal("phantom active rec left after second failed detection")
		}
	})
}

func TestS3KeyForPath(t *testing.T) {
	cases := map[string]string{
		"/app/data/detections/2026-09-06/cam0/120000-abc-snap.jpg": "detections/2026-09-06/cam0/120000-abc-snap.jpg",
		"/app/data/manual/cam0/120000-manual.jpg":                  "manual/cam0/120000-manual.jpg",
		"/app/data/manual/cam0/120000-manual.mp4":                  "manual/cam0/120000-manual.mp4",
	}
	for in, want := range cases {
		if got := s3KeyForPath(in); got != want {
			t.Fatalf("s3KeyForPath(%q)=%q want %q", in, got, want)
		}
	}
}

func burstTestRow(t *testing.T, app *App, id string) {
	t.Helper()
	d := &Detection{id: id, cameraID: "cam9", camName: "c", detType: "motion",
		startedAt: 1, endedAt: 2, snapshotPath: "", clipPath: ""}
	if err := dbInsertDetection(app.db, d); err != nil {
		t.Fatal(err)
	}
}

// Slow-primary simulation: eventT 5s in the past (as if the primary capture
// took 5s). Extras must still be spaced interval_s apart, not bunched.
func TestBurstSpacingAfterSlowPrimary(t *testing.T) {
	app := testGroupingApp(t) // buffer mode, no cameras -> grabs fail fast
	burstTestRow(t, app, "burst1")
	eventT := time.Now().Add(-5 * time.Second).Unix()
	start := time.Now()
	paths := captureSnapshotBurst(app, t.TempDir(), "burst1", "cam9", 2, 1, eventT)
	elapsed := time.Since(start)
	if len(paths) != 0 {
		t.Fatalf("expected no files (all grabs fail with no camera), got %v", paths)
	}
	if elapsed < 1900*time.Millisecond {
		t.Fatalf("extras bunched: 2 shots with interval=1 took %v", elapsed)
	}
}

// Continuous mode with an already-inactive camera: no shots, instant return.
func TestBurstContinuousInactive(t *testing.T) {
	app := testGroupingApp(t)
	burstTestRow(t, app, "burst2")
	setCamActive("cam9", false)
	defer setCamActive("cam9", false)
	start := time.Now()
	paths := captureSnapshotBurst(app, t.TempDir(), "burst2", "cam9", -1, 1, time.Now().Unix())
	if len(paths) != 0 {
		t.Fatalf("expected no shots while inactive, got %v", paths)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("continuous burst should stop immediately when inactive")
	}
}

// Continuous mode stops once the camera reports inactive mid-burst.
func TestBurstContinuousStopsOnInactive(t *testing.T) {
	app := testGroupingApp(t)
	burstTestRow(t, app, "burst3")
	setCamActive("cam9", true)
	defer setCamActive("cam9", false)
	go func() {
		time.Sleep(300 * time.Millisecond)
		setCamActive("cam9", false)
	}()
	start := time.Now()
	_ = captureSnapshotBurst(app, t.TempDir(), "burst3", "cam9", -1, 1, time.Now().Unix())
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("burst did not stop after inactive: %v", elapsed)
	}
}

func TestPruneReadsNullExtras(t *testing.T) {
	store := testDB(t)
	// Row with NULL extra_snapshots (the common case) plus one with extras.
	a := &Detection{id: "pa", cameraID: "cam0", camName: "c", detType: "motion",
		startedAt: 1, endedAt: 2, snapshotPath: "s.jpg", clipPath: "c.mp4"}
	if err := dbInsertDetection(store, a); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO detections(id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, extra_snapshots, locked) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		"pb", "cam0", "c", "motion", 0.9, 1, 2, "s2.jpg", "c2.mp4", "e1.jpg,e2.jpg", 0); err != nil {
		t.Fatal(err)
	}
	rows, err := listOldDetections(store, 999)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	byID := map[string]Detection{}
	for _, d := range rows {
		byID[d.id] = d
	}
	if byID["pa"].snapshotPath != "s.jpg" || byID["pa"].clipPath != "c.mp4" {
		t.Fatalf("NULL-extras row corrupted: %+v", byID["pa"])
	}
	if byID["pb"].extraSnap != "e1.jpg,e2.jpg" {
		t.Fatalf("extras not read: %q", byID["pb"].extraSnap)
	}
}

// fakeMqttMsg is a minimal paho mqtt.Message for handler tests.
type fakeMqttMsg struct {
	topic   string
	payload []byte
}

func (m *fakeMqttMsg) Duplicate() bool   { return false }
func (m *fakeMqttMsg) Qos() byte         { return 0 }
func (m *fakeMqttMsg) Retained() bool    { return false }
func (m *fakeMqttMsg) Topic() string     { return m.topic }
func (m *fakeMqttMsg) MessageID() uint16 { return 0 }
func (m *fakeMqttMsg) Payload() []byte   { return m.payload }
func (m *fakeMqttMsg) Ack()              {}

func withMqttApp(t *testing.T, app *App) {
	t.Helper()
	mqttMtx.Lock()
	prev := mqttApp
	mqttApp = app
	mqttMtx.Unlock()
	t.Cleanup(func() {
		mqttMtx.Lock()
		mqttApp = prev
		mqttMtx.Unlock()
	})
}

// A hostile TopicPrefix containing keywords must not misroute control topics.
func TestMqttHostilePrefix(t *testing.T) {
	withToggles(t, func() {
		setS3UploadEnabled(false)
		setDetectionEnabled(false)
		app := &App{cfg: &Config{}}
		app.cfg.Mqtt.TopicPrefix = "mys3"
		withMqttApp(t, app)
		mqttMessageHandler(nil, &fakeMqttMsg{topic: "mys3/s3/set", payload: []byte("ON")})
		if !s3UploadEnabled() {
			t.Fatal("s3/set not routed with hostile prefix")
		}
		mqttMessageHandler(nil, &fakeMqttMsg{topic: "mys3/detection/set", payload: []byte("ON")})
		if !detectionEnabled() {
			t.Fatal("detection/set not routed with hostile prefix")
		}
		// Unknown sub-topic changes nothing and doesn't crash.
		mqttMessageHandler(nil, &fakeMqttMsg{topic: "mys3/bogus/set", payload: []byte("ON")})
		mqttMessageHandler(nil, &fakeMqttMsg{topic: "mys3/s3/set", payload: []byte("bogus")})
		if !s3UploadEnabled() {
			t.Fatal("bad payload should not flip toggle")
		}
	})
}

func TestOnvifNotifyURLAppendsToken(t *testing.T) {
	app := &App{cfg: &Config{}}
	app.cfg.Server.Auth.Password = "s3cret-strong-pw"
	app.cfg.Server.OnvifNotifyURL = "http://192.168.1.2:8971/onvif/notify"
	tok := onvifNotifyToken(app)
	if tok == "" {
		t.Fatal("expected derived token")
	}
	// Explicit URL without token must gain the suffix (else cameras POST
	// to the bare path and handleOnvifNotify 404s, killing all detections).
	if got := onvifNotifyURL(app); got != "http://192.168.1.2:8971/onvif/notify/"+tok {
		t.Fatalf("explicit URL missing token suffix: %q", got)
	}
	// Already-suffixed URL must not double-append.
	app.cfg.Server.OnvifNotifyURL = "http://192.168.1.2:8971/onvif/notify/" + tok
	if got := onvifNotifyURL(app); got != "http://192.168.1.2:8971/onvif/notify/"+tok {
		t.Fatalf("token suffix duplicated: %q", got)
	}
	// No password, no token: explicit URL returned unchanged.
	app.cfg.Server.Auth.Password = ""
	app.cfg.Server.OnvifNotifyToken = ""
	app.cfg.Server.OnvifNotifyURL = "http://192.168.1.2:8971/onvif/notify"
	if got := onvifNotifyURL(app); got != "http://192.168.1.2:8971/onvif/notify" {
		t.Fatalf("open webhook URL rewritten: %q", got)
	}
}

func TestOnvifNotifyToken(t *testing.T) {
	app := &App{cfg: &Config{}}
	// No password, no explicit token: open legacy path.
	if got := onvifNotifyToken(app); got != "" {
		t.Fatalf("expected empty token, got %q", got)
	}
	// Explicit token wins over derivation.
	app.cfg.Server.Auth.Password = "s3cret-strong-pw"
	app.cfg.Server.OnvifNotifyToken = "explicit-token"
	if got := onvifNotifyToken(app); got != "explicit-token" {
		t.Fatalf("explicit token not honored: %q", got)
	}
	// Derived token: stable across calls, non-empty, hex.
	app.cfg.Server.OnvifNotifyToken = ""
	a := onvifNotifyToken(app)
	b := onvifNotifyToken(app)
	if a == "" || a != b || len(a) != 32 {
		t.Fatalf("derived token unstable/empty: %q %q", a, b)
	}
	app.cfg.Server.Auth.Password = "another-strong-pw"
	if c := onvifNotifyToken(app); c == a {
		t.Fatal("derived token must depend on password")
	}
}

func TestOnvifNotifyTokenWeakPassword(t *testing.T) {
	app := &App{cfg: &Config{}}
	// Weak passwords (empty, short, well-known defaults) must not seed the
	// webhook secret: an explicit token is required instead.
	for _, pw := range []string{"", "short", "admin", "changeme", "CHANGEME"} {
		app.cfg.Server.Auth.Password = pw
		app.cfg.Server.OnvifNotifyToken = ""
		if got := onvifNotifyToken(app); got != "" {
			t.Fatalf("weak password %q must not derive a token, got %q", pw, got)
		}
	}
	// An explicit token still works with a weak password.
	app.cfg.Server.Auth.Password = "admin"
	app.cfg.Server.OnvifNotifyToken = "explicit-token"
	if got := onvifNotifyToken(app); got != "explicit-token" {
		t.Fatalf("explicit token not honored with weak password: %q", got)
	}
}

func TestOnvifNotifyRejectsBadToken(t *testing.T) {
	store := testDB(t)
	app := &App{db: store, cfg: &Config{}}
	app.cfg.Server.Auth.Password = "s3cret-strong-pw" // enables token gating
	tok := onvifNotifyToken(app)
	body := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body/></s:Envelope>`
	// Wrong token -> 404, no detection processing.
	req := httptest.NewRequest("POST", "/onvif/notify/wrong?camera=x", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	handleOnvifNotify(app, rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad token: got %d want 404", rec.Code)
	}
	// Legacy path without token -> 404 as well when gating is on.
	req = httptest.NewRequest("POST", "/onvif/notify?camera=x", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	handleOnvifNotify(app, rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing token: got %d want 404", rec.Code)
	}
	// Correct token but unknown camera -> 200 ok:false (no spoofed detection).
	req = httptest.NewRequest("POST", "/onvif/notify/"+tok+"?camera=unknown-cam", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	handleOnvifNotify(app, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("good token: got %d want 200", rec.Code)
	}
	var out map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["ok"] {
		t.Fatalf("unknown camera must yield ok:false, got %q", rec.Body.String())
	}
}

func TestNotifyRateLimit(t *testing.T) {
	now := time.Now()
	ip := "10.9.9.9"
	for i := 0; i < 120; i++ {
		if !notifyAllowed(ip, now) {
			t.Fatalf("request %d unexpectedly limited", i)
		}
	}
	if notifyAllowed(ip, now) {
		t.Fatal("121st request in a minute should be limited")
	}
	if !notifyAllowed("10.9.9.10", now) {
		t.Fatal("different IP must have its own budget")
	}
	if !notifyAllowed(ip, now.Add(61*time.Second)) {
		t.Fatal("budget must reset after the window")
	}
}

func TestFileSizeHelpers(t *testing.T) {
	if fileSize("") != 0 || fileSize("/nonexistent-xyz") != 0 {
		t.Fatal("fileSize should be 0 for missing")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Detection{snapshotPath: p, clipPath: p, extraSnap: p + "," + p}
	if got := detectionSize(d); got != 20 {
		t.Fatalf("detectionSize=%d want 20", got)
	}
}

func TestPushoverMissingTokensSkips(t *testing.T) {
	withToggles(t, func() {
		setPushoverEnabled(true)
		pushRateMtx.Lock()
		lastPushUnix = 0
		pushRateMtx.Unlock()
		app := &App{cfg: &Config{}}
		app.cfg.Alerts.Pushover.Enabled = true
		app.cfg.Alerts.Pushover.MinIntervalS = 60
		app.cfg.Alerts.Pushover.AppToken = ""
		app.cfg.Alerts.Pushover.UserKey = ""
		// Must return without network and without consuming the throttle slot.
		pushNotify(app, "t", "m", "/nonexistent.jpg")
		pushRateMtx.Lock()
		defer pushRateMtx.Unlock()
		if lastPushUnix != 0 {
			t.Fatalf("missing tokens consumed throttle slot: lastPushUnix=%d", lastPushUnix)
		}
	})
}

// s3WriteObject is a test-only helper (no production callers): uploads a
// small in-memory blob via the shared s3PutObject path. Unlike deletes,
// uploads honor the runtime toggle: callers must not treat a skipped upload
// as success.
func s3WriteObject(app *App, key string, data []byte, contentType string) error {
	if len(data) == 0 {
		return fmt.Errorf("s3 upload skipped (empty object)")
	}
	return s3PutObject(app, key, bytes.NewReader(data), int64(len(data)), contentType)
}

func TestS3WriteObjectGuards(t *testing.T) {
	withToggles(t, func() {
		app := &App{cfg: &Config{}}
		app.cfg.Storage.S3.Endpoint = "https://s3.example.com"
		app.cfg.Storage.S3.Bucket = "b"
		setS3UploadEnabled(false)
		if err := s3WriteObject(app, "k", []byte("data"), "image/jpeg"); err == nil {
			t.Fatal("disabled upload should error (not silent success)")
		}
		setS3UploadEnabled(true)
		if err := s3WriteObject(app, "k", nil, "image/jpeg"); err == nil {
			t.Fatal("empty object should error")
		}
	})
}

func TestGo2rtcEscapesCredentials(t *testing.T) {
	app := &App{cfg: &Config{}}
	app.cfg.Stream.Port = 18884
	app.cfg.Stream.RtspPort = 8554
	app.cfg.Cameras = []CameraCfg{{Name: "c", Enabled: true, RtspURL: `rtsp://u:p"a\c@h:554/s`}}
	path, err := writeGo2rtcConfig(app)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `p\"a\\c`) {
		t.Fatalf("credentials not escaped: %q", string(blob))
	}
	// Two-way audio is never used: the source must carry #backchannel=0 so
	// glitchy TP-Link cameras don't get an extra backchannel SETUP.
	if !strings.Contains(string(blob), "#backchannel=0") {
		t.Fatalf("backchannel not disabled: %q", string(blob))
	}
}

func TestGo2rtcKeepsUserBackchannelFragment(t *testing.T) {
	app := &App{cfg: &Config{}}
	app.cfg.Stream.Port = 18884
	app.cfg.Stream.RtspPort = 8554
	app.cfg.Cameras = []CameraCfg{
		{Name: "a", Enabled: true, RtspURL: `rtsp://u:p@h:554/s#backchannel=1`},
		{Name: "b", Enabled: true, RtspURL: `rtsp://u:p@h:554/s#media=video`},
	}
	path, err := writeGo2rtcConfig(app)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got = string(blob)
	if !strings.Contains(got, "#backchannel=1") {
		t.Fatalf("user backchannel fragment lost: %q", got)
	}
	if strings.Contains(got, "backchannel=1#backchannel=0") {
		t.Fatalf("duplicate backchannel fragment: %q", got)
	}
	if !strings.Contains(got, "#media=video#backchannel=0") {
		t.Fatalf("backchannel not appended after existing fragment: %q", got)
	}
}

func TestAuthOKConstantTime(t *testing.T) {
	app := &App{cfg: &Config{}}
	app.cfg.Server.Auth.Username = "admin"
	app.cfg.Server.Auth.Password = "s3cret"
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.SetBasicAuth("admin", "s3cret")
	if !authOK(app, httptest.NewRecorder(), req) {
		t.Fatal("valid credentials rejected")
	}
	bad := httptest.NewRequest("GET", "/api/status", nil)
	bad.SetBasicAuth("admin", "wrong")
	if authOK(app, httptest.NewRecorder(), bad) {
		t.Fatal("invalid credentials accepted")
	}
}

func TestTruncateForLog(t *testing.T) {
	if got := truncateForLog("a\nb\rc", 100); got != "a b c" {
		t.Fatalf("newlines not flattened: %q", got)
	}
	if got := truncateForLog("abcdef", 3); got != "abc" {
		t.Fatalf("not capped: %q", got)
	}
}

func TestCompressSnapshotForPush(t *testing.T) {
	raw := snapshotJpg() // 640x360 placeholder JPEG
	// maxPx larger than source: still valid JPEG, same dimensions.
	out := compressSnapshotForPush(raw, 1280, 70)
	if len(out) == 0 {
		t.Fatal("expected compressed output")
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output not decodable: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 640 || h != 360 {
		t.Fatalf("unexpected dims %dx%d (want 640x360, aspect kept)", w, h)
	}
	// Downscale: long edge capped.
	small := compressSnapshotForPush(raw, 100, 70)
	if len(small) == 0 {
		t.Fatal("expected downscaled output")
	}
	simg, err := jpeg.Decode(bytes.NewReader(small))
	if err != nil {
		t.Fatalf("downscaled output not decodable: %v", err)
	}
	if w := simg.Bounds().Dx(); w != 100 {
		t.Fatalf("long edge=%d want 100", w)
	}
	// Lower quality is smaller for the same pixels.
	hi := compressSnapshotForPush(raw, 0, 95)
	lo := compressSnapshotForPush(raw, 0, 30)
	if len(hi) == 0 || len(lo) == 0 {
		t.Fatal("expected quality variants")
	}
	if len(lo) >= len(hi) {
		t.Fatalf("quality not applied: q30=%d q95=%d", len(lo), len(hi))
	}
	// Garbage in -> nil (caller falls back to text-only).
	if compressSnapshotForPush([]byte("not a jpeg"), 1280, 70) != nil {
		t.Fatal("garbage input should yield nil")
	}
	if compressSnapshotForPush(nil, 1280, 70) != nil {
		t.Fatal("empty input should yield nil")
	}
}

func TestPushAttachmentGate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "snap.jpg")
	if err := os.WriteFile(p, snapshotJpg(), 0o644); err != nil {
		t.Fatal(err)
	}
	base := PushoverCfg{AppToken: "a", UserKey: "u", SnapshotMaxPx: 1280, SnapshotQuality: 70}
	// Off -> nil without touching the file.
	off := base
	off.SendSnapshot = false
	if got := pushAttachment(off, p); got != nil {
		t.Fatal("send_snapshot=false must yield nil")
	}
	// On -> compressed bytes.
	on := base
	on.SendSnapshot = true
	if got := pushAttachment(on, p); len(got) == 0 {
		t.Fatal("expected attachment bytes")
	}
	// Missing file -> nil.
	if got := pushAttachment(on, filepath.Join(dir, "nope.jpg")); got != nil {
		t.Fatal("missing file must yield nil")
	}
}

func TestPushoverSnapshotDefaults(t *testing.T) {
	var c Config
	configDefaults(&c)
	if !c.Alerts.Pushover.SendSnapshot || c.Alerts.Pushover.SnapshotMaxPx != 1280 || c.Alerts.Pushover.SnapshotQuality != 70 {
		t.Fatalf("bad defaults: %+v", c.Alerts.Pushover)
	}
	// Old configs without the new keys must keep snapshots enabled.
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	blob := "server:\n  port: 8971\nalerts:\n  pushover:\n    enabled: true\n    app_token: x\n    user_key: y\n"
	if err := os.WriteFile(p, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Alerts.Pushover.SendSnapshot || cfg.Alerts.Pushover.SnapshotMaxPx != 1280 || cfg.Alerts.Pushover.SnapshotQuality != 70 {
		t.Fatalf("bad compat defaults: %+v", cfg.Alerts.Pushover)
	}
}
