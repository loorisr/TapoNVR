package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The backup endpoint redacts a copy of the config: the live camera
// credentials must survive the call.
func TestBackupDoesNotMutateLiveConfig(t *testing.T) {
	var cfg Config
	configDefaults(&cfg)
	cfg.Cameras = []CameraCfg{{
		Name: "front", Enabled: true,
		RtspURL: "rtsp://user:secret@192.0.2.10/stream1",
		Onvif:   OnvifCfg{Url: "http://192.0.2.10:2020", Username: "user", Password: "secret"},
	}}
	var app = &App{cfg: &cfg}
	var w = httptest.NewRecorder()
	handleSystemBackup(app, w, httptest.NewRequest("GET", "/api/system/backup", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("backup leaks a credential: %s", w.Body.String())
	}
	if got := cfg.Cameras[0].Onvif.Password; got != "secret" {
		t.Fatalf("live onvif password overwritten: %q", got)
	}
	if got := cfg.Cameras[0].RtspURL; got != "rtsp://user:secret@192.0.2.10/stream1" {
		t.Fatalf("live rtsp url overwritten: %q", got)
	}
}

// GET/POST /api/cameras/{id}/detection must report the values in effect
// (runtime override included), not the YAML defaults.
func TestCameraDetectionReportsEffectiveTuning(t *testing.T) {
	var cfg Config
	configDefaults(&cfg)
	cfg.Cameras = []CameraCfg{{Name: "front", Enabled: true}}
	var app = &App{cfg: &cfg}
	t.Cleanup(func() {
		camOverrideMtx.Lock()
		delete(debounceOverride, "cam0")
		delete(holdOverride, "cam0")
		camOverrideMtx.Unlock()
	})
	const path = "/api/cameras/cam0/detection"
	var w = httptest.NewRecorder()
	handleCameraDetection(app, w, httptest.NewRequest("POST", path, strings.NewReader(`{"debounce_s":42,"hold_s":7}`)), path)
	if body := w.Body.String(); !strings.Contains(body, `"debounce_s":42`) || !strings.Contains(body, `"hold_s":7`) {
		t.Fatalf("POST response does not echo the new values: %s", body)
	}
	w = httptest.NewRecorder()
	handleCameraDetection(app, w, httptest.NewRequest("GET", path, nil), path)
	var body = w.Body.String()
	for _, want := range []string{`"debounce_s":42`, `"hold_s":7`, `"debounce_inherit":false`, `"hold_inherit":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET response misses %s: %s", want, body)
		}
	}
}

// Only read-only playback endpoints for configured cameras reach go2rtc: its
// config/streams API returns camera credentials and accepts writes.
func TestStreamProxyAllowlist(t *testing.T) {
	var cases = []struct {
		method, target string
		want           bool
	}{
		{"GET", "/stream/ws?src=cam0", true},
		{"GET", "/stream/stream.m3u8?src=cam12", true},
		{"GET", "/stream/hls/playlist.m3u8?id=abc", true},
		{"HEAD", "/stream/frame.jpeg?src=cam1", true},
		{"GET", "/stream/config", false},
		{"GET", "/stream/streams", false},
		{"GET", "/stream/", false},
		{"POST", "/stream/exit", false},
		{"POST", "/stream/config", false},
		{"PUT", "/stream/streams?src=cam0", false},
		{"DELETE", "/stream/streams?src=cam0", false},
		{"POST", "/stream/ws?src=cam0", false},
		{"GET", "/stream/ws?src=rtsp://192.0.2.1/x", false},
		{"GET", "/stream/stream.mp4?src=cam0&src=exec:id", false},
		{"GET", "/stream/stream.mp4?src=cam0&dst=cam1", false},
		{"GET", "/stream/hls/../config", false},
	}
	for _, c := range cases {
		var r = httptest.NewRequest(c.method, c.target, nil)
		if got := streamProxyAllowed(r); got != c.want {
			t.Errorf("%s %s: allowed = %v, want %v", c.method, c.target, got, c.want)
		}
	}
	var w = httptest.NewRecorder()
	handleStream(w, httptest.NewRequest("GET", "/stream/config", nil))
	if w.Code != 403 {
		t.Fatalf("blocked endpoint status = %d, want 403", w.Code)
	}
}

// A camera that refuses both subscriptions at first (offline at boot) and
// whose PullPoint always faults must still end up on the webhook once it
// accepts it: the loop used to retry PullPoint alone forever.
func TestOnvifEventLoopRetriesWebhook(t *testing.T) {
	var subscribeCalls, pullPointCalls int32
	var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body = string(raw)
		var self = "http://" + r.Host + "/onvif/service"
		switch {
		case strings.Contains(body, "GetCapabilities"):
			_, _ = io.WriteString(w, "<Capabilities><Media><XAddr>"+self+"</XAddr></Media><Events><XAddr>"+self+"</XAddr></Events></Capabilities>")
		case strings.Contains(body, "GetProfiles"):
			_, _ = io.WriteString(w, `<Profiles token="p0"></Profiles>`)
		case strings.Contains(body, "CreatePullPointSubscription"):
			atomic.AddInt32(&pullPointCalls, 1)
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "<s:Fault>ter:InvalidArgVal</s:Fault>")
		case strings.Contains(body, "wsnt:Subscribe"):
			if atomic.AddInt32(&subscribeCalls, 1) <= 2 {
				w.WriteHeader(500)
				return
			}
			_, _ = io.WriteString(w, "<wsnt:SubscribeResponse><wsnt:SubscriptionReference><wsa:Address>"+self+"/sub1</wsa:Address></wsnt:SubscriptionReference></wsnt:SubscribeResponse>")
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	var prev = onvifSubscribeRetry
	onvifSubscribeRetry = 10 * time.Millisecond
	t.Cleanup(func() { onvifSubscribeRetry = prev })

	var cfg Config
	configDefaults(&cfg)
	cfg.Server.OnvifNotifyURL = "http://192.0.2.1:8971/onvif/notify"
	// Padding entries so this test's camera id is not shared with other tests.
	cfg.Cameras = make([]CameraCfg, 43)
	cfg.Cameras[42] = CameraCfg{Name: "retry-cam", Enabled: true, Onvif: OnvifCfg{Url: srv.URL + "/onvif/service", Username: "u", Password: "p"}}
	var app = &App{cfg: &cfg}
	// After the webhook subscribes, the loop sleeps until its 10 min renewal:
	// the goroutine is left parked for the rest of the test binary's life.
	go onvifEventLoop(app, &cfg.Cameras[42], "cam42")

	var deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		notifySubs.Lock()
		_, ok := notifySubs.m["cam42"]
		notifySubs.Unlock()
		if ok {
			if n := atomic.LoadInt32(&pullPointCalls); n < 2 {
				t.Fatalf("expected the PullPoint fallback to be tried between webhook attempts, got %d", n)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("webhook never subscribed (subscribe attempts=%d, pullpoint attempts=%d)", atomic.LoadInt32(&subscribeCalls), atomic.LoadInt32(&pullPointCalls))
}
