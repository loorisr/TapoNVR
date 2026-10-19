package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------- stream: go2rtc launcher + HLS proxy -------------

// streamCmd is the running go2rtc process (nil if not started). Guarded by
// streamCmdMtx: it is written by the launcher, cleared by the monitor
// goroutine, and read from the API/record/status paths.
var streamCmdMtx sync.Mutex
var streamCmd *exec.Cmd

// streamCmdDone is closed once the monitor goroutine has reaped the process,
// so shutdown can wait for the single owner of cmd.Wait() instead of racing it.
var streamCmdDone chan struct{}

// getStreamCmd returns the running go2rtc command (or nil).
func getStreamCmd() *exec.Cmd {
	streamCmdMtx.Lock()
	defer streamCmdMtx.Unlock()
	return streamCmd
}

// getStreamCmdPID returns the go2rtc PID (0 when not running).
func getStreamCmdPID() int {
	streamCmdMtx.Lock()
	defer streamCmdMtx.Unlock()
	if streamCmd != nil && streamCmd.Process != nil {
		return streamCmd.Process.Pid
	}
	return 0
}

// writeGo2rtcConfig writes a go2rtc config file wired to the configured
// cameras. It returns the config file path and any error. The file lives in
// the OS temp dir (regenerated every start): it must not take space in the
// data volume and must work when data_dir is read-only at startup.
func writeGo2rtcConfig(app *App) (string, error) {
	// Owner-only: the generated config carries rtsp://user:password@... URLs and
	// must not be readable by other accounts on a shared host.
	var dir = filepath.Join(os.TempDir(), "taponvr-go2rtc")
	if e := os.MkdirAll(dir, 0o700); e != nil {
		return "", e
	}
	_ = os.Chmod(dir, 0o700) // MkdirAll keeps the mode of a pre-existing dir
	var path = dir + "/go2rtc.yaml"
	var sb strings.Builder
	// Bind the API to loopback: only the in-process /stream/ proxy talks to
	// it, so it must not listen on all interfaces.
	sb.WriteString("api:\n  listen: \"127.0.0.1:" + strconv.Itoa(app.cfg.Stream.Port) + "\"\n  origin: \"*\"\n")
	sb.WriteString("log:\n  level: " + clampGo2rtcLogLevel(app.cfg.Stream.Go2rtcLogLevel) + "\n")
	// Expose the camera relay as RTSP on its own port so recording can pull the
	// re-broadcast instead of opening a second direct connection to the camera.
	// Bound to loopback: recording uses rtsp://127.0.0.1:port/camN internally,
	// so the unauthenticated relay is never exposed to the LAN.
	// NOTE: the go2rtc relay (RTSP/API/HLS ports) has no auth of its own by
	// design — restrict it at the network/firewall level. The taponvr /stream/
	// proxy path above remains gated by authOK.
	sb.WriteString("rtsp:\n  listen: \"127.0.0.1:" + strconv.Itoa(app.cfg.Stream.RtspPort) + "\"\n")
	sb.WriteString("webrtc:\n  enabled: true\n")
	sb.WriteString("  listen: :8555\n")
	// ICE servers are opt-in (stream.ice_servers, empty by default): no public
	// STUN is contacted unless configured, so there is no IP leak and isolated
	// LANs don't stall on unreachable Google servers.
	if servers := app.cfg.Stream.IceServers; len(servers) > 0 {
		sb.WriteString("  ice_servers:\n")
		for _, u := range servers {
			if strings.TrimSpace(u) == "" {
				continue
			}
			var esc = strings.ReplaceAll(u, "\\", "\\\\")
			esc = strings.ReplaceAll(esc, "\"", "\\\"")
			sb.WriteString("    - urls: [\"" + esc + "\"]\n")
		}
	}
	sb.WriteString("hls:\n  enabled: true\n")
	sb.WriteString("streams:\n")
	var enabledCams []string
	for i := range app.cfg.Cameras {
		var cam = app.cfg.Cameras[i]
		if !cam.Enabled || cam.RtspURL == "" {
			continue
		}
		var camID = "cam" + strconv.Itoa(i)
		// Two-way audio is never used by TapoNVR and the go2rtc API is bound to
		// loopback, so disable the RTSP backchannel on every source: go2rtc
		// recommends #backchannel=0 for glitchy TP-Link cameras, and the extra
		// ANNOUNCE/backchannel SETUP can wedge the camera's tiny session table.
		// A user-provided backchannel=... fragment is left untouched.
		var src = cam.RtspURL
		if !strings.Contains(src, "backchannel=") {
			src += "#backchannel=0"
		}
		// RTSP URLs carry credentials: escape backslash/quote so passwords
		// containing YAML metacharacters can't break the generated config.
		var esc = strings.ReplaceAll(src, "\\", "\\\\")
		esc = strings.ReplaceAll(esc, "\"", "\\\"")
		sb.WriteString("  " + camID + ": \"" + esc + "\"\n")
		enabledCams = append(enabledCams, camID)
	}
	if len(enabledCams) > 0 {
		sb.WriteString("preload:\n")
		for _, camID := range enabledCams {
			sb.WriteString("  " + camID + ":\n")
		}
	}
	if e := os.WriteFile(path, []byte(sb.String()), 0o600); e != nil {
		return "", e
	}
	return path, nil
}

