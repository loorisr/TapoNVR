package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testNotifyBody = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><wsnt:Notify xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"><wsnt:NotificationMessage><wsnt:Topic>tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic><wsnt:Message><tt:Message xmlns:tt="http://www.onvif.org/ver10/schema"><tt:Data><tt:SimpleItem Name="IsMotion" Value="false"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage></wsnt:Notify></s:Body></s:Envelope>`

func sourceTestApp(t *testing.T, bootSource string) *App {
	t.Helper()
	cfg := Config{}
	configDefaults(&cfg)
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.Auth.Password = ""
	cfg.Server.Auth.Disabled = true
	cfg.Cameras = []CameraCfg{{Name: "garden", Enabled: true, DetectionSource: bootSource}}
	app := &App{cfg: &cfg, db: testDB(t)}
	rememberCamBootSources(app)
	t.Cleanup(func() {
		camBootMtx.Lock()
		camBootSources = map[string]string{}
		camBootMtx.Unlock()
	})
	return app
}

func webhookAccepted(t *testing.T, app *App) bool {
	t.Helper()
	req := httptest.NewRequest("POST", "/onvif/notify?camera=cam0", strings.NewReader(testNotifyBody))
	req.RemoteAddr = "127.0.0.78:1"
	rec := httptest.NewRecorder()
	handleOnvifNotify(app, rec, req)
	var out map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}
	return out["ok"]
}

func switchSource(t *testing.T, app *App, source string) (restartRequired bool) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/cameras/cam0/detection", strings.NewReader(`{"source":"`+source+`"}`))
	rec := httptest.NewRecorder()
	handle(app, rec, req)
	var out struct {
		Source          string `json:"source"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out.Source != source {
		t.Fatalf("switch: %d %s", rec.Code, rec.Body.String())
	}
	return out.RestartRequired
}

// The RTSP reader only starts at boot: leaving ONVIF at runtime must not stop
// the running subscription, or the camera has no detection until a restart.
func TestRuntimeSwitchKeepsOnvifUntilRestart(t *testing.T) {
	app := sourceTestApp(t, "onvif")
	if switchSource(t, app, "onvif") {
		t.Error("restart_required with the boot source")
	}
	if !switchSource(t, app, "rtsp") {
		t.Error("restart_required missing after onvif -> rtsp")
	}
	if !webhookAccepted(t, app) {
		t.Error("webhook dropped after onvif -> rtsp, nothing detects until a restart")
	}
	if !switchSource(t, app, "off") {
		t.Error("restart_required missing after onvif -> off")
	}
	if webhookAccepted(t, app) {
		t.Error("webhook accepted with source off")
	}
	if switchSource(t, app, "onvif") {
		t.Error("restart_required after going back to the boot source")
	}
	if !webhookAccepted(t, app) {
		t.Error("webhook dropped after going back to onvif")
	}
}

// A camera already off ONVIF at boot has no poller: what still arrives comes
// from a stale subscription and stays ignored.
func TestStaleSubscriptionIgnoredWhenBootSourceHasNoOnvif(t *testing.T) {
	if webhookAccepted(t, sourceTestApp(t, "rtsp")) {
		t.Error("stale subscription accepted")
	}
}

// The API changes the source while webhooks read it (meaningful under -race).
func TestSourceSwitchConcurrentWithReads(t *testing.T) {
	app := sourceTestApp(t, "onvif")
	cam := &app.cfg.Cameras[0]
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = camDetectionSource(app.cfg, cam)
		}
	}()
	for i := 0; i < 50; i++ {
		switchSource(t, app, []string{"rtsp", "onvif"}[i%2])
	}
	wg.Wait()
}

func TestS3RetryHandlesMissingPrimarySnapshot(t *testing.T) {
	withToggles(t, func() {
		var mu sync.Mutex
		puts := map[string]int{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PUT" {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				puts[filepath.Base(r.URL.Path)]++
				mu.Unlock()
				w.Header().Set("ETag", `"abc"`)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
		}))
		defer srv.Close()
		dir := t.TempDir()
		cfg := Config{}
		configDefaults(&cfg)
		cfg.Server.DataDir = dir
		cfg.Storage.S3.Endpoint, cfg.Storage.S3.Bucket = srv.URL, "testbucket"
		cfg.Storage.S3.AccessKey, cfg.Storage.S3.SecretKey = "a", "s"
		app := &App{cfg: &cfg, db: testDB(t)}
		setS3UploadEnabled(true)
		extra, clip := filepath.Join(dir, "extra.jpg"), filepath.Join(dir, "clip.mp4")
		for _, p := range []string{extra, clip} {
			if err := os.WriteFile(p, []byte("data"), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		// Deleted primary: the present files go up and the row completes.
		gone := &Detection{id: newID(), snapshotPath: filepath.Join(dir, "gone.jpg"), extraSnap: extra, clipPath: clip}
		if err := dbInsertDetection(app.db, gone); err != nil {
			t.Fatal(err)
		}
		if !s3RetryOne(app, gone) {
			t.Error("row with a deleted primary snapshot stays pending")
		}
		mu.Lock()
		if puts["clip.mp4"] != 1 || puts["extra.jpg"] != 1 {
			t.Errorf("uploads: %v", puts)
		}
		mu.Unlock()
		// Anything else than "not found" may be transient: stay pending.
		broken := &Detection{id: newID(), snapshotPath: filepath.Join(clip, "child.jpg")}
		if err := dbInsertDetection(app.db, broken); err != nil {
			t.Fatal(err)
		}
		if s3RetryOne(app, broken) {
			t.Error("unreadable primary snapshot marked as uploaded")
		}
	})
}
