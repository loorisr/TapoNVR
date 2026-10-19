package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for the correctness/data-loss/race fixes. Each test names
// the behaviour it protects so a change that reintroduces the bug fails loudly.

// ---------------- retention: 0 must never wipe the archive -----------------

func TestRetentionZeroNeverPrunes(t *testing.T) {
	for _, days := range []int{0, -5} {
		store, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{}
		configDefaults(&cfg)
		cfg.Server.DataDir = t.TempDir()
		cfg.Server.RetentionDays = days
		app := &App{cfg: &cfg, db: store}
		var d Detection
		d.id = newID()
		d.cameraID = "cam0"
		d.camName = "c"
		d.detType = "motion"
		d.startedAt = time.Now().Unix() - 30*24*3600 // very old
		d.endedAt = d.startedAt + 10
		if err := dbInsertDetection(store, &d); err != nil {
			t.Fatal(err)
		}
		pruneOnce(app)
		got, err := getDetection(store, d.id)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("retention_days=%d deleted a detection (0/negative must mean never-prune)", days)
		}
	}
}

func TestRetentionPrunesOldUnlockedRows(t *testing.T) {
	store, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RetentionDays = 7
	app := &App{cfg: &cfg, db: store}
	var old Detection
	old.id = newID()
	old.cameraID, old.camName, old.detType = "cam0", "c", "motion"
	old.startedAt = time.Now().Unix() - 30*24*3600
	old.endedAt = old.startedAt + 10
	if err := dbInsertDetection(store, &old); err != nil {
		t.Fatal(err)
	}
	var locked Detection
	locked.id = newID()
	locked.cameraID, locked.camName, locked.detType = "cam0", "c", "motion"
	locked.startedAt, locked.endedAt = old.startedAt, old.endedAt
	if err := dbInsertDetection(store, &locked); err != nil {
		t.Fatal(err)
	}
	if err := dbSetLocked(store, locked.id, true); err != nil {
		t.Fatal(err)
	}
	pruneOnce(app)
	if got, _ := getDetection(store, old.id); got != nil {
		t.Fatal("an expired unlocked detection must be pruned")
	}
	if got, _ := getDetection(store, locked.id); got == nil {
		t.Fatal("a locked detection must survive the prune")
	}
}

func TestRetentionByTypeOverridesGlobal(t *testing.T) {
	store, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RetentionDays = 30
	cfg.Record.RetentionByType = map[string]int{"motion": 3}
	app := &App{cfg: &cfg, db: store}
	mkdet := func(dtype string) string {
		var d Detection
		d.id = newID()
		d.cameraID, d.camName, d.detType = "cam0", "c", dtype
		d.startedAt = time.Now().Unix() - 10*24*3600
		d.endedAt = d.startedAt + 10
		if err := dbInsertDetection(store, &d); err != nil {
			t.Fatal(err)
		}
		return d.id
	}
	motionID, personID := mkdet("motion"), mkdet("person")
	pruneOnce(app)
	if got, _ := getDetection(store, motionID); got != nil {
		t.Fatal("motion older than retention_by_type motion:3 must be pruned")
	}
	if got, _ := getDetection(store, personID); got == nil {
		t.Fatal("person must survive under the global 30-day retention")
	}
}

func TestRetentionByTypeConfig(t *testing.T) {
	var yml = []byte("record:\n  retention_by_type:\n    Motion: 3\n    bogus: 5\n    person: -2\n")
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, yml, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Record.RetentionByType["motion"] != 3 {
		t.Fatalf("motion not normalized: %+v", cfg.Record.RetentionByType)
	}
	if _, ok := cfg.Record.RetentionByType["bogus"]; ok {
		t.Fatalf("unknown label kept: %+v", cfg.Record.RetentionByType)
	}
	if _, ok := cfg.Record.RetentionByType["person"]; ok {
		t.Fatalf("negative entry kept: %+v", cfg.Record.RetentionByType)
	}
}

// ---------------- config: defaults + strict keys --------------------------

func TestRecordSnapshotsDefaultsPreserved(t *testing.T) {
	var yml = []byte("server:\n  data_dir: ./data\nrecord:\n  snapshots:\n    enabled: true\n")
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, yml, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Record.Snapshots.Enabled {
		t.Fatal("snapshots.enabled not applied")
	}
	if cfg.Record.Snapshots.Count != 1 || cfg.Record.Snapshots.IntervalS != 2 {
		t.Fatalf("partial record.snapshots wiped its defaults: count=%d interval=%d (want 1/2)",
			cfg.Record.Snapshots.Count, cfg.Record.Snapshots.IntervalS)
	}
}