// isUnsafeGo2rtcPath reports whether a go2rtc binary path contains shell
// metacharacters or parent-directory escapes. The path is passed to exec
// directly (no shell), but such values indicate config injection/confusion
// and are refused before any process is spawned.
func isUnsafeGo2rtcPath(bin string) bool {
	if strings.ContainsAny(bin, ";|&$`()") || strings.Contains(bin, "..") {
		return true
	}
	return strings.ContainsAny(filepath.Clean(bin), ";|&$`()\n")
}

// validateGo2rtcPath cleans a go2rtc binary path and resolves it: paths with
// separators must be regular executable files, bare names resolve via the
// local directory or PATH (the allowlist). Unsafe values fail outright.
func validateGo2rtcPath(bin string) (string, error) {
	var raw = strings.TrimSpace(bin)
	if raw == "" {
		return "", fmt.Errorf("empty go2rtc path")
	}
	if isUnsafeGo2rtcPath(raw) {
		return "", fmt.Errorf("unsafe go2rtc path %q (must be a plain path or binary name)", raw)
	}
	var cleaned = filepath.Clean(raw)
	var checkFile = func(p string) (string, error) {
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("go2rtc path %q is not a regular file", p)
		}
		if st.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("go2rtc path %q is not executable", p)
		}
		return p, nil
	}
	if strings.Contains(cleaned, "/") {
		return checkFile(cleaned)
	}
	// Bare binary name: local directory first, then PATH.
	if p, err := checkFile("./" + cleaned); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath(cleaned); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("go2rtc binary %q not found (absolute path or PATH name required)", raw)
}

// resolveGo2rtc returns the go2rtc binary path, falling back to the local
// directory when the configured value is a bare name not found on PATH.
// Unsafe values resolve to "" (rejected before exec).
func resolveGo2rtc(bin string) string {
	if isUnsafeGo2rtcPath(bin) {
		return ""
	}
	if p, err := validateGo2rtcPath(bin); err == nil {
		return p
	}
	if strings.Contains(bin, "/") {
		return bin
	}
	return bin
}

// streamStopRequested tells the monitor that the exit was intentional (stopStream)
// so a stopped process is never relaunched.
var streamStopRequested = false

// streamBackoff is the current restart delay (exponential, capped). Reset
// after a long healthy run.
var streamBackoff = 5 * time.Second

const streamBackoffMax = 5 * time.Minute

// streamCtxMtx guards the go2rtc process context: stopStream cancels it so a
// go2rtc start racing the stop is killed even if the kill loop misses it.
var streamCtxMtx sync.Mutex
var streamCtx = context.Background()
var streamCancel context.CancelFunc = func() {}

