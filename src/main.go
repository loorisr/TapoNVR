package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// dataDirWritable reports whether the process can create files in dir. MkdirAll
// succeeding is not enough: the directory may exist with ownership/mode that
// denies this UID (SQLite then fails with the opaque SQLITE_CANTOPEN).
func dataDirWritable(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func run(args []string) int {
	// Container HEALTHCHECK on shell-less (distroless) images: probe the local
	// API the same way the old wget check did (200 ok or 401 auth = alive).
	if len(args) >= 1 && args[0] == "--healthcheck" {
		return runHealthcheck(args[1:])
	}
	var path string
	if len(args) >= 2 && args[0] == "--config" {
		path = args[1]
	} else {
		logIt(LERR, "missing --config <path>", map[string]string{"hint": "usage: taponvr --config <path>.yaml"})
		return 1
	}
	cfg, cerr := loadConfig(path)
	if cerr != nil {
		logIt(LERR, "failed to load config", map[string]string{"path": path, "error": cerr.Error()})
		return 1
	}
	if e := os.MkdirAll(cfg.Server.DataDir, 0o750); e != nil {
		// Non-fatal here: openDB reports the actionable permission hint if the
		// directory really is unusable.
		logIt(LWARN, "failed to create data dir", map[string]string{"dir": cfg.Server.DataDir, "error": e.Error()})
	} else {
		// Media + DB live here (incl. camera credentials in db.sqlite):
		// keep it owner-only even when a pre-existing dir is looser.
		_ = os.Chmod(cfg.Server.DataDir, 0o750)
	}
	// Probe writability explicitly: an existing directory owned by another
	// UID (a named volume created by an older image build, or a bind-mount
	// owned by a different host user) surfaces as SQLITE_CANTOPEN, which says
	// nothing about why. Report the real cause and the fix.
	if !dataDirWritable(cfg.Server.DataDir) {
		logIt(LERR, "data dir is not writable by this user", map[string]string{
			"dir": cfg.Server.DataDir, "uid": strconv.Itoa(os.Getuid()), "gid": strconv.Itoa(os.Getgid()),
			"hint": "fix volume/dir ownership on the host: bind-mount: chown -R $(id -u):$(id -g) ./data; named volume: docker run --rm -v <vol>:/data alpine chown -R $(id -u):$(id -g) /data  (or delete the volume to let it be re-created: docker compose down && docker volume rm <project>_taponvr-data)",
		})
		return 1
	}
	var dbfile = cfg.Server.DataDir + "/db.sqlite"
	store, derr := openDB(dbfile)
	if derr != nil {
		// SQLITE_CANTOPEN (14) almost always means the data dir is not
		// writable by this UID (named volume created by another UID, or a
		// root-owned ./data bind-mount). Say so explicitly with the fix.
		logIt(LERR, "failed to open database", map[string]string{
			"file": dbfile, "error": derr.Error(), "uid": strconv.Itoa(os.Getuid()),
			"hint": "data dir not writable: chown it to this UID (bind-mount: chown -R $(id -u):$(id -g) ./data; named volume: docker run --rm -v <vol>:/data --entrypoint sh golang:1.27-alpine -c 'chown -R 1000:1000 /data') or recreate the volume",
		})
		return 1
	}
	logIt(LINFO, "config loaded", map[string]string{"path": path, "cameras": strconv.Itoa(len(cfg.Cameras)), "port": strconv.Itoa(cfg.Server.Port)})
	for i := range cfg.Cameras {
		if !cfg.Cameras[i].Enabled {
			logIt(LWARN, "camera disabled (enabled:false), skipping record/onvif/rtsp/stream", map[string]string{"camera": cfg.Cameras[i].Name, "id": fmt.Sprintf("cam%d", i)})
		}
	}
	// Sync configured cameras into the metadata store.
	for i := range cfg.Cameras {
		var cam = cfg.Cameras[i]
		var ptz = 0
		if cam.Ptz != "off" {
			ptz = 1
		}
		if e := dbUpsertCam(store, &cam, fmt.Sprintf("cam%d", i), ptz); e != nil {
			logIt(LWARN, "camera upsert failed", map[string]string{"camera": cam.Name, "error": e.Error()})
		}
	}
	var app = initApp(cfg, store)
	if pw := cfg.Server.Auth.Password; pw == "" || pw == "changeme" || pw == "CHANGEME" || pw == "admin" || len(pw) < 8 {
		logIt(LWARN, "insecure auth: set server.auth.password", map[string]string{"hint": "use a strong password (>=8 chars, not admin/changeme) before production"})
	}
	// Initialize runtime toggles from config
	setRecordingEnabled(cfg.Record.Enabled)
	setS3UploadEnabled(cfg.Storage.Enabled)
	setSnapshotsEnabled(cfg.Record.Snapshots.Enabled)
	setPushoverEnabled(cfg.Alerts.Pushover.Enabled)
	initVision(&cfg.Vision, cfg.Vision.Enabled)
	// detection defaults to enabled; no config field yet
	if cfg.Alerts.Pushover.Enabled && (cfg.Alerts.Pushover.AppToken == "" || cfg.Alerts.Pushover.UserKey == "") {
		logIt(LWARN, "pushover enabled but app_token or user_key is empty; notifications will be skipped", nil)
	}
	if cfg.Storage.Enabled && (cfg.Storage.S3.Endpoint == "" || cfg.Storage.S3.Bucket == "") {
		logIt(LWARN, "storage enabled but s3 endpoint or bucket is empty; uploads will fail", nil)
	}
	if cfg.Vision.APIKey != "" && !(len(cfg.Vision.URL) >= 8 && (cfg.Vision.URL[:7] == "https:/" || cfg.Vision.URL[:8] == "https://")) {
		logIt(LWARN, "vision api_key sent over plain HTTP; use https or isolated LAN", nil)
	}
	if ep := cfg.Storage.S3.Endpoint; ep != "" && len(ep) >= 7 && ep[:7] == "http://" {
		lower := ep[7:]
		if !(len(lower) >= 9 && lower[:9] == "localhost") && !(len(lower) >= 9 && lower[:9] == "127.0.0.1") && cfg.Storage.S3.SecretKey != "" {
			logIt(LWARN, "s3 endpoint uses plain HTTP with secrets; use https or loopback", nil)
		}
	}
	// Boot retention must not delay the API: on a big/S3 archive the pass
	// takes minutes (Docker HEALTHCHECK would then crash-loop the container).
	// The daily loop still runs in the foreground on schedule.
	go pruneOnce(&app)
	startPruneLoop(&app)
	startStream(&app)
	buffersStart(&app)
	onvifEventsStart(&app)
	snapshotWarmupStart(&app)
	detectionRTSPStart(&app)
	initMqtt(&app)
	handleSignals()
	appDB = store
	var serr = serve(&app)
	stopMqtt()
	stopStream()
	store.Close()
	if serr != nil {
		logIt(LERR, "server failed", map[string]string{"error": serr.Error()})
		return 1
	}
	return 0
}

// runHealthcheck probes the local API for container HEALTHCHECK use.
// Accepts an optional "--config <path>" (default config.yaml) to find the
// port. Exit 0 when the server answers 200 or 401, 1 otherwise.
func runHealthcheck(args []string) int {
	var path = "config.yaml"
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			path = args[i+1]
		}
	}
	var port = 8971
	// Quiet: loadConfig warns on clamped values; a probe every 30s must not
	// spam docker logs.
	quietConfigLogs = true
	if cfg, err := loadConfig(path); err == nil && cfg.Server.Port != 0 {
		port = cfg.Server.Port
	}
	quietConfigLogs = false
	var client = &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 || resp.StatusCode == 401 {
		return 0
	}
	return 1
}