func TestRecordUnknownKeyRejected(t *testing.T) {
	for _, body := range []string{
		"record:\n  pre_detecton: 30\n",        // typo in the main key
		"record:\n  snapshots:\n    coun: 3\n", // typo in a sub-key
		"record:\n  bogus_key: 1\n",
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(path, []byte("server:\n  data_dir: ./data\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatalf("an unknown key under record: must fail config load, got no error for %q", body)
		}
	}
}

// ---------------- JSON / CSV / LIKE escaping ------------------------------

func TestJSONStringControlCharsAreValid(t *testing.T) {
	// NUL and friends must round-trip: emitting them raw produces invalid JSON.
	var want = "a" + string(rune(1)) + "b\nc\"d\\e\rf\tg"
	raw := jsonString(want)
	var back string
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("jsonString produced invalid JSON %s: %v", raw, err)
	}
	if back != want {
		t.Fatalf("round-trip mismatch: %q", back)
	}
}

func TestEscapeJSONKeepsLiteralValid(t *testing.T) {
	var raw = "{" + kv("k", "x") + ",\"v\":\"" + escapeJSON("pq\"r") + "\"}"
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("escapeJSON produced invalid JSON %s: %v", raw, err)
	}
	if m["v"] != "pq\"r" {
		t.Fatalf("unexpected value %q", m["v"])
	}
}

func TestCSVEscapeNeutralizesFormulas(t *testing.T) {
	for _, in := range []string{"=1+1", "+cmd", "-2", "@SUM(A1)"} {
		var got = csvEscape(in)
		if strings.ContainsAny(string(got[0]), "=+-@") {
			t.Fatalf("csvEscape(%q) = %q still starts with a formula character", in, got)
		}
	}
	if got := csvEscape("plain"); got != "plain" {
		t.Fatalf("csvEscape must not alter ordinary values, got %q", got)
	}
	if got := csvEscape("a,b"); got != "\"a,b\"" {
		t.Fatalf("csvEscape quoting changed: %q", got)
	}
}

func TestLikeEscape(t *testing.T) {
	if got := likeEscape("100%_x"); got != "100\\%\\_x" {
		t.Fatalf("likeEscape = %q", got)
	}
}

// ---------------- ONVIF topic classification ------------------------------

func TestOnvifTopicTypeIgnoresNonDetections(t *testing.T) {
	cases := map[string]string{
		"tns1:RuleEngine/CellMotionDetector/Motion":    "motion",
		"tns1:RuleEngine/MyRuleDetector/PeopleDetect":  "person",
		"tns1:RuleEngine/LineDetector/Cross":           "motion",
		"tns1:RuleEngine/FieldDetector/ObjectsInside":  "motion",
		"tns1:VideoSource/MotionAlarm":                 "motion",
		"tns1:RuleEngine/MyRuleDetector/VehicleDetect": "vehicle",
		// not detections: these used to be reported as "other" and created
		// recordings for tamper alarms, SD-card events, relay triggers...
		"tns1:Device/Trigger/Relay":        "",
		"tns1:Monitoring/Storage/Inserted": "",
		"tns1:Device/Tampering/Tampering":  "",
		"":                                 "",
	}
	for topic, want := range cases {
		if got := onvifTopicType(topic); got != want {
			t.Errorf("onvifTopicType(%q) = %q, want %q", topic, got, want)
		}
	}
}

func TestOnvifEventActiveIgnoresUnrelatedNotifications(t *testing.T) {
	var raw = []byte("<s:Envelope><s:Body><tev:Notify><wsnt:Message><wsnt:Topic>tns1:Device/Trigger/Relay</wsnt:Topic><tt:Message><tt:Source><tt:SimpleItem Name=\"Input\" Value=\"1\"/></tt:Source><tt:Data><tt:SimpleItem Name=\"State\" Value=\"true\"/></tt:Data></tt:Message></wsnt:Message></tev:Notify></s:Body></s:Envelope>")
	if dtype, active := onvifEventActive(raw); active && dtype != "" {
		t.Fatalf("a relay notification must not look like a detection (got %q)", dtype)
	}
}

// ---------------- grouping slots ------------------------------------------

func TestConfirmActiveRecDoesNotHijack(t *testing.T) {
	clearAllActiveRecs()
	defer clearAllActiveRecs()
	var now = time.Now().Unix()
	setActiveRec("cam0", &activeRec{id: "real", firstAt: now, lastAt: now, endAt: now + 10, established: true})
	// A different recording (e.g. a manual test detection) must not steal the
	// camera's grouping slot.
	confirmActiveRec("cam0", "other", now+100)
	if r := getActiveRec("cam0"); r == nil || r.id != "real" {
		t.Fatalf("slot was hijacked: %+v", r)
	}
	// The owner still extends normally.
	confirmActiveRec("cam0", "real", now+50)
	if r := getActiveRec("cam0"); r == nil || r.endAt != now+50 {
		t.Fatalf("owner confirm lost: %+v", r)
	}
	// A free camera can still be reserved.
	confirmActiveRec("cam1", "x", now+5)
	if r := getActiveRec("cam1"); r == nil || r.id != "x" {
		t.Fatalf("free camera not reserved: %+v", r)
	}
}

