package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression tests for the maintenance/robustness fixes: default bind host,
// healthcheck target, orphan vision rows, bounded prune queries, webhook
// events from disabled cameras and the auth failure limiter.

func TestDefaultHostIsAllInterfaces(t *testing.T) {
	var cfg Config
	configDefaults(&cfg)
	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("default server.host = %q, want 0.0.0.0", cfg.Server.Host)
	}
	for in, want := range map[string]string{
		"": "", "0.0.0.0": "", " 0.0.0.0 ": "", "[IP_ADDRESS]": "",
		"192.168.1.5": "192.168.1.5", "[::1]": "::1", "::": "::",
	} {
		if got := listenHost(in); got != want {
			t.Errorf("listenHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeHostFollowsBindAddress(t *testing.T) {
	for in, want := range map[string]string{
		"": "127.0.0.1", "0.0.0.0": "127.0.0.1", "::": "127.0.0.1",
		"[IP_ADDRESS]": "127.0.0.1", "192.168.1.5": "192.168.1.5", "127.0.0.1": "127.0.0.1",
	} {
		if got := probeHost(in); got != want {
			t.Errorf("probeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func insertOldDetection(t *testing.T, store *DB, ageDays int) Detection {
	t.Helper()
	var d Detection
	d.id = newID()
	d.cameraID, d.camName, d.detType = "cam0", "c", "motion"
	d.startedAt = time.Now().Unix() - int64(ageDays)*24*3600
	d.endedAt = d.startedAt + 10
	if err := dbInsertDetection(store, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeleteDetectionRowsRemovesVisionOverrides(t *testing.T) {
	store := testDB(t)
	d := insertOldDetection(t, store, 1)
	if err := dbAddVisionOverride(store, d.id, 0, "person"); err != nil {
		t.Fatal(err)
	}
	if err := dbDeleteDetectionRows(store, d.id); err != nil {
		t.Fatal(err)
	}
	if n := dbCountOne(store, "SELECT COUNT(*) FROM vision_overrides WHERE detection_id=?", d.id); n != 0 {
		t.Fatalf("vision_overrides left behind after delete: %d", n)
	}
	if got, _ := getDetection(store, d.id); got != nil {
		t.Fatal("detection row still present")
	}
}

func TestPruneRemovesVisionOverrides(t *testing.T) {
	store := testDB(t)
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RetentionDays = 7
	app := &App{cfg: &cfg, db: store}
	d := insertOldDetection(t, store, 30)
	if err := dbAddVisionOverride(store, d.id, 1, "car"); err != nil {
		t.Fatal(err)
	}
	pruneOnce(app)
	if got, _ := getDetection(store, d.id); got != nil {
		t.Fatal("old detection not pruned")
	}
	if n := dbCountOne(store, "SELECT COUNT(*) FROM vision_overrides"); n != 0 {
		t.Fatalf("prune left %d orphan vision_overrides rows", n)
	}
}

func TestListOldDetectionsLimit(t *testing.T) {
	store := testDB(t)
	for i := 0; i < 5; i++ {
		insertOldDetection(t, store, 30+i)
	}
	cutoff := time.Now().Unix()
	if rows, err := listOldDetections(store, cutoff, 2); err != nil || len(rows) != 2 {
		t.Fatalf("limit 2: got %d rows, err %v", len(rows), err)
	}
	if rows, err := listOldDetections(store, cutoff, 0); err != nil || len(rows) != 5 {
		t.Fatalf("limit 0 (unbounded): got %d rows, err %v", len(rows), err)
	}
	rows, err := listOldDetectionsByType(store, cutoff, "motion", 3)
	if err != nil || len(rows) != 3 {
		t.Fatalf("by type limit 3: got %d rows, err %v", len(rows), err)
	}
	// Oldest first.
	if rows[0].endedAt > rows[1].endedAt {
		t.Fatal("rows not ordered oldest first")
	}
}

func TestOnvifNotifyIgnoresDisabledCamera(t *testing.T) {
	store := testDB(t)
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.Auth.Password = ""
	cfg.Server.OnvifNotifyToken = ""
	cfg.Cameras = []CameraCfg{{Name: "garden", Enabled: false}}
	app := &App{cfg: &cfg, db: store}
	body := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><wsnt:Notify xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"><wsnt:NotificationMessage><wsnt:Topic>tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic><wsnt:Message><tt:Message xmlns:tt="http://www.onvif.org/ver10/schema"><tt:Data><tt:SimpleItem Name="IsMotion" Value="true"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage></wsnt:Notify></s:Body></s:Envelope>`
	for _, cam := range []string{"cam0", "garden"} {
		req := httptest.NewRequest("POST", "/onvif/notify?camera="+cam, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.9:1"
		rec := httptest.NewRecorder()
		handleOnvifNotify(app, rec, req)
		var out map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["ok"] {
			t.Fatalf("camera=%s: disabled camera event accepted: %d %s", cam, rec.Code, rec.Body.String())
		}
	}
	if camIsActive("cam0") {
		t.Fatal("disabled camera marked active")
	}
}

func TestAuthLimiterIgnoresCredentiallessRequests(t *testing.T) {
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.Auth.Password = "a-strong-password"
	app := &App{cfg: &cfg}
	const ip = "198.51.100.7"
	authFailMtx.Lock()
	delete(authFailMap, ip)
	authFailMtx.Unlock()
	// A page load fires many unauthenticated requests before the browser
	// answers the Basic challenge: none of them may count as failures.
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		if authOK(app, rec, req) {
			t.Fatal("request without credentials accepted")
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: got %d want 401", i, rec.Code)
		}
	}
	// The correct password must still get through afterwards.
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.RemoteAddr = ip + ":1234"
	req.SetBasicAuth(cfg.Server.Auth.Username, cfg.Server.Auth.Password)
	if !authOK(app, httptest.NewRecorder(), req) {
		t.Fatal("valid credentials rejected")
	}
	// Wrong passwords are still limited.
	var last int
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.RemoteAddr = ip + ":1234"
		req.SetBasicAuth("admin", "wrong")
		rec := httptest.NewRecorder()
		authOK(app, rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("wrong passwords not rate limited: last status %d", last)
	}
	authFailMtx.Lock()
	delete(authFailMap, ip)
	authFailMtx.Unlock()
}