// newStreamCtx installs a fresh cancellable context for the next go2rtc
// generation and returns it.
func newStreamCtx() context.Context {
	streamCtxMtx.Lock()
	defer streamCtxMtx.Unlock()
	streamCtx, streamCancel = context.WithCancel(context.Background())
	return streamCtx
}

// streamCtxNow returns the current go2rtc process context.
func streamCtxNow() context.Context {
	streamCtxMtx.Lock()
	defer streamCtxMtx.Unlock()
	return streamCtx
}

// cancelStreamCtx cancels the current go2rtc process context (kills the child).
func cancelStreamCtx() {
	streamCtxMtx.Lock()
	defer streamCtxMtx.Unlock()
	if streamCancel != nil {
		streamCancel()
	}
}

// streamTimerMtx guards streamRelaunchTimer: the pending retry timer is
// tracked so stopStream can cancel it instead of leaving a dangling
// time.AfterFunc that fires after shutdown.
var streamTimerMtx sync.Mutex
var streamRelaunchTimer *time.Timer

// stopStreamRelaunchTimer cancels a pending go2rtc relaunch, if any.
func stopStreamRelaunchTimer() {
	streamTimerMtx.Lock()
	defer streamTimerMtx.Unlock()
	if streamRelaunchTimer != nil {
		streamRelaunchTimer.Stop()
		streamRelaunchTimer = nil
	}
}

// startStream launches go2rtc as a child process with the generated config.
// A monitor goroutine restarts it with backoff if it dies: without this,
// live view, recording relay and RTSP detection stay dead until a manual
// restart. stopStream performs the intentional shutdown.
func startStream(app *App) {
	streamCmdMtx.Lock()
	streamStopRequested = false
	streamBackoff = 5 * time.Second
	streamCmdMtx.Unlock()
	newStreamCtx()
	launchGo2rtc(app)
}

// launchGo2rtc starts one go2rtc process (or schedules a retry when the
// binary/config is unavailable). At most one live process exists at a time;
// the monitor owns cmd.Wait() for its own generation.
func launchGo2rtc(app *App) {
	streamCmdMtx.Lock()
	if streamStopRequested {
		streamCmdMtx.Unlock()
		return
	}
	streamCmdMtx.Unlock()
	var bin = resolveGo2rtc(app.cfg.Stream.Go2rtcPath)
	if bin == "" {
		if isUnsafeGo2rtcPath(app.cfg.Stream.Go2rtcPath) {
			logIt(LWARN, "invalid go2rtc path; live view disabled (fix stream.go2rtc_path and restart)", map[string]string{"binary": app.cfg.Stream.Go2rtcPath})
			return
		}
		return
	}
	var path, err = writeGo2rtcConfig(app)
	if err != nil {
		logIt(LWARN, "stream config write failed", map[string]string{"error": err.Error()})
		scheduleRelaunch(app)
		return
	}
	if st, e := os.Stat(bin); os.IsNotExist(e) {
		logIt(LWARN, "go2rtc binary missing; live view disabled, will retry", map[string]string{"binary": bin})
		scheduleRelaunch(app)
		return
	} else if e == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0) {
		logIt(LWARN, "go2rtc binary not a regular executable; live view disabled, will retry", map[string]string{"binary": bin})
		scheduleRelaunch(app)
		return
	}
	// CommandContext ties the child lifetime to the stream context: stopStream
	// cancels it, so the process is killed even if the kill loop below races.
	var cmd = exec.CommandContext(streamCtxNow(), bin, "-c", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stormWriter{w: os.Stderr, note: stormNoteLine}
	var started = time.Now()
	if e := cmd.Start(); e != nil {
		logIt(LWARN, "failed to start go2rtc; live view disabled, will retry", map[string]string{"binary": bin, "error": e.Error()})
		scheduleRelaunch(app)
		return
	}
	streamCmdMtx.Lock()
	if streamStopRequested {
		// Stopped while starting: kill the newcomer, no relaunch.
		streamCmdMtx.Unlock()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}
	streamCmd = cmd
	streamCmdDone = make(chan struct{})
	var done = streamCmdDone
	streamCmdMtx.Unlock()
	logIt(LINFO, "go2rtc started", map[string]string{"binary": bin, "port": strconv.Itoa(app.cfg.Stream.Port)})
	go monitorGo2rtc(app, cmd, done, started)
}