// ---------------- vision JSON + snapshot paths ----------------------------

func TestVisionDetsJSONStaysValidWhenHuge(t *testing.T) {
	var dets []visionDetection
	for i := 0; i < 4000; i++ {
		dets = append(dets, visionDetection{Label: strings.Repeat("x", 40), Confidence: 0.9})
	}
	var out = visionDetsJSON(dets)
	var back []json.RawMessage
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("visionDetsJSON truncated into invalid JSON: %v", err)
	}
	if len(out) > 70000 {
		t.Fatalf("visionDetsJSON not bounded: %d bytes", len(out))
	}
}

func TestDbAppendSnapPathsAppends(t *testing.T) {
	store, err := openDB(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var d Detection
	d.id = newID()
	d.cameraID, d.camName, d.detType = "cam0", "c", "motion"
	d.startedAt, d.endedAt = time.Now().Unix(), time.Now().Unix()
	if err := dbInsertDetection(store, &d); err != nil {
		t.Fatal(err)
	}
	// Concurrent appends must not lose paths (was a read-modify-write).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = dbAppendSnapPaths(store, d.id, []string{"/tmp/snap" + strconv.Itoa(i) + ".jpg"})
		}(i)
	}
	wg.Wait()
	got, err := getDetection(store, d.id)
	if err != nil || got == nil {
		t.Fatalf("reload failed: %v", err)
	}
	if n := len(strings.Split(got.extraSnap, ",")); n != 8 {
		t.Fatalf("lost snapshot paths: got %d of 8 (%q)", n, got.extraSnap)
	}
}

// ---------------- camera host must not leak credentials -------------------

func TestCameraHostOmitsCredentials(t *testing.T) {
	c := &CameraCfg{Name: "x", Onvif: OnvifCfg{Url: "http://admin:secret@192.168.1.50:8080/onvif/service"}}
	if host := cameraHost(c); host != "192.168.1.50" {
		t.Fatalf("cameraHost = %q, want 192.168.1.50", host)
	}
	var payload = jsonCamString(c, "cam0", 0)
	if strings.Contains(payload, "secret") {
		t.Fatal("/api/cameras payload leaks the camera password")
	}
	if strings.Contains(payload, "rtsp_url") {
		t.Fatal("/api/cameras must not expose rtsp_url (it carries credentials)")
	}
}

// ---------------- CSRF guard ----------------------------------------------

func TestCSRFOK(t *testing.T) {
	mk := func(method, site, origin string) *http.Request {
		// Absolute URL so r.Host matches the Origin used below.
		r := httptest.NewRequest(method, "http://nvr.local:8971/api/detection/state", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	if !csrfOK(mk("GET", "cross-site", "https://evil.example")) {
		t.Fatal("GET must never be blocked")
	}
	if !csrfOK(mk("POST", "same-origin", "")) {
		t.Fatal("same-origin POST must be allowed")
	}
	if csrfOK(mk("POST", "cross-site", "https://evil.example")) {
		t.Fatal("cross-site POST must be rejected")
	}
	if csrfOK(mk("DELETE", "same-site", "")) {
		t.Fatal("same-site (different origin) DELETE must be rejected")
	}
	if !csrfOK(mk("POST", "", "")) {
		t.Fatal("non-browser clients must keep working")
	}
	if csrfOK(mk("POST", "", "https://evil.example")) {
		t.Fatal("foreign Origin must be rejected")
	}
	if !csrfOK(mk("POST", "", "http://nvr.local:8971")) {
		t.Fatal("matching Origin must be allowed")
	}
}

// ---------------- go2rtc process handle: readers vs monitor goroutine ------

func TestStreamCmdAccessorsRaceFree(t *testing.T) {
	// /bin/true exits immediately, so the monitor goroutine (which clears
	// streamCmd) runs concurrently with the readers below - exactly the
	// interleaving that used to be an unsynchronized global write.
	var cfg Config
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Stream.Go2rtcPath = "/bin/true"
	var app = &App{cfg: &cfg, db: nil}
	defer stopStream()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = getStreamCmd()
				_ = getStreamCmdPID()
			}
		}()
	}
	startStream(app)
	wg.Wait()
	stopStream()
	stopStream() // must stay safe when called twice (signal handler + run)
}

// ---------------- documented config must stay loadable --------------------

func TestExampleConfigLoads(t *testing.T) {
	// Guards doc drift: config.example.yaml is what users copy, so a key that
	// the loader rejects (or a documented key it does not know) must fail here.
	cfg, err := loadConfig("../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml no longer loads: %v", err)
	}
	if cfg.Server.Port == 0 {
		t.Fatal("example config lost its server.port default")
	}
	if cfg.Server.RetentionDays <= 0 {
		t.Fatal("example config must document a positive retention_days")
	}
}