// appDB is closed on SIGTERM/SIGINT so the SQLite WAL is checkpointed
// instead of aborting via os.Exit with an open handle.
var appDB *DB

// handleSignals stops the child go2rtc process and exits when the server is
// terminated, so no orphaned go2rtc is left running.
func handleSignals() {
	var ch = make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		stopMqtt()
		stopStream()
		if appDB != nil {
			appDB.Close()
		}
		os.Exit(0)
	}()
}

// stopStream terminates the go2rtc child process if it is still running.
// The monitor goroutine in startStream owns cmd.Wait(); here we only signal it
// and wait for the reap, so the two never race on the same process state.
// It also disarms the restart supervisor, and loops so a restart racing the
// stop is killed too. Safe to call twice.
func stopStream() {
	streamCmdMtx.Lock()
	streamStopRequested = true
	streamCmdMtx.Unlock()
	// Disarm the supervisor first: cancel any pending relaunch timer and kill
	// the process context, so neither a scheduled retry nor a start racing
	// the stop can resurrect go2rtc after this returns.
	stopStreamRelaunchTimer()
	cancelStreamCtx()
	var deadline = time.Now().Add(10 * time.Second)
	for {
		streamCmdMtx.Lock()
		cmd := streamCmd
		done := streamCmdDone
		streamCmdMtx.Unlock()
		if cmd == nil {
			return
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
		streamCmdMtx.Lock()
		var still = streamCmd != nil
		streamCmdMtx.Unlock()
		if !still || time.Now().After(deadline) {
			return
		}
	}
}