// scheduleRelaunch retries a failed start after the backoff delay (unless a
// stop was requested in between). A storm pause (see stormNoteLine) extends
// the delay so a wedged camera is not hammered.
func scheduleRelaunch(app *App) {
	streamCmdMtx.Lock()
	var bo = streamBackoff
	if bo < 5*time.Second {
		bo = 5 * time.Second
	}
	if bo > streamBackoffMax {
		bo = streamBackoffMax
	}
	streamBackoff = bo * 2
	if streamBackoff > streamBackoffMax {
		streamBackoff = streamBackoffMax
	}
	streamCmdMtx.Unlock()
	if pause := stormPauseRemaining(); pause > bo {
		bo = pause
	}
	streamTimerMtx.Lock()
	if streamRelaunchTimer != nil {
		streamRelaunchTimer.Stop()
	}
	streamRelaunchTimer = time.AfterFunc(bo, func() {
		streamTimerMtx.Lock()
		streamRelaunchTimer = nil
		streamTimerMtx.Unlock()
		streamCmdMtx.Lock()
		var stop = streamStopRequested
		var running = streamCmd != nil
		streamCmdMtx.Unlock()
		if stop || running {
			return
		}
		launchGo2rtc(app)
	})
	streamTimerMtx.Unlock()
}

// ---------------- go2rtc reconnect-storm supervisor ----------------
//
// go2rtc retries a failed producer with NO delay (its backoff only covers dial
// errors), so a TP-Link camera whose session table wedged gets hammered with
// dozens of connections per second until rebooted. The supervisor watches the
// go2rtc log stream for that signature and pauses the relay with exponential
// backoff, turning an endless storm into one retry per minute or less.

const (
	stormWindow    = 10 * time.Second
	stormThreshold = 40
)

// stormWriter fan-outs go2rtc stderr to the process stderr and to note().
type stormWriter struct {
	w    io.Writer
	note func(string)
	mtx  sync.Mutex
	buf  []byte
}

func (s *stormWriter) Write(p []byte) (int, error) {
	var n, err = s.w.Write(p)
	s.mtx.Lock()
	s.buf = append(s.buf, p...)
	for {
		var i = bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		if s.note != nil {
			s.note(string(s.buf[:i]))
		}
		s.buf = s.buf[i+1:]
	}
	if len(s.buf) > 8192 {
		s.buf = s.buf[len(s.buf)-8192:]
	}
	s.mtx.Unlock()
	return n, err
}

// stormState guards the reconnect-storm detector.
var stormState = struct {
	sync.Mutex
	lines  []time.Time
	pauses int
	until  time.Time
}{}

