package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Regression tests for the bug-fix pass (routing boundary, prune override,
// ONVIF parsing, vision storage, CSV filters, mixed-case S3 scheme).

// ---------------- routing: DELETE prefix must be a real path boundary -------

func TestDeleteRouteRequiresDetectionBoundary(t *testing.T) {
	store := testDB(t)
	app := &App{db: store, cfg: &Config{}}
	app.cfg.Server.Auth.Password = "" // bypass auth in authOK
	app.cfg.Server.DataDir = t.TempDir()
	var d = &Detection{id: "keepme", cameraID: "cam0", camName: "c", detType: "motion", startedAt: 1, endedAt: 2}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	// A path that merely *starts with* /api/detections must not reach the
	// delete-all branch of handleDetectionDelete.
	req := httptest.NewRequest("DELETE", "/api/detectionsXYZ", nil)
	rec := httptest.NewRecorder()
	handle(app, rec, req)
	if got, _ := getDetection(store, "keepme"); got == nil {
		t.Fatalf("DELETE /api/detectionsXYZ wiped the archive (prefix boundary missing); code=%d", rec.Code)
	}
	// The real delete-all path still works.
	req2 := httptest.NewRequest("DELETE", "/api/detections", nil)
	rec2 := httptest.NewRecorder()
	handle(app, rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("DELETE /api/detections failed: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if got, _ := getDetection(store, "keepme"); got != nil {
		t.Fatal("DELETE /api/detections did not remove the row")
	}
}

// ---------------- retention: per-type override with global keep-forever ----

func TestPruneByTypeRunsWhenGlobalRetentionZero(t *testing.T) {
	store, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RetentionDays = 0 // keep everything globally
	cfg.Record.RetentionByType = map[string]int{"motion": 1}
	app := &App{cfg: &cfg, db: store}
	var old = timeNowMinusDays(30)
	for _, dt := range []string{"motion", "person"} {
		var d = Detection{id: "old-" + dt, cameraID: "cam0", camName: "c", detType: dt, startedAt: old, endedAt: old + 10}
		if err := dbInsertDetection(store, &d); err != nil {
			t.Fatal(err)
		}
	}
	pruneOnce(app)
	if got, _ := getDetection(store, "old-motion"); got != nil {
		t.Fatal("retention_by_type override ignored when retention_days=0")
	}
	if got, _ := getDetection(store, "old-person"); got == nil {
		t.Fatal("a type absent from retention_by_type must keep the global (keep-forever) rule")
	}
}

func timeNowMinusDays(days int) int64 {
	return time.Now().Unix() - int64(days)*24*3600
}

// ---------------- ONVIF: SubscriptionReference address parsing -------------

func TestParseSubscriptionAddrNamespaceOnElement(t *testing.T) {
	// The WS-Addressing namespace URI contains "addressing"; when declared on
	// the SubscriptionReference element it used to be matched before the real
	// <Address> and the parser returned "".
	var resp = `<?xml version="1.0"?><s:Envelope><s:Body><wsnt:SubscribeResponse>` +
		`<wsnt:SubscriptionReference xmlns:wsa5="http://www.w3.org/2005/08/addressing">` +
		`<wsa5:Address>http://192.168.1.80/onvif/sub/1</wsa5:Address></wsnt:SubscriptionReference>` +
		`</wsnt:SubscribeResponse></s:Body></s:Envelope>`
	if got := parseSubscriptionAddr(resp); got != "http://192.168.1.80/onvif/sub/1" {
		t.Fatalf("parseSubscriptionAddr = %q", got)
	}
}

func TestOnvifEventActiveHonorsIsMotionFalse(t *testing.T) {
	envelope := func(v string) []byte {
		return []byte(`<s:Envelope><s:Body><tev:Notify><wsnt:Message>` +
			`<wsnt:Topic>tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic>` +
			`<tt:Message><tt:Data><tt:SimpleItem Name="IsMotion" Value="` + v + `"/></tt:Data></tt:Message>` +
			`</wsnt:Message></tev:Notify></s:Body></s:Envelope>`)
	}
	if dtype, active := onvifEventActive(envelope("false")); dtype != "motion" || active {
		t.Fatalf("IsMotion=false: got dtype=%q active=%v (want motion/false)", dtype, active)
	}
	if dtype, active := onvifEventActive(envelope("true")); dtype != "motion" || !active {
		t.Fatalf("IsMotion=true: got dtype=%q active=%v (want motion/true)", dtype, active)
	}
}

// ---------------- config: unknown camera keys are rejected ------------------

func TestCameraUnknownKeyRejected(t *testing.T) {
	for _, body := range []string{
		"cameras:\n  - name: c\n    enabeld: false\n", // typo: would leave camera enabled
		"cameras:\n  - name: c\n    onvif:\n      pasword: x\n",
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(path, []byte("server:\n  data_dir: ./data\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatalf("unknown camera key must fail config load, got no error for %q", body)
		}
	}
}

// ---------------- vision: stored JSON/error must stay well-formed ----------

func TestVisionDetsJSONSingleHugeBoxIsArray(t *testing.T) {
	var dets = []visionDetection{{Label: strings.Repeat("x", 70000), Confidence: 0.9}}
	var out = visionDetsJSON(dets)
	if out == "null" {
		t.Fatal("visionDetsJSON returned null instead of an empty array")
	}
	var back []json.RawMessage
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("visionDetsJSON not valid JSON: %v", err)
	}
}

func TestVisionErrorStoredValidUTF8(t *testing.T) {
	store := testDB(t)
	d := &Detection{id: "v1", cameraID: "cam0", camName: "c", detType: "motion", startedAt: 1, endedAt: 2, snapshotPath: "/s.jpg"}
	if err := dbInsertDetection(store, d); err != nil {
		t.Fatal(err)
	}
	var msg = strings.Repeat("🎥", 200) // 800 bytes, multi-byte runes
	if err := dbUpsertVisionResult(store, "v1", 0, "/s.jpg", nil, msg, 5); err != nil {
		t.Fatal(err)
	}
	got, err := dbGetVisionResult(store, "v1", "/s.jpg")
	if err != nil || got == nil {
		t.Fatalf("dbGetVisionResult err=%v got=%v", err, got)
	}
	if !utf8.ValidString(got.err) {
		t.Fatalf("stored vision_error is not valid UTF-8: %q", got.err)
	}
	if len(got.err) > 300 {
		t.Fatalf("vision_error not truncated: %d bytes", len(got.err))
	}
}

func TestSnapStatusJSONConcurrent(t *testing.T) {
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			snapRecord("cam-conc", "buffer", true, "")
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		_ = snapStatusJSON("cam-conc")
	}
	<-done
}

// ---------------- CSV: vision filters honoured -----------------------------

func TestCSVHonorsVisionFilters(t *testing.T) {
	store := testDB(t)
	app := &App{db: store}
	for _, id := range []string{"hasvis", "novis"} {
		d := &Detection{id: id, cameraID: "cam0", camName: "c", detType: "person", startedAt: 1, endedAt: 2}
		if err := dbInsertDetection(store, d); err != nil {
			t.Fatal(err)
		}
	}
	res := &visionResult{Detections: []visionDetection{{Label: "person", Confidence: 0.9}}}
	if err := dbUpsertVisionResult(store, "hasvis", 0, "/s.jpg", res, "", 5); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/detections?format=csv&has_vision=1", nil)
	rec := httptest.NewRecorder()
	handleDetections(app, rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "hasvis") || strings.Contains(body, "novis") {
		t.Fatalf("has_vision filter not applied to CSV: %q", body)
	}
	req2 := httptest.NewRequest("GET", "/api/detections?format=csv&label=person", nil)
	rec2 := httptest.NewRecorder()
	handleDetections(app, rec2, req2)
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "hasvis") || strings.Contains(body2, "novis") {
		t.Fatalf("label filter not applied to CSV: %q", body2)
	}
}

// ---------------- S3: mixed-case scheme endpoint ---------------------------

func TestGetS3ClientMixedCaseScheme(t *testing.T) {
	app := &App{cfg: &Config{}}
	app.cfg.Storage.S3.Endpoint = "HTTPS://minio.example.com"
	app.cfg.Storage.S3.AccessKey = "case-test-key"
	app.cfg.Storage.S3.SecretKey = "case-test-secret"
	if _, err := getS3Client(app); err != nil {
		t.Fatalf("mixed-case https scheme rejected: %v", err)
	}
}