// stormNoteLine records one go2rtc log line. When the producer keeps dying
// (error=EOF / retry=) at a storm rate, the relay is paused and the current
// process killed; the monitor relaunches it only after the pause.
func stormNoteLine(line string) {
	if !strings.Contains(line, "error=EOF") && !strings.Contains(line, "[streams] retry=") {
		return
	}
	var now = time.Now()
	stormState.Lock()
	stormState.lines = append(stormState.lines, now)
	var cut = now.Add(-stormWindow)
	var keep = stormState.lines[:0]
	for _, t := range stormState.lines {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	stormState.lines = keep
	var trigger = len(stormState.lines) >= stormThreshold && !now.Before(stormState.until)
	var pause time.Duration
	if trigger {
		stormState.pauses++
		pause = 60 * time.Second << min(stormState.pauses-1, 3)
		if pause > 8*time.Minute {
			pause = 8 * time.Minute
		}
		stormState.until = now.Add(pause)
		stormState.lines = nil
	}
	stormState.Unlock()
	if !trigger {
		return
	}
	logIt(LWARN, "go2rtc producer reconnect storm: pausing the relay; the camera is likely wedged (reboot it)", map[string]string{"pause": pause.String()})
	killStreamProcess()
}

// stormPauseRemaining returns the time left in the current storm pause.
func stormPauseRemaining() time.Duration {
	stormState.Lock()
	defer stormState.Unlock()
	return time.Until(stormState.until)
}

// stormReset clears the detector after a long healthy run so a one-off storm
// does not shorten future pauses.
func stormReset() {
	stormState.Lock()
	stormState.lines = nil
	stormState.pauses = 0
	stormState.until = time.Time{}
	stormState.Unlock()
}

// killStreamProcess kills the current go2rtc process, if any. The monitor
// goroutine owns the reap and will schedule the relaunch (bounded by any storm
// pause in scheduleRelaunch).
func killStreamProcess() {
	streamCmdMtx.Lock()
	var cmd = streamCmd
	streamCmdMtx.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// monitorGo2rtc reaps one go2rtc generation. Intentional stops clear the
// handle; crashes swap in a backoff-delayed replacement.
func monitorGo2rtc(app *App, cmd *exec.Cmd, done chan struct{}, started time.Time) {
	// Single owner of cmd.Wait(): stopStream only kills and waits on `done`.
	_ = cmd.Wait()
	var lived = time.Since(started)
	streamCmdMtx.Lock()
	var stop = streamStopRequested
	if lived > 5*time.Minute {
		streamBackoff = 5 * time.Second
	}
	if stop {
		if streamCmd == cmd {
			streamCmd = nil
			streamCmdDone = nil
		}
		streamCmdMtx.Unlock()
		close(done)
		logIt(LINFO, "go2rtc stopped", map[string]string{})
		return
	}
	if lived > 10*time.Minute {
		// Healthy for a while: allow future storm pauses to start short again.
		stormReset()
	}
	if streamCmd == cmd {
		streamCmd = nil
		// streamCmdDone is replaced by the next launch; close the old one so
		// nobody waits on a dead generation.
		streamCmdDone = nil
	}
	streamCmdMtx.Unlock()
	close(done)
	logIt(LWARN, "go2rtc exited unexpectedly; restarting with backoff", map[string]string{"lived": lived.String()})
	scheduleRelaunch(app)
}

// streamProxyReverse returns a WebSocket-capable reverse proxy that maps our
// /stream/ prefix onto the go2rtc /api/ endpoints (HLS manifests, segments,
// and the WebSocket WebRTC signaling channel).
func streamProxyReverse(app *App) *httputil.ReverseProxy {
	var target = &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", app.cfg.Stream.Port)}
	var rp = httputil.NewSingleHostReverseProxy(target)
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeJSON(w, 502, "{\"error\":\""+escapeJSON("stream unavailable; is go2rtc running?")+"\"}")
	}
	// Rewrite /stream/X -> /api/X on go2rtc.
	var director = rp.Director
	rp.Director = func(req *http.Request) {
		director(req)
		req.URL.Path = "/api/" + strings.TrimPrefix(req.URL.Path, "/stream/")
	}
	return rp
}

// streamProxy is built at startup.
var streamProxy *httputil.ReverseProxy

// initStreamProxy wires the /stream/ reverse proxy for the app instance.
func initStreamProxy(app *App) {
	streamProxy = streamProxyReverse(app)
}

// handleStream serves the /stream/ prefix through the go2rtc reverse proxy.
func handleStream(w http.ResponseWriter, r *http.Request) {
	if streamProxy != nil {
		streamProxy.ServeHTTP(w, r)
		return
	}
	writeJSON(w, 502, "{\"error\":\"stream not initialized\"}")
}
