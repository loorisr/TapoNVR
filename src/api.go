package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtp"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------- api: HTTP server -----------------------------

// cameraHost returns the camera's host/IP only - never its credentials. The
// full rtsp_url embeds user:password and must not leave the server; the UI only
// needs the host to label a camera.
func cameraHost(c *CameraCfg) string {
	if c == nil {
		return ""
	}
	for _, raw := range []string{c.Onvif.Url, c.RtspURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		var host = u.Host
		if strings.Contains(host, ":") {
			if h, _, e := net.SplitHostPort(host); e == nil {
				host = h
			}
		}
		if host != "" && host != "0.0.0.0" && host != "127.0.0.1" {
			return host
		}
	}
	return ""
}

func jsonCamString(c *CameraCfg, id string, ptz int) string {
	var arr []string
	arr = append(arr, kv("id", id))
	arr = append(arr, kv("name", c.Name))
	arr = append(arr, `"enabled":`+boolJSON(c.Enabled))
	// host only: rtsp_url carries the camera password and is deliberately not
	// exposed to the browser.
	arr = append(arr, kv("host", cameraHost(c)))
	arr = append(arr, `"stream":"/stream/ws?src=`+id+`"`)
	arr = append(arr, `"ptz_available":`+strconv.Itoa(ptz))
	return "{" + strings.Join(arr, ",") + "}"
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func detJSON(d *Detection) string {
	var arr []string
	arr = append(arr, kv("id", d.id))
	arr = append(arr, kv("camera_id", d.cameraID))
	arr = append(arr, kv("camera_name", d.camName))
	arr = append(arr, kv("type", d.detType))
	// types is the full merged tag list (primary first), e.g. ["person","motion"].
	var tagJSON []string
	for _, t := range detectionTags(d) {
		tagJSON = append(tagJSON, jsonString(t))
	}
	arr = append(arr, `"types":[`+strings.Join(tagJSON, ",")+`]`)
	arr = append(arr, `"confidence":`+fmt.Sprintf("%.2f", d.confidence))
	arr = append(arr, `"started_at":`+fmt.Sprintf("%d", d.startedAt))
	arr = append(arr, `"ended_at":`+fmt.Sprintf("%d", d.endedAt))
	var duration = d.endedAt - d.startedAt
	if duration < 0 {
		duration = 0
	}
	arr = append(arr, `"duration":`+fmt.Sprintf("%d", duration))
	// Total media size: cached at write time, computed live for old rows.
	var totalSize = d.sizeBytes
	if totalSize <= 0 {
		totalSize = detectionSize(d)
	}
	arr = append(arr, `"size":`+fmt.Sprintf("%d", totalSize))
	var snapURL string
	if d.snapshotPath != "" {
		snapURL = "/api/detections/" + d.id + "/snapshot"
	}
	arr = append(arr, kv("snapshot", snapURL))
	arr = append(arr, kv("clip", detectionClipURL(d)))
	// True while a buffer-mode clip is being assembled: the Video button can
	// show "assembling…" instead of looking broken.
	arr = append(arr, `"clip_pending":`+boolJSON(isClipPending(d.id)))
	// All snapshots for the detection (primary first, then timed extras).
	var snaps []string
	if snapURL != "" {
		snaps = append(snaps, snapURL)
	}
	if d.extraSnap != "" {
		var extras = strings.Split(d.extraSnap, ",")
		for k := range extras {
			if extras[k] == "" {
				continue
			}
			snaps = append(snaps, "/api/detections/"+d.id+"/snapshot/"+strconv.Itoa(k+1))
		}
	}
	var quoted []string
	for _, u := range snaps {
		quoted = append(quoted, jsonString(u))
	}
	arr = append(arr, `"snapshots":[`+strings.Join(quoted, ",")+`]`)
	arr = append(arr, `"locked":`+boolJSON(d.locked != 0))
	arr = append(arr, `"uploaded":`+boolJSON(d.uploaded != 0))
	arr = append(arr, `"s3_expected":`+boolJSON(d.s3Expected != 0))
	if bbox, ok := sanitizeBboxJSON(d.bboxJSON); ok {
		arr = append(arr, `"bbox":`+bbox)
	} else {
		arr = append(arr, `"bbox":null`)
	}
	arr = append(arr, visionJSON(d))
	return "{" + strings.Join(arr, ",") + "}"
}

// visionJSON renders the YOLO summary for a detection. The full boxes array
// lives in d.visionJSON (complete via GET /api/detections/{id}/vision); the
// list payload carries a summary plus the top 10 boxes to stay light.
func visionJSON(d *Detection) string {
	if d == nil {
		return `"vision":null`
	}
	var dets = strings.TrimSpace(d.visionJSON)
	if dets == "" {
		dets = "[]"
	}
	// Truncate embedded boxes to the top 10 (by stored order = conf desc).
	// Validate: corrupt DB JSON must not break the whole list response.
	var short = validJSONArrayOrEmpty(dets)
	if short != "[]" {
		var arr []json.RawMessage
		if json.Unmarshal([]byte(short), &arr) == nil && len(arr) > 10 {
			var top []string
			for _, m := range arr[:10] {
				top = append(top, string(m))
			}
			short = "[" + strings.Join(top, ",") + "]"
		}
	}
	return `"vision":{"count":` + strconv.Itoa(d.visionCount) +
		`,"top":` + jsonString(d.visionTop) +
		`,"ms":` + fmt.Sprintf("%.1f", d.visionMs) +
		`,"at":` + strconv.FormatInt(d.visionAt, 10) +
		`,"error":` + jsonString(d.visionError) +
		`,"detections":` + short + `}`
}

// validJSONArrayOrEmpty returns s when it is valid JSON array/object, else "[]".
func validJSONArrayOrEmpty(s string) string {
	var t = strings.TrimSpace(s)
	if t == "" {
		return "[]"
	}
	var js json.RawMessage
	if json.Unmarshal([]byte(t), &js) != nil {
		return "[]"
	}
	return t
}

// detectionClipURL returns the clip API path only when a clip file actually
// exists on disk. Otherwise it returns "" so clients (Video buttons/tabs,
// MQTT consumers) can treat the recording as snapshot-only instead of
// advertising a link that would 404 (e.g. while a buffer-mode clip is still
// being assembled, or when recording is disabled).
func detectionClipURL(d *Detection) string {
	if d == nil || d.clipPath == "" {
		return ""
	}
	if s, e := os.Stat(d.clipPath); e != nil || s.Size() == 0 {
		return ""
	}
	return "/api/detections/" + d.id + "/clip"
}

// secureHeaders sets baseline security headers on every API/JSON response.
// Cookies set elsewhere should use SameSite=Lax (note: Basic-auth API has no
// session cookies; this is for any future cookie use).
func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	secureHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// authFailMtx bounds per-IP 401 bursts: 10 failures/min per IP, then 429.
type authFailEntry struct {
	n     int
	since time.Time
}

var authFailMtx sync.Mutex
var authFailMap = map[string]authFailEntry{}

// authOpenWarned logs the empty-password open-auth warning only once.
var authOpenWarnedMtx sync.Mutex
var authOpenWarned = false

// authRateLimited reports whether ip exceeded 10 failures in the last minute.
func authRateLimited(ip string) bool {
	authFailMtx.Lock()
	defer authFailMtx.Unlock()
	var e = authFailMap[ip]
	if time.Since(e.since) > time.Minute {
		return false
	}
	return e.n >= 10
}

func authNoteFailure(ip string) {
	authFailMtx.Lock()
	var e = authFailMap[ip]
	if time.Since(e.since) > time.Minute {
		e = authFailEntry{0, time.Now()}
	}
	e.n++
	e.since = e.since.UTC()
	if e.since.IsZero() {
		e.since = time.Now()
	}
	authFailMap[ip] = e
	authFailMtx.Unlock()
}

func authClientIP(r *http.Request) string {
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && ip != "" {
		return ip
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

// authOK checks HTTP Basic credentials against config (constant-time
// comparison so usernames/passwords aren't enumerable via timing).
// NOTE: an empty server.auth.password leaves the API open (backward compat
// for existing deployments, ideally behind a reverse proxy). Set
// server.auth.disabled=true to acknowledge it explicitly and silence the
// once-per-process WARN below.
func authOK(app *App, w http.ResponseWriter, r *http.Request) bool {
	if app.cfg.Server.Auth.Password == "" {
		if !app.cfg.Server.Auth.Disabled {
			authOpenWarnedMtx.Lock()
			var warn = !authOpenWarned
			authOpenWarned = true
			authOpenWarnedMtx.Unlock()
			if warn {
				logIt(LWARN, "auth open: server.auth.password is empty, API is unauthenticated", map[string]string{"hint": "set server.auth.password or server.auth.disabled=true behind reverse proxy"})
			}
		}
		return true
	}
	var u, p, ok = r.BasicAuth()
	if ok && subtle.ConstantTimeCompare([]byte(u), []byte(app.cfg.Server.Auth.Username)) == 1 && subtle.ConstantTimeCompare([]byte(p), []byte(app.cfg.Server.Auth.Password)) == 1 {
		return true
	}
	var ip = authClientIP(r)
	if authRateLimited(ip) {
		secureHeaders(w)
		w.WriteHeader(429)
		_, _ = w.Write([]byte("rate limited"))
		return false
	}
	authNoteFailure(ip)
	secureHeaders(w)
	w.Header().Set("WWW-Authenticate", "Basic realm=\"taponvr\"")
	w.WriteHeader(401)
	_, _ = w.Write([]byte("unauthorized"))
	return false
}

// serve starts the HTTP server and blocks.
func serve(app *App) error {
	initStreamProxy(app)
	var mux = http.NewServeMux()
	mux.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		// Live streams are camera footage: same credentials as the UI. The
		// browser sends cached Basic credentials on the WebSocket upgrade
		// automatically, so video-rtc.js needs no changes.
		if !authOK(app, w, r) {
			return
		}
		handleStream(w, r)
	})
	// ONVIF cameras cannot do Basic auth: this endpoint stays open but is
	// gated by a shared-secret path token inside handleOnvifNotify.
	mux.HandleFunc("/onvif/notify", func(w http.ResponseWriter, r *http.Request) {
		handleOnvifNotify(app, w, r)
	})
	mux.HandleFunc("/onvif/notify/", func(w http.ResponseWriter, r *http.Request) {
		handleOnvifNotify(app, w, r)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		// Detection SSE leaks camera names/types: same credentials as the UI.
		// EventSource sends cached Basic credentials on same-origin requests.
		if !authOK(app, w, r) {
			return
		}
		handleEvents(app, w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handle(app, w, r)
	})
	// server.host is honored when set (empty or 0.0.0.0 = all interfaces).
	var host = strings.TrimSpace(app.cfg.Server.Host)
	if host == "" || host == "0.0.0.0" || host == "[IP_ADDRESS]" {
		host = ""
	}
	var addr = net.JoinHostPort(host, strconv.Itoa(app.cfg.Server.Port))
	logIt(LINFO, "http server listening", map[string]string{"addr": addr})
	// Explicit timeouts: without them a slow (or malicious) client can pin
	// goroutines and sockets indefinitely, and /onvif/notify is reachable
	// without credentials. WriteTimeout stays 0 on purpose: the SSE stream and
	// the clip/snapshot endpoints legitimately write for longer than any fixed
	// write deadline would allow.
	var srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.ListenAndServe()
}

// handle dispatches API and static requests.
// csrfOK rejects cross-site state-changing requests. Browsers attach
// Sec-Fetch-Site / Origin to every request; a foreign origin can otherwise make
// the browser replay its cached Basic credentials on our state-changing
// endpoints (POST /api/.../state, DELETE /api/detections/...). Requests that
// carry neither header (curl, automation using Basic auth) are allowed: they
// cannot be triggered by a web page. When both are absent, an X-Requested-With
// header (AJAX) is also accepted; JSON automation without any of these headers
// is still allowed so curl keeps working.
// NOTE: any future session cookies should use SameSite=Lax/Strict.
func csrfOK(r *http.Request) bool {
	switch r.Method {
	case "GET", "HEAD", "OPTIONS":
		return true
	}
	if site := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); site != "" {
		return site == "same-origin" || site == "none"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err == nil && u.Host != "" {
			return strings.EqualFold(u.Host, r.Host)
		}
		return false
	}
	// Neither Sec-Fetch-Site nor Origin: could be curl/automation or an old
	// browser. X-Requested-With marks AJAX; JSON bodies mark automation.
	if r.Header.Get("X-Requested-With") != "" {
		return true
	}
	if ct := r.Header.Get("Content-Type"); strings.Contains(strings.ToLower(ct), "json") {
		return true
	}
	return true
}

func handle(app *App, w http.ResponseWriter, r *http.Request) {
	var path = r.URL.Path
	if !authOK(app, w, r) {
		return
	}
	if !csrfOK(r) {
		writeJSON(w, 403, "{\"error\":\"cross-site request rejected\"}")
		return
	}
	if path == "/api/cameras" {
		if r.Method != "GET" {
			methodNotAllowed(w)
			return
		}
		var arr []string
		var ptz = 0
		for i := range app.cfg.Cameras {
			var cam = app.cfg.Cameras[i]
			if cam.Ptz != "off" {
				ptz = 1
			} else {
				ptz = 0
			}
			arr = append(arr, jsonCamString(&cam, fmt.Sprintf("cam%d", i), ptz))
		}
		writeJSON(w, 200, "["+strings.Join(arr, ",")+"]")
		return
	}
	if path == "/api/status" {
		var body = "{" + kv("uptime", strconv.Itoa(int(time.Now().Unix()-app.boot))) + "}"
		writeJSON(w, 200, body)
		return
	}
	if strings.HasPrefix(path, "/api/detections/") && strings.HasSuffix(path, "/lock") {
		handleDetectionLock(app, w, r, path)
		return
	}
	if (path == "/api/detections" || strings.HasPrefix(path, "/api/detections/")) && r.Method == "DELETE" {
		handleDetectionDelete(app, w, r, path)
		return
	}
	if path == "/api/detections" {
		handleDetections(app, w, r)
		return
	}
	if path == "/api/onvif_events" {
		handleOnvifEvents(app, w, r)
		return
	}
	if strings.HasPrefix(path, "/api/onvif_events/") {
		handleOnvifEventDelete(app, w, r, path)
		return
	}
	if strings.HasPrefix(path, "/api/detections/") && (strings.HasSuffix(path, "/snapshot") || strings.HasSuffix(path, "/clip") || strings.Contains(path, "/snapshot/")) {
		handleDetectionMedia(app, w, r, path)
		return
	}
	if path == "/api/detection/state" {
		handleDetectionState(app, w, r)
		return
	}
	if path == "/api/recording/state" {
		handleRecordingState(app, w, r)
		return
	}
	if path == "/api/s3/state" {
		handleS3State(app, w, r)
		return
	}
	if path == "/api/snapshots/state" {
		handleSnapshotsState(app, w, r)
		return
	}
	if path == "/api/pushover/state" {
		handlePushoverState(app, w, r)
		return
	}
	if path == "/api/s3/retry" {
		handleS3Retry(app, w, r)
		return
	}
	if path == "/api/mqtt/status" {
		handleMqttStatus(app, w, r)
		return
	}
	if path == "/api/vision/status" {
		handleVisionStatus(app, w, r)
		return
	}
	if path == "/api/vision/state" {
		handleVisionState(app, w, r)
		return
	}
	if strings.HasPrefix(path, "/api/detections/") && strings.HasSuffix(path, "/vision") {
		handleDetectionVision(app, w, r, path)
		return
	}
	if path == "/api/system/status" {
		handleSystemStatus(app, w, r)
		return
	}
	if path == "/api/system/backup" {
		handleSystemBackup(app, w, r)
		return
	}
	if strings.HasPrefix(path, "/api/detections/") && strings.HasSuffix(path, "/timeline") {
		handleDetectionTimeline(app, w, r, path)
		return
	}
	if strings.HasPrefix(path, "/api/detections/") && strings.HasSuffix(path, "/vision/exclude") {
		handleVisionExclude(app, w, r, path)
		return
	}
	if strings.HasPrefix(path, "/api/cameras/") && strings.HasSuffix(path, "/detection") {
		handleCameraDetection(app, w, r, path)
		return
	}
	if path == "/api/test/detection" && r.Method == "POST" {
		runTestDetection(app, w)
		return
	}
	if strings.HasPrefix(path, "/api/cameras/") && strings.HasSuffix(path, "/ptz") {
		handlePTZ(app, w, r, path)
		return
	}
	if strings.HasPrefix(path, "/api/cameras/") && strings.HasSuffix(path, "/snapshot") {
		handleCameraSnapshot(app, w, r, path)
		return
	}
	if path == "/api/cameras/record" {
		handleCamerasRecordStatus(app, w, r)
		return
	}
	if strings.HasPrefix(path, "/api/cameras/") && strings.HasSuffix(path, "/record") {
		handleCameraRecord(app, w, r, path)
		return
	}
	if strings.HasPrefix(path, "/api/manual/jobs/") {
		handleManualJob(w, r, path)
		return
	}
	if strings.HasPrefix(path, "/api/manual/") {
		handleManualSnapshot(app, w, r, path)
		return
	}
	if strings.HasPrefix(path, "/api") {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	serveStatic(app, w, r, path)
}

func methodNotAllowed(w http.ResponseWriter) {
	w.WriteHeader(405)
	_, _ = w.Write([]byte("method not allowed"))
}

// handlePTZ handles POST /api/cameras/{id}/ptz?action=move|stop|home
// plus pan/tilt/zoom velocities (range -1..1) for the move action.
func handlePTZ(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) < 4 {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	var idx int
	var found = false
	for i := range app.cfg.Cameras {
		if fmt.Sprintf("cam%d", i) == id {
			idx, found = i, true
			break
		}
	}
	if !found {
		writeJSON(w, 404, "{\"error\":\"no such camera\"}")
		return
	}
	var cam = &app.cfg.Cameras[idx]
	var q = r.URL.Query()
	var action = q.Get("action")
	if action == "" {
		if q.Get("stop") == "1" || q.Get("stop") == "true" {
			action = "stop"
		}
	}
	// Velocities are documented as -1..1: clamp so a bad client cannot ask the
	// camera for an out-of-range speed.
	var f = func(name string, def float64) float32 {
		var v = q.Get(name)
		if v == "" {
			return float32(def)
		}
		var fv, err = strconv.ParseFloat(v, 32)
		if err != nil {
			return float32(def)
		}
		if fv < -1 {
			fv = -1
		}
		if fv > 1 {
			fv = 1
		}
		return float32(fv)
	}
	var err error
	switch action {
	case "move":
		err = onvifPTZMove(cam, f("pan", 0), f("tilt", 0))
	case "stop":
		err = onvifPTZStop(cam)
	case "home":
		err = onvifPTZHome(cam)
	default:
		writeJSON(w, 400, "{\"error\":\"action must be move, stop or home\"}")
		return
	}
	if err != nil {
		writeJSON(w, 502, "{\"error\":"+jsonString(err.Error())+"}")
		return
	}
	writeJSON(w, 200, "{\"ok\":true}")
}

// handleCameraSnapshot captures a manual snapshot for a camera.
// POST /api/cameras/{id}/snapshot -> {ok:true, snapshot:"/api/manual/..."}
func handleCameraSnapshot(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) < 5 {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	var idx int
	var found = false
	for i := range app.cfg.Cameras {
		if fmt.Sprintf("cam%d", i) == id {
			idx, found = i, true
			break
		}
	}
	if !found {
		writeJSON(w, 404, "{\"error\":\"no such camera\"}")
		return
	}
	var cam = &app.cfg.Cameras[idx]
	var camID = id
	var dir = app.cfg.Server.DataDir + "/manual/" + camID
	_ = ensureDir(dir)
	var now = time.Now()
	var fname = friendlyTS(now.Unix()) + "-" + newID() + "-manual.jpg"
	var dest = dir + "/" + fname
	var source, ok = grabSnapshotChain(app, camID, dest)
	if !ok {
		source = ""
	}
	// verify file exists
	if ok {
		if s, e := os.Stat(dest); e != nil || s.Size() == 0 {
			ok = false
		}
	}
	if !ok {
		// fallback to placeholder so button never hard-fails (useful when camera offline or go2rtc down)
		_ = os.Remove(dest)
		_ = writeFileBestEffort(dest, snapshotJpg(), 0o640)
		if s, e := os.Stat(dest); e == nil && s.Size() > 0 {
			ok = true
			source = "placeholder"
			snapRecord(camID, "placeholder", true, "live sources failed; placeholder served")
			logIt(LWARN, "snapshot fallback to placeholder", map[string]string{"camera": cam.Name, "path": dest})
		}
	}
	if !ok {
		_ = os.Remove(dest)
		writeJSON(w, 502, "{\"error\":\"snapshot failed\"}")
		return
	}
	logIt(LINFO, "manual snapshot captured", map[string]string{"camera": cam.Name, "path": dest, "source": source})
	// also create a detection so it appears in Recordings tab
	nowMs := time.Now().Unix()
	d := Detection{
		id:           newID(),
		cameraID:     camID,
		camName:      cam.Name,
		detType:      "manual-snapshot",
		confidence:   1.0,
		startedAt:    nowMs,
		endedAt:      nowMs,
		snapshotPath: dest,
		clipPath:     "",
	}
	d.sizeBytes = detectionSize(&d)
	if s3UploadEnabled() {
		d.s3Expected = 1
	}
	if err := dbInsertDetection(app.db, &d); err != nil {
		logIt(LERR, "db insert failed", map[string]string{"error": err.Error()})
		writeJSON(w, 500, "{\"error\":\"db\"}")
		return
	}
	if s3UploadEnabled() {
		// Best-effort immediate upload; failures stay pending for /api/s3/retry.
		s3RetryOne(app, &d)
	}
	// Vision: same async YOLO analysis as event snapshots (index 0 = primary).
	// No-op when vision is disabled; degraded-silent on API failure.
	queueVisionForDetection(app, d.id, dest)
	// also insert raw event for Event list
	var mev = OnvifEvent{id: newID(), cameraID: camID, camName: cam.Name, detType: "manual-snapshot", createdAt: nowMs, detectionID: d.id}
	if err := dbInsertOnvifEvent(app.db, &mev); err != nil {
		logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
	}
	mqttPublishDetection(&d)
	mqttPublishEvent(camID, cam.Name, "manual-snapshot", d.id, mev.id, true)
	var snapURL = "/api/manual/" + camID + "/" + fname
	writeJSON(w, 200, "{"+kvRaw("ok", "true")+","+kv("snapshot", snapURL)+","+kv("detection_id", d.id)+"}")
}

// handleCamerasRecordStatus reports toggle-recording state for every camera in
// one call so the UI doesn't poll N per-camera endpoints every 3 s.
// GET /api/cameras/record -> {"ok":true,"cameras":{"cam0":{"recording":bool,"started_at":int},...}}
func handleCamerasRecordStatus(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	var pairs []string
	manualRecMtx.Lock()
	for i := range app.cfg.Cameras {
		var camID = fmt.Sprintf("cam%d", i)
		var inner string
		if st := manualRecMap[camID]; st != nil {
			inner = `"recording":true,"started_at":` + strconv.FormatInt(st.start, 10)
		} else {
			inner = `"recording":false,"started_at":0`
		}
		pairs = append(pairs, jsonString(camID)+`:{`+inner+`}`)
	}
	manualRecMtx.Unlock()
	writeJSON(w, 200, `{"ok":true,"cameras":{`+strings.Join(pairs, ",")+`}}`)
}

// handleCameraRecord toggles manual video recording for a camera.
// GET  /api/cameras/{id}/record -> {recording:bool, started_at:int}
// POST /api/cameras/{id}/record -> start if not recording, stop if recording
// POST /api/cameras/{id}/record?duration=10 -> legacy fixed-duration (non-toggle) for compat
func handleCameraRecord(app *App, w http.ResponseWriter, r *http.Request, path string) {
	var seg = strings.Split(path, "/")
	if len(seg) < 5 {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	var idx int
	var found = false
	for i := range app.cfg.Cameras {
		if fmt.Sprintf("cam%d", i) == id {
			idx, found = i, true
			break
		}
	}
	if !found {
		writeJSON(w, 404, "{\"error\":\"no such camera\"}")
		return
	}
	var cam = &app.cfg.Cameras[idx]
	var camID = id

	if r.Method == "GET" {
		manualRecMtx.Lock()
		st, ok := manualRecMap[camID]
		manualRecMtx.Unlock()
		if ok && st != nil {
			writeJSON(w, 200, "{\"recording\":true,\"started_at\":"+strconv.FormatInt(st.start, 10)+"}")
		} else {
			writeJSON(w, 200, "{\"recording\":false}")
		}
		return
	}
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	// legacy fixed duration mode: ?duration=10 still works as one-shot
	if durStr := r.URL.Query().Get("duration"); durStr != "" || r.URL.Query().Get("seconds") != "" {
		if durStr == "" {
			durStr = r.URL.Query().Get("seconds")
		}
		if v, err := strconv.Atoi(durStr); err == nil && v > 0 && v <= 120 {
			// If currently recording, stop first. stopManualRecord is atomic
			// (it reports errNotRecording when there is nothing to stop), so
			// there is no check-then-act window here.
			if clip, serr := stopManualRecord(app, camID); serr == nil && clip != "" {
				writeJSON(w, 200, "{"+kvRaw("ok", "true")+","+kv("clip", clip)+",\"recording\":false}")
				return
			} else if serr != nil && !errors.Is(serr, errNotRecording) {
				writeJSON(w, 502, "{\"error\":\"stop failed: "+escapeJSON(serr.Error())+"\"}")
				return
			}
			// Async: recordClipGo blocks up to 120s and must not pin the HTTP
			// handler. Run in background, return 202 with a job id; clients
			// poll GET /api/manual/jobs/{id} or the detection list.
			var job = &manualJob{id: newID(), camID: camID, status: "running"}
			manualJobsMtx.Lock()
			manualJobs[job.id] = job
			manualJobsMtx.Unlock()
			var camCopy = *cam
			go runLegacyManualRecord(app, job, &camCopy, camID, v)
			logIt(LINFO, "manual record started (async legacy)", map[string]string{"camera": cam.Name, "job": job.id, "duration": strconv.Itoa(v)})
			writeJSON(w, 202, "{"+kvRaw("ok", "true")+","+kvRaw("async", "true")+","+kv("job_id", job.id)+","+kv("status_url", "/api/manual/jobs/"+job.id)+"}")
			return
		}
		// ?duration= was given but invalid (not 1..120): reject instead of
		// silently falling through to toggle mode (state-changing surprise).
		writeJSON(w, 400, "{\"error\":\"bad duration (1..120 seconds)\"}")
		return
	}
	// toggle mode: click to start, click again to stop
	if clip, serr := stopManualRecord(app, camID); serr == nil {
		writeJSON(w, 200, "{"+kvRaw("ok", "true")+","+kv("clip", clip)+",\"recording\":false}")
		return
	} else if !errors.Is(serr, errNotRecording) {
		writeJSON(w, 502, "{\"error\":\"stop failed: "+escapeJSON(serr.Error())+"\"}")
		return
	}
	// start new toggle recording
	_, err := startManualRecord(app, camID, cam.Name)
	if err != nil {
		writeJSON(w, 502, "{\"error\":\"start failed: "+escapeJSON(err.Error())+"\"}")
		return
	}
	writeJSON(w, 200, "{\"ok\":true,\"recording\":true,\"started_at\":"+strconv.FormatInt(time.Now().Unix(), 10)+"}")
}

// manualJobs tracks async legacy fixed-duration recordings (?duration=).
// The handler returns 202 immediately so a 120s record never pins the HTTP
// handler; clients poll GET /api/manual/jobs/{id} or the detection list.
type manualJob struct {
	id          string
	camID       string
	status      string // "running", "done", "error"
	clip        string
	detectionID string
	errMsg      string
}

var manualJobsMtx sync.Mutex
var manualJobs = map[string]*manualJob{}

// writeFileBestEffort writes data, logging on failure.
func writeFileBestEffort(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// handleManualJob serves GET /api/manual/jobs/{id} -> job status.
func handleManualJob(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	var id = strings.TrimPrefix(path, "/api/manual/jobs/")
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	manualJobsMtx.Lock()
	var job = manualJobs[id]
	var copyJob manualJob
	if job != nil {
		copyJob = *job
	}
	manualJobsMtx.Unlock()
	if job == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	var body = "{" + kv("job_id", copyJob.id) + "," + kv("status", copyJob.status) +
		"," + kv("clip", copyJob.clip) + "," + kv("detection_id", copyJob.detectionID) +
		"," + kv("error", copyJob.errMsg) + "}"
	writeJSON(w, 200, body)
}

// runLegacyManualRecord performs the blocking record+insert for an async job.
func runLegacyManualRecord(app *App, job *manualJob, cam *CameraCfg, camID string, durSec int) {
	var dir = app.cfg.Server.DataDir + "/manual/" + camID
	_ = ensureDir(dir)
	var now = time.Now()
	var fname = friendlyTS(now.Unix()) + "-" + newID() + "-manual.mp4"
	var dest = dir + "/" + fname
	src := recordingsSource(app, cam, camID)
	var fail = func(msg string) {
		_ = os.Remove(dest)
		manualJobsMtx.Lock()
		job.status = "error"
		job.errMsg = msg
		manualJobsMtx.Unlock()
		logIt(LWARN, "manual record failed", map[string]string{"camera": cam.Name, "error": msg})
	}
	if src == "" {
		fail("no rtsp url")
		return
	}
	if err := recordClipGo(src, durSec, dest); err != nil {
		fail("record failed: " + err.Error())
		return
	}
	if s, e := os.Stat(dest); e != nil || s.Size() == 0 {
		fail("record empty")
		return
	}
	nowMs := time.Now().Unix()
	d := Detection{id: newID(), cameraID: camID, camName: cam.Name, detType: "manual-video", confidence: 1.0, startedAt: now.Unix(), endedAt: nowMs, snapshotPath: "", clipPath: dest}
	tmpSnap := dir + "/" + friendlyTS(now.Unix()) + "-" + d.id + "-manual-thumb.jpg"
	if _, ok := grabSnapshotChain(app, camID, tmpSnap); ok {
		d.snapshotPath = tmpSnap
	} else {
		_ = writeFileBestEffort(tmpSnap, snapshotJpg(), 0o640)
		d.snapshotPath = tmpSnap
	}
	d.sizeBytes = detectionSize(&d)
	if s3UploadEnabled() {
		d.s3Expected = 1
	}
	if err := dbInsertDetection(app.db, &d); err != nil {
		logIt(LERR, "db insert failed", map[string]string{"error": err.Error()})
		fail("db")
		return
	}
	if s3UploadEnabled() {
		s3RetryOne(app, &d)
	}
	var mev2 = OnvifEvent{id: newID(), cameraID: camID, camName: cam.Name, detType: "manual-video", createdAt: nowMs, detectionID: d.id}
	if err := dbInsertOnvifEvent(app.db, &mev2); err != nil {
		logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
	}
	mqttPublishDetection(&d)
	mqttPublishEvent(camID, cam.Name, "manual-video", d.id, mev2.id, true)
	logIt(LINFO, "manual record captured", map[string]string{"camera": cam.Name, "path": dest, "duration": strconv.Itoa(durSec)})
	var clipURL = "/api/manual/" + camID + "/" + fname
	manualJobsMtx.Lock()
	job.status = "done"
	job.clip = clipURL
	job.detectionID = d.id
	manualJobsMtx.Unlock()
}

// startManualRecord starts a toggle-mode manual recording, returns tmp clip path.
func startManualRecord(app *App, camID, camName string) (string, error) {
	idx := -1
	for i := range app.cfg.Cameras {
		if fmt.Sprintf("cam%d", i) == camID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("no such camera")
	}
	cam := &app.cfg.Cameras[idx]
	src := recordingsSource(app, cam, camID)
	if src == "" {
		return "", fmt.Errorf("no rtsp url")
	}
	dir := app.cfg.Server.DataDir + "/manual/" + camID
	_ = ensureDir(dir)
	now := time.Now()
	// Unique per recording: two rapid starts in the same second must not
	// share (and clobber) one dest/tmp file.
	recID := newID()
	fname := friendlyTS(now.Unix()) + "-" + recID + "-manual.mp4"
	dest := dir + "/" + fname
	tmp := dest + ".ts"
	manualRecMtx.Lock()
	if _, ok := manualRecMap[camID]; ok {
		manualRecMtx.Unlock()
		return "", fmt.Errorf("already recording")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	st := &manualRecState{cancel: cancel, dest: dest, tmp: tmp, start: now.Unix(), id: recID, done: done}
	manualRecMap[camID] = st
	manualRecMtx.Unlock()
	// start background loop
	go manualRecordLoop(ctx, src, tmp, done)
	// Safety cap: a forgotten toggle recording stops itself after 30 min so
	// it cannot fill data_dir unboundedly.
	go func(id string) {
		select {
		case <-done:
		case <-time.After(30 * time.Minute):
			manualRecMtx.Lock()
			cur, ok := manualRecMap[camID]
			manualRecMtx.Unlock()
			if ok && cur != nil && cur.id == id {
				if clip, serr := stopManualRecord(app, camID); serr == nil {
					logIt(LINFO, "manual record auto-stopped at 30min cap", map[string]string{"camera": camName, "clip": clip})
				}
			}
		}
	}(recID)
	logIt(LINFO, "manual record started", map[string]string{"camera": camName, "tmp": tmp, "dest": dest})
	return dest, nil
}

// errNotRecording is returned by stopManualRecord when the camera has no
// ongoing toggle recording (as opposed to a real stop/transmux failure).
var errNotRecording = errors.New("not recording")

func stopManualRecord(app *App, camID string) (string, error) {
	manualRecMtx.Lock()
	st, ok := manualRecMap[camID]
	if !ok || st == nil {
		manualRecMtx.Unlock()
		return "", errNotRecording
	}
	delete(manualRecMap, camID)
	manualRecMtx.Unlock()
	// signal stop
	st.cancel()
	// Wait for the recorder to flush and close its writer (bounded: a hung
	// RTSP read must not pin the request forever). A fixed sleep here was a
	// heuristic and could transmux a half-flushed file.
	if st.done != nil {
		select {
		case <-st.done:
		case <-time.After(5 * time.Second):
		}
	}
	// transmux TS to MP4
	if _, err := os.Stat(st.tmp); err != nil {
		return "", fmt.Errorf("no data recorded")
	}
	var finalDest = st.dest
	if err := transmuxTsSegmentsToMp4([]string{st.tmp}, st.dest); err != nil {
		// Fallback: keep the raw TS under a .ts name (a TS renamed to .mp4
		// is an invalid MP4 and would be advertised as video/mp4).
		var tsDest = strings.TrimSuffix(st.dest, ".mp4") + ".ts"
		if rerr := os.Rename(st.tmp, tsDest); rerr == nil {
			finalDest = tsDest
		} else {
			return "", fmt.Errorf("transmux failed: %v", err)
		}
	} else {
		_ = os.Remove(st.tmp)
	}
	if s, e := os.Stat(finalDest); e != nil || s.Size() == 0 {
		return "", fmt.Errorf("empty clip")
	}
	// create detection entry so it appears in Recordings
	var camName = ""
	for i := range app.cfg.Cameras {
		if fmt.Sprintf("cam%d", i) == camID {
			camName = app.cfg.Cameras[i].Name
			break
		}
	}
	nowMs := time.Now().Unix()
	d := Detection{id: st.id, cameraID: camID, camName: camName, detType: "manual-video", confidence: 1.0, startedAt: st.start, endedAt: nowMs, snapshotPath: "", clipPath: finalDest}
	// thumbnail (buffer → native → rtsp → http)
	dir := app.cfg.Server.DataDir + "/manual/" + camID
	tmpSnap := dir + "/" + friendlyTS(st.start) + "-" + st.id + "-manual-thumb.jpg"
	if _, ok := grabSnapshotChain(app, camID, tmpSnap); ok {
		d.snapshotPath = tmpSnap
	} else {
		_ = writeFileBestEffort(tmpSnap, snapshotJpg(), 0o640)
		d.snapshotPath = tmpSnap
	}
	d.sizeBytes = detectionSize(&d)
	if s3UploadEnabled() {
		d.s3Expected = 1
	}
	if err := dbInsertDetection(app.db, &d); err != nil {
		logIt(LERR, "db insert failed", map[string]string{"error": err.Error()})
		return "", err
	}
	if s3UploadEnabled() {
		s3RetryOne(app, &d)
	}
	var mev3 = OnvifEvent{id: newID(), cameraID: camID, camName: camName, detType: "manual-video", createdAt: nowMs, detectionID: d.id}
	if err := dbInsertOnvifEvent(app.db, &mev3); err != nil {
		logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
	}
	mqttPublishDetection(&d)
	mqttPublishEvent(camID, camName, "manual-video", d.id, mev3.id, true)
	logIt(LINFO, "manual record stopped", map[string]string{"camera": camName, "path": finalDest, "duration": strconv.FormatInt(nowMs-st.start, 10)})
	// dest basename carries the actual timestamp of the start.
	clipURL := "/api/manual/" + camID + "/" + finalDest[strings.LastIndex(finalDest, "/")+1:]
	return clipURL, nil
}

// manualRecordLoop captures one manual recording into a single MPEG-TS file.
// `done` is closed last (after the writer is flushed/closed) so the caller can
// wait for a deterministic teardown instead of sleeping.
// RTSP Start/Describe run under rtspOpTimeout (10s, see records.go) and honor
// ctx cancel so a dead camera cannot pin the recorder goroutine.
func manualRecordLoop(ctx context.Context, src, outTs string, done chan struct{}) {
	defer close(done)
	select {
	case <-ctx.Done():
		return
	default:
	}
	u, err := base.ParseURL(src)
	if err != nil {
		return
	}
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := rtspOpTimeout(10*time.Second, c.Start); err != nil {
		return
	}
	defer c.Close()
	var desc *description.Session
	if derr := rtspOpTimeout(10*time.Second, func() error {
		d, _, e := c.Describe(u)
		if e == nil {
			desc = d
		}
		return e
	}); derr != nil || desc == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	default:
	}
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		return
	}
	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		return
	}
	// single TS file for whole manual recording
	w, err := newTsSegmentWriter(outTs, forma.SPS, forma.PPS)
	if err != nil {
		return
	}
	defer w.close()
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return
	}
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		pts, ok := c.PacketPTS(medi, pkt)
		if !ok {
			return
		}
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if !errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) && !errors.Is(err, rtph264.ErrMorePacketsNeeded) {
			}
			return
		}
		_ = w.writeH264(au, pts)
	})
	if _, err := c.Play(nil); err != nil {
		return
	}
	// wait until canceled
	<-ctx.Done()
	// deferred w.close() flushes; done is closed after it returns
}

// handleManualSnapshot serves a previously captured manual media (snapshot or clip).
// GET /api/manual/{camID}/{file}
func handleManualSnapshot(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	// path is /api/manual/cam0/file.jpg -> trim prefix
	var rel = strings.TrimPrefix(path, "/api/manual/")
	if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "\\") || strings.HasPrefix(rel, "/") {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var parts = strings.Split(rel, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(parts[1], "/") {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var camID = parts[0]
	var fname = parts[1]
	// camID must be a configured camera; filename is a flat basename with a
	// safe media extension (no subdirs, no hidden files).
	var camOK = false
	for i := range app.cfg.Cameras {
		if fmt.Sprintf("cam%d", i) == camID {
			camOK = true
			break
		}
	}
	if !camOK {
		writeJSON(w, 404, "{\"error\":\"no such camera\"}")
		return
	}
	if strings.HasPrefix(fname, ".") {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var lower = strings.ToLower(fname)
	if !(strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".ts")) {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var base = filepath.Clean(app.cfg.Server.DataDir + "/manual/" + camID)
	var full = filepath.Clean(base + "/" + fname)
	if !pathInsideDir(app.cfg.Server.DataDir, full) || filepath.Dir(full) != base {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	ctype := "image/jpeg"
	if strings.HasSuffix(strings.ToLower(rel), ".mp4") {
		ctype = "video/mp4"
	} else if strings.HasSuffix(strings.ToLower(rel), ".ts") {
		ctype = "video/mp2t"
	}
	serveFile(w, r, full, ctype)
}

var testDetectionMtx sync.Mutex

func runTestDetection(app *App, w http.ResponseWriter) {
	if !testDetectionMtx.TryLock() {
		writeJSON(w, 429, "{\"error\":\"test already running\"}")
		return
	}
	defer testDetectionMtx.Unlock()
	var dtype = "motion"
	var camID = ""
	var camName = "test"
	var src = ""
	for i := range app.cfg.Cameras {
		if app.cfg.Cameras[i].Enabled {
			var cam = &app.cfg.Cameras[i]
			camID = fmt.Sprintf("cam%d", i)
			camName = cam.Name
			src = recordingsSource(app, cam, camID)
			break
		}
	}
	if camID == "" {
		camID = "cam0"
		camName = "test"
	}
	var d, err = collectDetection(app, camID, camName, src, dtype)
	if err != nil {
		writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
		return
	}
	if d != nil {
		var ev = OnvifEvent{id: newID(), cameraID: camID, camName: camName, detType: dtype, createdAt: d.startedAt + int64(app.cfg.Record.PreDetection), detectionID: d.id}
		if err := dbInsertOnvifEvent(app.db, &ev); err != nil {
			logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
		}
		confirmActiveRec(camID, d.id, d.endedAt)
		writeJSON(w, 200, detJSON(d))
	} else {
		writeJSON(w, 500, "{\"error\":\"no detection\"}")
	}
}

func parseUnixOrDate(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		// millis -> seconds
		if n > 100000000000 {
			return n / 1000
		}
		return n
	}
	// YYYY-MM-DD (start of day local)
	if len(v) == 10 && v[4] == '-' && v[7] == '-' {
		if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			return t.Unix()
		}
	}
	// datetime-local "2006-01-02T15:04" or with seconds
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func endOfDayUnix(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if len(v) == 10 && v[4] == '-' && v[7] == '-' {
		if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			// Start of the NEXT day minus a second: adding 24h is off by an
			// hour on DST transition days.
			y, m, d := t.Date()
			return time.Date(y, m, d+1, 0, 0, 0, 0, time.Local).Add(-time.Second).Unix()
		}
	}
	return parseUnixOrDate(v)
}

// parseListFilter extracts camera/type/from/to from query params.
// Supported: camera | camera_id | camera_name, type | det_type,
// from | since | from_date | from_dt | date (start), to | until | to_date | to_dt | date (end of day).
func parseListFilter(r *http.Request) (string, string, int64, int64) {
	var q = r.URL.Query()
	var cam = q.Get("camera")
	if cam == "" {
		cam = q.Get("camera_id")
	}
	if cam == "" {
		cam = q.Get("camera_name")
	}
	var ty = q.Get("type")
	if ty == "" {
		ty = q.Get("det_type")
	}
	var from = parseUnixOrDate(q.Get("from"))
	if from == 0 {
		from = parseUnixOrDate(q.Get("since"))
	}
	if from == 0 {
		from = parseUnixOrDate(q.Get("from_date"))
	}
	if from == 0 {
		from = parseUnixOrDate(q.Get("from_dt"))
	}
	var to = parseUnixOrDate(q.Get("to"))
	if to == 0 {
		to = parseUnixOrDate(q.Get("until"))
	}
	if to == 0 {
		to = endOfDayUnix(q.Get("to_date"))
		if to == 0 {
			to = parseUnixOrDate(q.Get("to_dt"))
		}
	}
	if d := strings.TrimSpace(q.Get("date")); d != "" {
		if from == 0 {
			from = parseUnixOrDate(d)
		}
		if to == 0 {
			to = endOfDayUnix(d)
		}
	}
	// date-only from_date should start at day start (already), to handled as end of day above.
	// Only when it actually parses: an unparseable value must not reset a valid
	// from/from_dt back to 0 and silently widen the range to everything.
	if fd := strings.TrimSpace(q.Get("from_date")); fd != "" && len(fd) == 10 {
		if v := parseUnixOrDate(fd); v != 0 {
			from = v
		}
	}
	return cam, ty, from, to
}

func handleDetections(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		serveDetectionsCSV(app, w, r)
		return
	}
	var cam, ty, from, to = parseListFilter(r)
	var q = r.URL.Query()
	var label = strings.TrimSpace(q.Get("label"))
	var hasVision = q.Get("has_vision") == "1" || strings.EqualFold(q.Get("has_vision"), "true")
	// Pagination: ?limit= (default 500, max 5000) and ?offset=. The total
	// matching rows ride along in X-Total-Count so old recordings stay
	// reachable instead of stopping at the first 500.
	var limit = 500
	if v, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && v > 0 {
		limit = v
	}
	if limit > 5000 {
		limit = 5000
	}
	var offset int
	if v, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil && v > 0 {
		offset = v
	}
	var dlist []Detection
	var err error
	if label != "" || hasVision {
		dlist, err = listDetectionsFiltered(app.db, from, to, cam, ty, label, hasVision, limit, offset)
	} else {
		dlist, err = listDetectionsFiltered(app.db, from, to, cam, ty, "", false, limit, offset)
	}
	if err != nil {
		writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
		return
	}
	w.Header().Set("X-Total-Count", strconv.FormatInt(countDetectionsFiltered(app.db, from, to, cam, ty, label, hasVision), 10))
	var arr []string
	for i := range dlist {
		arr = append(arr, detJSON(&dlist[i]))
	}
	writeJSON(w, 200, "["+strings.Join(arr, ",")+"]")
}

func handleOnvifEvents(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		// ?detection_id= deletes one group (all raw events of a recording);
		// without it, the whole table is cleared.
		if gid := strings.TrimSpace(r.URL.Query().Get("detection_id")); gid != "" {
			res, err := app.db.db.Exec("DELETE FROM onvif_events WHERE detection_id=?", gid)
			if err != nil {
				writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
				return
			}
			var n int64
			if res != nil {
				n, _ = res.RowsAffected()
			}
			writeJSON(w, 200, "{\"ok\":true,\"removed\":"+strconv.FormatInt(n, 10)+"}")
			return
		}
		if _, derr := app.db.db.Exec("DELETE FROM onvif_events"); derr != nil {
			writeJSON(w, 500, "{\"error\":"+jsonString(derr.Error())+"}")
			return
		}
		writeJSON(w, 200, "{\"ok\":true}")
		return
	}
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		serveOnvifEventsCSV(app, w, r)
		return
	}
	var cam, ty, from, to = parseListFilter(r)
	var elist, err = listOnvifEvents(app.db, from, to, cam, ty)
	if err != nil {
		writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
		return
	}
	var arr []string
	for i := range elist {
		arr = append(arr, onvifEventJSON(&elist[i]))
	}
	writeJSON(w, 200, "["+strings.Join(arr, ",")+"]")
}

// handleOnvifEventDelete removes one raw event.
//
//	DELETE /api/onvif_events/{eventID} -> {"ok":true}
//
// Linked recordings are untouched (delete those from the Recordings tab).
func handleOnvifEventDelete(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "DELETE" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) != 4 || seg[3] == "" {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	res, err := app.db.db.Exec("DELETE FROM onvif_events WHERE id=?", seg[3])
	if err != nil {
		writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
		return
	}
	var n int64
	if res != nil {
		n, _ = res.RowsAffected()
	}
	if n == 0 {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	writeJSON(w, 200, "{\"ok\":true}")
}

// handleDetectionDelete removes media + rows.
//
//	DELETE /api/detections/{id}  -> remove one detection
//	DELETE /api/detections       -> remove all detections
func handleDetectionDelete(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "DELETE" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) == 4 && seg[3] != "" {
		var id = seg[3]
		var d, err = getDetection(app.db, id)
		if err != nil {
			writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
			return
		}
		if d == nil {
			writeJSON(w, 404, "{\"error\":\"not found\"}")
			return
		}
		// DB first, then files/S3 (same ordering as delete-all and pruneOnce):
		// a crash between the steps never leaves a DB row pointing at
		// missing media. DB failures are reported (no silent ok:true).
		var dberr error
		if tx, terr := app.db.db.Begin(); terr == nil {
			var e1, e2, ec error
			_, e1 = tx.Exec("DELETE FROM detections WHERE id=?", id)
			_, e2 = tx.Exec("DELETE FROM vision_results WHERE detection_id=?", id)
			if e1 == nil && e2 == nil {
				ec = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if e1 != nil {
				dberr = e1
			} else if e2 != nil {
				dberr = e2
			} else {
				dberr = ec
			}
		} else {
			dberr = terr
			if _, e := app.db.db.Exec("DELETE FROM detections WHERE id=?", id); e != nil {
				dberr = e
			} else if e := dbDeleteVisionResults(app.db, id); e != nil {
				dberr = e
			} else {
				dberr = nil
			}
		}
		if dberr != nil {
			writeJSON(w, 500, "{\"error\":"+jsonString(dberr.Error())+"}")
			return
		}
		deleteDetectionFiles(app, d)
		// Don't let later events extend the ghost of this group.
		clearActiveRec(d.cameraID, id)
		writeJSON(w, 200, "{\"ok\":true}")
		return
	}
	if len(seg) == 3 {
		// Uncapped: the JSON listing is paginated, so delete-all reads the
		// whole table directly — otherwise old rows, files and S3 objects
		// would be left behind while the UI looked empty.
		var dlist, err = listAllDetections(app.db)
		if err != nil {
			writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
			return
		}
		var dberr error
		if tx, terr := app.db.db.Begin(); terr == nil {
			var e1, e2, ec error
			_, e1 = tx.Exec("DELETE FROM detections")
			_, e2 = tx.Exec("DELETE FROM vision_results")
			if e1 == nil && e2 == nil {
				ec = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if e1 != nil {
				dberr = e1
			} else if e2 != nil {
				dberr = e2
			} else {
				dberr = ec
			}
		} else {
			dberr = terr
			if _, e := app.db.db.Exec("DELETE FROM detections"); e != nil {
				dberr = e
			} else if _, e := app.db.db.Exec("DELETE FROM vision_results"); e != nil {
				dberr = e
			} else {
				dberr = nil
			}
		}
		if dberr != nil {
			writeJSON(w, 500, "{\"error\":"+jsonString(dberr.Error())+"}")
			return
		}
		var n = 0
		for i := range dlist {
			deleteDetectionFiles(app, &dlist[i])
			n++
		}
		clearAllActiveRecs()
		pruneEmptyDirs(app.cfg.Server.DataDir + "/detections")
		pruneEmptyDirs(app.cfg.Server.DataDir + "/manual")
		writeJSON(w, 200, "{\"ok\":true,\"removed\":"+fmt.Sprintf("%d", n)+"}")
		return
	}
	writeJSON(w, 400, "{\"error\":\"bad path\"}")
}

// handleDetectionLock toggles the locked flag for a recording.
//
//	POST /api/detections/{id}/lock  body {"locked":true/false}  or ?locked=1
//	Locked recordings are excluded from automatic retention prune.
func handleDetectionLock(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) < 4 {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	var d, err = getDetection(app.db, id)
	if err != nil {
		writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
		return
	}
	if d == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	// parse desired value
	var lockedStr = r.URL.Query().Get("locked")
	if lockedStr == "" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s := strings.TrimSpace(string(body))
		// try JSON {"locked":true}
		var j map[string]any
		if err := json.Unmarshal([]byte(s), &j); err == nil {
			if v, ok := j["locked"]; ok {
				switch vv := v.(type) {
				case bool:
					if vv {
						lockedStr = "true"
					} else {
						lockedStr = "false"
					}
				case float64:
					if vv != 0 {
						lockedStr = "true"
					} else {
						lockedStr = "false"
					}
				case string:
					lockedStr = vv
				}
			}
		} else if s != "" {
			lockedStr = s
		}
		if lockedStr == "" {
			// toggle if not specified
			lockedStr = boolJSON(d.locked == 0)
		}
	}
	bv, ok := parseMqttBool(lockedStr)
	if !ok {
		// also accept "1"/"0" already handled, fallback to lockedbool
		if lockedStr == "1" {
			bv, ok = true, true
		} else if lockedStr == "0" {
			bv, ok = false, true
		}
	}
	if !ok {
		writeJSON(w, 400, "{\"error\":\"locked must be true/false or 1/0\"}")
		return
	}
	if err := dbSetLocked(app.db, id, bv); err != nil {
		writeJSON(w, 500, "{\"error\":"+jsonString(err.Error())+"}")
		return
	}
	writeJSON(w, 200, "{\"ok\":true,\"locked\":"+boolJSON(bv)+"}")
}

// handleDetectionState returns or toggles the global detection switch.
//
//	GET /api/detection/state                  -> {"enabled":bool}
//	POST /api/detection/state?enabled=true|false  (also accepts ON/OFF, 1/0)
//
// Deduplicated via serveBoolToggle (see toggleRoutes in handle).
func handleDetectionState(app *App, w http.ResponseWriter, r *http.Request) {
	serveBoolToggle(w, r, detectionEnabled, setDetectionEnabled)
}

func extractEnabledParam(r *http.Request) string {
	if v := r.URL.Query().Get("enabled"); v != "" {
		return v
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		return ""
	}
	// Only ParseForm for form bodies; JSON bodies are handled below. Parsing
	// unconditionally would consume/lock the body for JSON clients.
	var ct = strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/x-www-form-urlencoded") || strings.Contains(ct, "multipart/form-data") {
		_ = r.ParseForm()
		if v := r.Form.Get("enabled"); v != "" {
			return v
		}
		return ""
	}
	// try JSON body {"enabled":true} or raw ON/OFF
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	// try JSON
	var j map[string]any
	if err := json.Unmarshal([]byte(s), &j); err == nil {
		if ev, ok := j["enabled"]; ok {
			switch evv := ev.(type) {
			case bool:
				if evv {
					return "true"
				}
				return "false"
			case string:
				return evv
			case float64:
				if evv == 1 {
					return "true"
				}
				return "false"
			}
		}
	}
	// raw payload like "ON" or "true"
	return s
}

// handleRecordingState returns or toggles the global recording switch.
//
//	GET /api/recording/state                  -> {"enabled":bool}
//	POST /api/recording/state?enabled=true|false
//
// Deduplicated via serveBoolToggle (see toggleRoutes in handle).
func handleRecordingState(app *App, w http.ResponseWriter, r *http.Request) {
	serveBoolToggle(w, r, recordingEnabled, setRecordingEnabled)
}

// serveBoolToggle implements a GET/POST {"enabled":bool} toggle endpoint.
// POST bodies are capped at 1MB. All six toggle endpoints share this helper
// (toggleRoutes): detection, recording, s3, snapshots, pushover, vision.
func serveBoolToggle(w http.ResponseWriter, r *http.Request, get func() bool, set func(bool)) {
	if r.Method == "POST" {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	}
	if r.Method == "GET" {
		writeJSON(w, 200, "{\"enabled\":"+boolJSON(get())+"}")
		return
	}
	if r.Method == "POST" {
		var v = extractEnabledParam(r)
		if v == "" {
			writeJSON(w, 400, "{\"error\":\"enabled must be true/false or ON/OFF\"}")
			return
		}
		if bv, ok := parseMqttBool(v); ok {
			set(bv)
			writeJSON(w, 200, "{\"enabled\":"+boolJSON(get())+"}")
			return
		}
		writeJSON(w, 400, "{\"error\":\"enabled must be true/false or ON/OFF\"}")
		return
	}
	methodNotAllowed(w)
}

// handleS3State returns or toggles the S3 upload switch.
//
//	GET /api/s3/state  -> {"enabled":bool}
//	POST /api/s3/state?enabled=true|false
func handleS3State(app *App, w http.ResponseWriter, r *http.Request) {
	serveBoolToggle(w, r, s3UploadEnabled, setS3UploadEnabled)
}

// handleSnapshotsState returns or toggles the snapshots switch.
//
//	GET /api/snapshots/state  -> {"enabled":bool}
//	POST /api/snapshots/state?enabled=true|false
func handleSnapshotsState(app *App, w http.ResponseWriter, r *http.Request) {
	serveBoolToggle(w, r, snapshotsEnabled, setSnapshotsEnabled)
}

// handlePushoverState returns or toggles the push notification switch.
//
//	GET /api/pushover/state  -> {"enabled":bool}
//	POST /api/pushover/state?enabled=true|false
func handlePushoverState(app *App, w http.ResponseWriter, r *http.Request) {
	serveBoolToggle(w, r, pushoverEnabled, setPushoverEnabled)
}

// handleVisionState returns or toggles the YOLO analysis switch.
//
//	GET /api/vision/state  -> {"enabled":bool}
//	POST /api/vision/state?enabled=true|false
func handleVisionState(app *App, w http.ResponseWriter, r *http.Request) {
	serveBoolToggle(w, r, visionEnabled, setVisionEnabled)
}

// handleVisionStatus proxies the vision API readiness (no API key leaked).
//
//	GET /api/vision/status -> {enabled,url,ready,model,min_conf,classes,snapshots}
func handleVisionStatus(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	var enabled = visionEnabled()
	var ready = false
	var model string
	if enabled {
		ready, model = cachedVisionReady(app)
	} else if app != nil && app.cfg != nil && strings.TrimSpace(app.cfg.Vision.URL) != "" {
		// Report readiness even when locally disabled so the UI can hint
		// "API up but analysis off".
		ready, model = cachedVisionReady(app)
	}
	var u string
	if app != nil && app.cfg != nil {
		u = strings.TrimSpace(app.cfg.Vision.URL)
	}
	var body = "{" + kvRaw("enabled", boolJSON(enabled)) +
		"," + kv("url", u) +
		"," + kvRaw("ready", boolJSON(ready)) +
		"," + kv("model", model)
	if app != nil && app.cfg != nil {
		body += `,"min_conf":` + fmt.Sprintf("%.3f", app.cfg.Vision.MinConf) +
			"," + kv("classes", app.cfg.Vision.Classes) +
			"," + kv("snapshots", app.cfg.Vision.Snapshots)
	}
	body += "}"
	writeJSON(w, 200, body)
}

// visionResultJSON renders one per-snapshot outcome (full boxes included).
func visionResultJSON(d *Detection, r *VisionSnapResult) string {
	var dets = validJSONArrayOrEmpty(r.json)
	var snapURL = "/api/detections/" + d.id + "/snapshot"
	if r.snapIndex > 0 {
		snapURL += "/" + strconv.Itoa(r.snapIndex)
	}
	return `{"snap_index":` + strconv.Itoa(r.snapIndex) +
		`,"snapshot":` + jsonString(snapURL) +
		`,"count":` + strconv.Itoa(r.count) +
		`,"top":` + jsonString(r.top) +
		`,"ms":` + fmt.Sprintf("%.1f", r.ms) +
		`,"total_ms":` + fmt.Sprintf("%.1f", r.totalMs) +
		`,"at":` + strconv.FormatInt(r.at, 10) +
		`,"error":` + jsonString(r.err) +
		`,"detections":` + dets + `}`
}

// handleDetectionVision serves or triggers YOLO analysis for one detection.
//
//	GET  /api/detections/{id}/vision -> summary + per-snapshot results[]
//	POST /api/detections/{id}/vision -> re-analyze primary snapshot now (sync)
func handleDetectionVision(app *App, w http.ResponseWriter, r *http.Request, path string) {
	var seg = strings.Split(path, "/")
	if len(seg) < 5 || seg[3] == "" {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	var d, err = getDetection(app.db, id)
	if err != nil {
		writeJSON(w, 500, "{\"error\":\"db\"}")
		return
	}
	if d == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	if r.Method == "GET" {
		var dets = validJSONArrayOrEmpty(d.visionJSON)
		var results []string
		if rows, rerr := dbListVisionResults(app.db, id); rerr != nil {
			logIt(LWARN, "vision results query failed", map[string]string{"error": rerr.Error()})
		} else {
			for i := range rows {
				results = append(results, visionResultJSON(d, &rows[i]))
			}
		}
		writeJSON(w, 200, `{"id":`+jsonString(d.id)+
			`,"vision":{"count":`+strconv.Itoa(d.visionCount)+
			`,"top":`+jsonString(d.visionTop)+
			`,"ms":`+fmt.Sprintf("%.1f", d.visionMs)+
			`,"at":`+strconv.FormatInt(d.visionAt, 10)+
			`,"error":`+jsonString(d.visionError)+
			`,"detections":`+dets+
			`,"results":[`+strings.Join(results, ",")+`]}}`)
		return
	}
	if r.Method == "POST" {
		if !visionEnabled() {
			writeJSON(w, 503, "{\"error\":\"vision is disabled\"}")
			return
		}
		if d.snapshotPath == "" {
			writeJSON(w, 404, "{\"error\":\"no snapshot\"}")
			return
		}
		if _, serr := os.Stat(d.snapshotPath); serr != nil {
			writeJSON(w, 404, "{\"error\":\"snapshot missing\"}")
			return
		}
		sem, ok := visionAcquire()
		if !ok {
			// Workers saturated: queue the re-analysis instead of failing
			// with 429 so the click is never lost (result arrives via the
			// normal async path: SSE/broadcast + next list refresh).
			visionEnqueue(app, id, d.snapshotPath, 0)
			writeJSON(w, 202, `{"ok":true,"queued":true,"id":`+jsonString(d.id)+`}`)
			return
		}
		res, verr := visionAnalyzeFile(app, d.snapshotPath)
		visionRelease(sem)
		if verr != nil {
			visionNoteFailure()
			_ = dbSetVisionSnapError(app.db, id, 0, d.snapshotPath, verr.Error())
			writeJSON(w, 502, "{\"error\":"+jsonString(verr.Error())+"}")
			return
		}
		visionNoteSuccess()
		if derr := dbSetVision(app.db, id, res, d.snapshotPath, 0, time.Now().Unix()); derr != nil {
			writeJSON(w, 500, "{\"error\":"+jsonString(derr.Error())+"}")
			return
		}
		if fresh, ferr := getDetection(app.db, id); ferr == nil && fresh != nil {
			mqttPublishDetection(fresh)
			d = fresh
		}
		var dets = strings.TrimSpace(d.visionJSON)
		if dets == "" {
			dets = "[]"
		}
		writeJSON(w, 200, `{"ok":true,"id":`+jsonString(d.id)+
			`,"vision":{"count":`+strconv.Itoa(d.visionCount)+
			`,"top":`+jsonString(d.visionTop)+
			`,"ms":`+fmt.Sprintf("%.1f", d.visionMs)+
			`,"at":`+strconv.FormatInt(d.visionAt, 10)+
			`,"error":`+jsonString(d.visionError)+
			`,"detections":`+dets+`}}`)
		return
	}
	methodNotAllowed(w)
}

// s3RetryMtx guards s3RetryRunning: only one retry job at a time.
var s3RetryMtx sync.Mutex
var s3RetryRunning = false

// handleS3Retry re-uploads rows still pending (s3_expected=1 AND uploaded=0).
//
//	POST /api/s3/retry -> {"ok":true,"queued":N} (job runs in background)
func handleS3Retry(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	if !s3UploadEnabled() {
		writeJSON(w, 400, "{\"error\":\"s3 upload is disabled\"}")
		return
	}
	s3RetryMtx.Lock()
	if s3RetryRunning {
		s3RetryMtx.Unlock()
		writeJSON(w, 200, "{\"ok\":true,\"running\":true,\"queued\":0}")
		return
	}
	s3RetryRunning = true
	s3RetryMtx.Unlock()
	var queued = dbCount(app.db, "SELECT COUNT(*) FROM detections WHERE uploaded=0 AND s3_expected=1")
	go s3RetryLoop(app)
	writeJSON(w, 200, "{\"ok\":true,\"running\":true,\"queued\":"+strconv.FormatInt(queued, 10)+"}")
}

// s3RetryLoop uploads every pending row's present files and marks it uploaded
// when the full set succeeds. Rows younger than 120s are skipped: a buffer-mode
// clip may still be assembling (assembleClip fills clip_path asynchronously).
// Paginated so >500 pending rows are not starved.
func s3RetryLoop(app *App) {
	defer func() {
		s3RetryMtx.Lock()
		s3RetryRunning = false
		s3RetryMtx.Unlock()
	}()
	if app.db == nil || app.db.db == nil {
		return
	}
	var done, failed int
	var cutoff = time.Now().Unix() - 120
	// No OFFSET pagination: processed rows leave the WHERE set, so OFFSET
	// would skip ~500 pending rows per run. Fetch all pending ids once and
	// attempt each exactly once per run (failures stay pending for next run).
	rows, err := app.db.db.Query("SELECT id FROM detections WHERE uploaded=0 AND s3_expected=1 ORDER BY started_at ASC")
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		d, derr := getDetection(app.db, id)
		if derr != nil || d == nil {
			continue
		}
		if d.endedAt > cutoff {
			continue // clip may still be assembling; next retry will catch it
		}
		if s3RetryOne(app, d) {
			done++
		} else {
			failed++
			if failed%10 == 0 {
				time.Sleep(s3Backoff(failed/10 + 1))
			}
		}
	}
	logIt(LINFO, "s3 retry complete", map[string]string{"uploaded": strconv.Itoa(done), "failed": strconv.Itoa(failed)})
}

// s3RetryOne uploads one detection's snapshot (+extras) and clip when present.
// Returns true when everything present uploaded and the row was marked uploaded.
// Small files (snapshots) upload concurrently via s3PutFiles; the clip streams
// separately since it can be hundreds of MB.
func s3RetryOne(app *App, d *Detection) bool {
	var small []s3PutJob
	if d.snapshotPath != "" {
		small = append(small, s3PutJob{key: s3KeyForPath(d.snapshotPath), path: d.snapshotPath, contentType: "image/jpeg"})
	}
	if d.extraSnap != "" {
		for _, p := range strings.Split(d.extraSnap, ",") {
			if p == "" {
				continue
			}
			if _, serr := os.Stat(p); serr != nil {
				continue // pruned since; not fatal
			}
			small = append(small, s3PutJob{key: s3KeyForPath(p), path: p, contentType: "image/jpeg"})
		}
	}
	if len(small) > 0 && !s3PutFiles(app, small) {
		return false
	}
	if d.clipPath != "" {
		if _, serr := os.Stat(d.clipPath); serr == nil {
			if err := s3PutFile(app, s3KeyForPath(d.clipPath), d.clipPath, clipContentType(d.clipPath)); err != nil {
				return false
			}
		}
		// clip path recorded but file gone (pruned): treat remaining files as complete
	}
	// A successful upload that fails to mark the row must stay pending (and
	// count as failed): otherwise the retry log over-reports success.
	if err := dbSetUploaded(app.db, d.id); err != nil {
		logIt(LWARN, "s3 retry mark-uploaded failed", map[string]string{"id": d.id, "error": err.Error()})
		return false
	}
	return true
}

// handleMqttStatus reports MQTT connection state.
func handleMqttStatus(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	mqttMtx.Lock()
	c := mqttClient
	mqttMtx.Unlock()
	connected := false
	if c != nil {
		connected = c.IsConnected()
	}
	enabled := app.cfg.Mqtt.Enabled
	broker := mqttBrokerURL(&app.cfg.Mqtt)
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	writeJSON(w, 200, "{"+kvRaw("enabled", boolJSON(enabled))+","+kvRaw("connected", boolJSON(connected))+","+kv("broker", broker)+","+kv("topic_prefix", prefix)+","+kvRaw("detection_enabled", boolJSON(detectionEnabled()))+","+kvRaw("recording_enabled", boolJSON(recordingEnabled()))+"}")
}

// ---------------- system status page ----------------------------

// procRSSBytes returns the resident set size of pid in bytes via /proc (0 when unavailable).
func procRSSBytes(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb int64
			if _, serr := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:")), "%d", &kb); serr == nil {
				return kb * 1024
			}
		}
	}
	return 0
}

// go2rtcHTTPOK probes the go2rtc HTTP API root.
func go2rtcHTTPOK(app *App) bool {
	if getStreamCmd() == nil {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/streams", app.cfg.Stream.Port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == 200
}

// probeRelay checks one relayed stream with an RTSP DESCRIBE against the local
// go2rtc RTSP port. Bounded by timeout; never blocks the status handler. On
// timeout the client connection is closed so the probe goroutine (and its
// socket) cannot pile up across status polls — done is buffered so the
// woken goroutine always exits.
func probeRelay(camID string, rtspPort int) bool {
	done := make(chan bool, 1)
	var cmu sync.Mutex
	var cc *gortsplib.Client
	go func() {
		u, err := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/%s", rtspPort, camID))
		if err != nil {
			done <- false
			return
		}
		c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
		cmu.Lock()
		cc = &c
		cmu.Unlock()
		if err := c.Start(); err != nil {
			done <- false
			return
		}
		defer c.Close()
		_, _, err = c.Describe(u)
		done <- err == nil
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(3 * time.Second):
		cmu.Lock()
		c := cc
		cmu.Unlock()
		if c != nil {
			c.Close()
		}
		return false
	}
}

// probeRelays DESCRIBEs every enabled relay concurrently, returning per-cam results.
func probeRelays(camIDs []string, rtspPort int) map[string]bool {
	out := map[string]bool{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range camIDs {
		wg.Add(1)
		go func(camID string) {
			defer wg.Done()
			if probeRelay(camID, rtspPort) {
				mu.Lock()
				out[camID] = true
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	return out
}

func dbCount(db *DB, q string, args ...any) int64 {
	var n int64
	if db == nil || db.db == nil {
		return 0
	}
	if err := db.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// mediaStatsCache bounds how often the recordings tree is walked: the walk is
// O(files) and the Status tab polls every few seconds.
var mediaStatsMtx sync.Mutex
var mediaStatsCache = map[string]struct {
	bytes, jpgs, mp4s int64
	at                time.Time
}{}

func walkMediaStatsCached(root string) (int64, int64, int64) {
	mediaStatsMtx.Lock()
	if r, ok := mediaStatsCache[root]; ok && time.Since(r.at) < 5*time.Second {
		mediaStatsMtx.Unlock()
		return r.bytes, r.jpgs, r.mp4s
	}
	mediaStatsMtx.Unlock()
	var b, j, m = walkMediaStats(root)
	mediaStatsMtx.Lock()
	mediaStatsCache[root] = struct {
		bytes, jpgs, mp4s int64
		at                time.Time
	}{b, j, m, time.Now()}
	mediaStatsMtx.Unlock()
	return b, j, m
}

// statusProbeCache bounds blocking network I/O inside status handlers: the UI
// polls every few seconds, so go2rtc/relay/vision probes are cached with a
// short TTL instead of running per request (which piled up handlers +
// sockets).
var statusProbeMtx sync.Mutex
var statusProbeGo2rtc struct {
	ok bool
	at time.Time
}
var statusProbeRelays struct {
	res  map[string]bool
	args string
	at   time.Time
}
var statusProbeVision struct {
	ok    bool
	model string
	url   string
	at    time.Time
}

func cachedGo2rtcHTTPOK(app *App) bool {
	statusProbeMtx.Lock()
	if time.Since(statusProbeGo2rtc.at) < 10*time.Second {
		ok := statusProbeGo2rtc.ok
		statusProbeMtx.Unlock()
		return ok
	}
	statusProbeMtx.Unlock()
	ok := go2rtcHTTPOK(app)
	statusProbeMtx.Lock()
	statusProbeGo2rtc.ok, statusProbeGo2rtc.at = ok, time.Now()
	statusProbeMtx.Unlock()
	return ok
}

func cachedProbeRelays(camIDs []string, rtspPort int) map[string]bool {
	key := strings.Join(camIDs, ",") + "|" + strconv.Itoa(rtspPort)
	statusProbeMtx.Lock()
	if statusProbeRelays.res != nil && statusProbeRelays.args == key && time.Since(statusProbeRelays.at) < 10*time.Second {
		res := statusProbeRelays.res
		statusProbeMtx.Unlock()
		return res
	}
	statusProbeMtx.Unlock()
	res := probeRelays(camIDs, rtspPort)
	statusProbeMtx.Lock()
	statusProbeRelays.res, statusProbeRelays.args, statusProbeRelays.at = res, key, time.Now()
	statusProbeMtx.Unlock()
	return res
}

func cachedVisionReady(app *App) (bool, string) {
	var url string
	if app != nil && app.cfg != nil {
		url = strings.TrimSpace(app.cfg.Vision.URL)
	}
	statusProbeMtx.Lock()
	if statusProbeVision.url == url && time.Since(statusProbeVision.at) < 15*time.Second {
		ok, model := statusProbeVision.ok, statusProbeVision.model
		statusProbeMtx.Unlock()
		return ok, model
	}
	statusProbeMtx.Unlock()
	ok, model := visionCheckReady(app)
	statusProbeMtx.Lock()
	statusProbeVision.ok, statusProbeVision.model, statusProbeVision.url, statusProbeVision.at = ok, model, url, time.Now()
	statusProbeMtx.Unlock()
	return ok, model
}

// walkMediaStats sums the recordings tree: total bytes, jpg count, mp4 count.
func walkMediaStats(root string) (bytes int64, jpgs int64, mp4s int64) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		lower := strings.ToLower(d.Name())
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		sz := info.Size()
		bytes += sz
		if strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") {
			jpgs++
		} else if strings.HasSuffix(lower, ".mp4") {
			if sz > 0 {
				mp4s++
			}
		} else if strings.HasSuffix(lower, ".ts") {
			// in-progress manual recording; count bytes but not as video
		}
		return nil
	})
	return bytes, jpgs, mp4s
}

func fsFreeTotal(path string) (free int64, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	free = int64(st.Bavail) * int64(st.Bsize)
	total = int64(st.Blocks) * int64(st.Bsize)
	return free, total
}

// handleSystemStatus serves GET /api/system/status — single payload for the Status tab.
func handleSystemStatus(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	// streams: probe each enabled relay with RTSP DESCRIBE (no shape guessing).
	var total, enabled int
	var enabledIDs []string
	for i := range app.cfg.Cameras {
		total++
		if app.cfg.Cameras[i].Enabled {
			enabled++
			enabledIDs = append(enabledIDs, fmt.Sprintf("cam%d", i))
		}
	}
	go2rtcRunning := getStreamCmd() != nil
	go2rtcHTTP := cachedGo2rtcHTTPOK(app)
	relayUp := map[string]bool{}
	if go2rtcRunning {
		relayUp = cachedProbeRelays(enabledIDs, app.cfg.Stream.RtspPort)
	}
	var up = len(relayUp)
	// memory
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	var go2rtcPID = getStreamCmdPID()
	// mqtt
	mqttMtx.Lock()
	mc := mqttClient
	mqttMtx.Unlock()
	mqttConn := mc != nil && mc.IsConnected()
	// db stats
	eventsTotal := dbCount(app.db, "SELECT COUNT(*) FROM onvif_events")
	detsTotal := dbCount(app.db, "SELECT COUNT(*) FROM detections")
	s3pending := dbCount(app.db, "SELECT COUNT(*) FROM detections WHERE uploaded=0 AND s3_expected=1")
	locked := dbCount(app.db, "SELECT COUNT(*) FROM detections WHERE locked=1")
	var lastEvent, lastDet int64
	if app.db != nil && app.db.db != nil {
		_ = app.db.db.QueryRow("SELECT COALESCE(MAX(created_at),0) FROM onvif_events").Scan(&lastEvent)
		_ = app.db.db.QueryRow("SELECT COALESCE(MAX(started_at),0) FROM detections").Scan(&lastDet)
	}
	// per-camera detection counts + last time
	type camAgg struct {
		n    int64
		last int64
	}
	aggs := map[string]*camAgg{}
	if app.db != nil && app.db.db != nil {
		rows, err := app.db.db.Query("SELECT camera_id, COUNT(*), COALESCE(MAX(started_at),0) FROM detections GROUP BY camera_id")
		if err == nil {
			for rows.Next() {
				var id string
				var a camAgg
				if serr := rows.Scan(&id, &a.n, &a.last); serr == nil {
					c := a
					aggs[id] = &c
				}
			}
			rows.Close()
		}
	}
	// disk
	detRoot := app.cfg.Server.DataDir + "/detections"
	diskBytes, jpgs, mp4s := walkMediaStatsCached(detRoot)
	bufRoot := bufferBaseDir(app)
	bufBytes, _, _ := walkMediaStatsCached(bufRoot)
	dataFree, dataTotal := fsFreeTotal(app.cfg.Server.DataDir)
	// cameras array
	var camArr []string
	for i := range app.cfg.Cameras {
		id := fmt.Sprintf("cam%d", i)
		c := app.cfg.Cameras[i]
		a := aggs[id]
		var n, last int64
		if a != nil {
			n, last = a.n, a.last
		}
		camArr = append(camArr, "{"+kv("id", id)+","+kv("name", c.Name)+
			`,"enabled":`+boolJSON(c.Enabled)+
			`,"stream_up":`+boolJSON(relayUp[id])+
			`,"detections":`+strconv.FormatInt(n, 10)+
			`,"last_detection":`+strconv.FormatInt(last, 10)+
			","+kv("detection_source", camDetectionSource(app.cfg, &app.cfg.Cameras[i]))+
			`,"rtsp_boxes":`+strconv.FormatInt(rtspStatGet(id).boxes, 10)+
			`,"rtsp_last_box":`+strconv.FormatInt(rtspStatGet(id).lastBox, 10)+
			","+kv("rtsp_last_type", rtspStatGet(id).lastType)+
			","+snapStatusJSON(id)+"}")
	}
	mqttBroker := mqttBrokerURL(&app.cfg.Mqtt)
	var visionReady, visionModel string
	var visionAnalyzed int64
	var visionAvgMs, visionAvgTotalMs float64
	var visionErr string
	if app != nil && app.cfg != nil {
		if visionEnabled() {
			var ok bool
			ok, visionModel = cachedVisionReady(app)
			if ok {
				visionReady = "true"
			} else {
				visionReady = "false"
			}
		} else {
			visionReady = "false"
			visionModel = ""
		}
		visionAnalyzed = dbCount(app.db, "SELECT COUNT(*) FROM detections WHERE vision_count>0")
		if app.db != nil && app.db.db != nil {
			_ = app.db.db.QueryRow("SELECT COALESCE(vision_error,'') FROM detections WHERE vision_error<>'' ORDER BY vision_at DESC LIMIT 1").Scan(&visionErr)
			_ = app.db.db.QueryRow("SELECT COALESCE(AVG(vision_ms),0) FROM detections WHERE vision_count>0").Scan(&visionAvgMs)
			// Total = full client round-trip (network + server handler).
			// Only rows measured with the timing build count (old rows = 0).
			_ = app.db.db.QueryRow("SELECT COALESCE(AVG(vision_total_ms),0) FROM vision_results WHERE vision_count>0 AND vision_total_ms>0").Scan(&visionAvgTotalMs)
		}
	}
	status := "{" +
		`"streams_total":` + strconv.Itoa(total) +
		`,"streams_enabled":` + strconv.Itoa(enabled) +
		`,"streams_up":` + strconv.Itoa(up) +
		`,"backend_mem_alloc":` + strconv.FormatUint(mem.Alloc, 10) +
		`,"backend_mem_sys":` + strconv.FormatUint(mem.Sys, 10) +
		`,"go2rtc_mem_rss":` + strconv.FormatInt(procRSSBytes(go2rtcPID), 10) +
		`,"go2rtc_running":` + boolJSON(go2rtcRunning) +
		`,"go2rtc_http_ok":` + boolJSON(go2rtcHTTP) +
		`,"go2rtc_port":` + strconv.Itoa(app.cfg.Stream.Port) +
		`,"go2rtc_rtsp_port":` + strconv.Itoa(app.cfg.Stream.RtspPort) +
		"," + kvRaw("s3_enabled", boolJSON(s3UploadEnabled())) +
		"," + kvRaw("pushover_enabled", boolJSON(pushoverEnabled())) +
		"," + kvRaw("mqtt_enabled", boolJSON(app.cfg.Mqtt.Enabled)) +
		"," + kvRaw("mqtt_connected", boolJSON(mqttConn)) +
		"," + kv("mqtt_broker", mqttBroker) +
		"," + kvRaw("detection_enabled", boolJSON(detectionEnabled())) +
		"," + kvRaw("recording_enabled", boolJSON(recordingEnabled())) +
		"," + kvRaw("snapshots_enabled", boolJSON(snapshotsEnabled())) +
		"," + kvRaw("vision_enabled", boolJSON(visionEnabled())) +
		`,"vision_ready":` + boolJSON(visionReady == "true") +
		"," + kv("vision_model", visionModel) +
		"," + kv("vision_error_last", visionErr) +
		`,"vision_analyzed":` + strconv.FormatInt(visionAnalyzed, 10) +
		`,"vision_avg_ms":` + fmt.Sprintf("%.1f", visionAvgMs) +
		`,"vision_avg_total_ms":` + fmt.Sprintf("%.1f", visionAvgTotalMs) +
		"," + kv("record_mode", app.cfg.Record.Mode) +
		"," + kv("detection_source", app.cfg.Detection.Source) +
		`,"retention_days":` + strconv.Itoa(app.cfg.Server.RetentionDays) +
		"," + kv("webhook", app.cfg.Server.OnvifNotifyURL) +
		`,"uptime":` + strconv.FormatInt(now.Unix()-app.boot, 10) +
		`,"time":` + strconv.FormatInt(now.Unix(), 10) +
		"}"
	stats := "{" +
		`"events":` + strconv.FormatInt(eventsTotal, 10) +
		`,"detections":` + strconv.FormatInt(detsTotal, 10) +
		`,"disk_bytes":` + strconv.FormatInt(diskBytes, 10) +
		`,"screenshots":` + strconv.FormatInt(jpgs, 10) +
		`,"videos":` + strconv.FormatInt(mp4s, 10) +
		`,"s3_pending":` + strconv.FormatInt(s3pending, 10) +
		`,"locked":` + strconv.FormatInt(locked, 10) +
		`,"buffer_bytes":` + strconv.FormatInt(bufBytes, 10) +
		`,"data_free":` + strconv.FormatInt(dataFree, 10) +
		`,"data_total":` + strconv.FormatInt(dataTotal, 10) +
		`,"last_event":` + strconv.FormatInt(lastEvent, 10) +
		`,"last_detection":` + strconv.FormatInt(lastDet, 10) +
		"}"
	writeJSON(w, 200, `{"status":`+status+`,"stats":`+stats+`,`+workersJSON()+`,"cameras":[`+strings.Join(camArr, ",")+`]}`)
}

func handleDetectionMedia(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "GET" && r.Method != "HEAD" {
		methodNotAllowed(w)
		return
	}
	// Forms:
	//   /api/detections/{id}/snapshot        -> snapshot index 0 (primary)
	//   /api/detections/{id}/snapshot/{n}    -> snapshot index n (0-based)
	//   /api/detections/{id}/clip            -> clip
	var seg = strings.Split(path, "/")
	if len(seg) < 5 {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	var kind = seg[4]
	var idx = 0
	var idxSet = false
	if kind == "snapshot" && len(seg) >= 6 && seg[5] != "" {
		var iv, err = strconv.Atoi(seg[5])
		if err != nil || iv < 0 {
			writeJSON(w, 404, "{\"error\":\"not found\"}")
			return
		}
		idx = iv
		idxSet = true
	}
	// Direct lookup by id (no LIMIT truncation: old recordings keep serving).
	var rec, err = getDetection(app.db, id)
	if err != nil {
		writeJSON(w, 500, "{\"error\":\"db\"}")
		return
	}
	if rec == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	// Jail every served path inside data_dir: a poisoned DB row must not
	// become an arbitrary file read.
	var dataDir = app.cfg.Server.DataDir
	if dataDir == "" {
		dataDir = "./data"
	}
	var f string
	if kind == "snapshot" {
		if !idxSet || idx == 0 {
			f = rec.snapshotPath
		} else if rec.extraSnap != "" {
			var extras = strings.Split(rec.extraSnap, ",")
			if idx-1 < len(extras) {
				f = extras[idx-1]
			}
		}
		if f == "" || !pathInsideDir(dataDir, f) {
			writeJSON(w, 404, "{\"error\":\"not found\"}")
			return
		}
		if f != "" {
			// ?boxes=1 burns the YOLO boxes into the served JPEG (serve-time
			// only, the stored file is untouched). Only when the detection
			// has a positive result for THIS exact file (vision_snap).
			var qb = r.URL.Query().Get("boxes")
			if qb == "1" || strings.EqualFold(qb, "true") {
				if serveVisionBoxes(app, w, r, rec, f) {
					return
				}
				// fall back to the camera's own RTSP SEI boxes when no YOLO
				// result applies to this exact file
				if serveRTSPBoxes(w, r, rec, f) {
					return
				}
				// fall through to the plain file when no boxes apply
			}
			serveFile(w, r, f, "image/jpeg")
			return
		}
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	} else if kind == "clip" && rec.clipPath != "" {
		if !pathInsideDir(dataDir, rec.clipPath) {
			writeJSON(w, 404, "{\"error\":\"not found\"}")
			return
		}
		var clipType = "video/mp4"
		if strings.HasSuffix(strings.ToLower(rec.clipPath), ".ts") {
			clipType = "video/mp2t"
		}
		serveFile(w, r, rec.clipPath, clipType)
		return
	}
	writeJSON(w, 404, "{\"error\":\"not found\"}")
}

// serveVisionBoxes serves the snapshot file with YOLO boxes burned in.
// Returns false when no boxes apply (caller falls back to the plain file).
func serveVisionBoxes(app *App, w http.ResponseWriter, r *http.Request, rec *Detection, f string) bool {
	if app == nil || rec == nil || f == "" {
		return false
	}
	// Per-snapshot lookup: each file gets its own boxes (or none).
	vr, err := dbGetVisionResult(app.db, rec.id, f)
	if err != nil || vr == nil || vr.count <= 0 {
		return false
	}
	var dets = parseVisionDets(vr.json)
	if len(dets) == 0 {
		return false
	}
	var mt = recModTime(f)
	if vat := time.Unix(vr.at, 0); vat.After(mt) {
		mt = vat
	}
	cacheKey := f + "|" + mt.UTC().Format("20060102T150405") + "|" + strconv.FormatInt(vr.at, 10)
	if cached, ok := boxesCacheGet(cacheKey); ok && len(cached) > 0 {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Vision-Boxes", strconv.Itoa(len(dets)))
		w.Header().Set("X-Boxes-Cache", "hit")
		http.ServeContent(w, r, "snapshot-boxes.jpg", mt, bytes.NewReader(cached))
		return true
	}
	raw, rerr := os.ReadFile(f)
	if rerr != nil || len(raw) == 0 {
		return false
	}
	out, derr := drawVisionBoxes(raw, dets)
	if derr != nil {
		return false
	}
	boxesCacheSet(cacheKey, out)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Vision-Boxes", strconv.Itoa(len(dets)))
	w.Header().Set("X-Boxes-Cache", "miss")
	http.ServeContent(w, r, "snapshot-boxes.jpg", mt, bytes.NewReader(out))
	return true
}

// recModTime returns the file mtime for cache validators (zero on error).
func recModTime(f string) (t time.Time) {
	if st, err := os.Stat(f); err == nil {
		t = st.ModTime()
	}
	return t
}

// pathInsideDir reports whether target stays inside dir after cleaning.
// Rejects ".." escapes and absolute-path swaps. Resolves symlinks when the
// target exists so a poisoned DB row (detections/symlink->/etc/passwd) cannot
// escape via ServeContent following the link.
func pathInsideDir(dir, target string) bool {
	var cleanDir = filepath.Clean(dir)
	var cleanTarget = filepath.Clean(target)
	if cleanDir == "" || cleanTarget == "" || cleanTarget == cleanDir {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(cleanTarget); err == nil {
		cleanTarget = resolved
	}
	if resolvedDir, err := filepath.EvalSymlinks(cleanDir); err == nil {
		cleanDir = resolvedDir
	}
	var rel, err = filepath.Rel(cleanDir, cleanTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// serveFile streams a media file without loading it into RAM (clips can be
// hundreds of MB). http.ServeContent also handles Range requests, so the UI
// video player can seek without re-downloading the whole clip.
func setStaticSecurityHeaders(w http.ResponseWriter) {
	// Clickjacking/MIME hardening on every served asset and media file.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func serveFile(w http.ResponseWriter, r *http.Request, f string, ctype string) {
	if f == "" {
		writeJSON(w, 404, "{\"error\":\"missing\"}")
		return
	}
	var fh, oerr = os.Open(f)
	if oerr != nil {
		writeJSON(w, 404, "{\"error\":\"missing\"}")
		return
	}
	defer fh.Close()
	var st, serr = fh.Stat()
	if serr != nil || st.IsDir() {
		writeJSON(w, 404, "{\"error\":\"missing\"}")
		return
	}
	w.Header().Set("Content-Type", ctype)
	setStaticSecurityHeaders(w)
	http.ServeContent(w, r, st.Name(), st.ModTime(), fh)
}

func serveStatic(app *App, w http.ResponseWriter, r *http.Request, path string) {
	var root = app.cfg.Server.WebUIDir
	if root == "" {
		root = "webui/dist"
	}
	if path == "/" {
		path = "/index.html"
	}
	// Reject path traversal. r.URL.Path is decoded, so encoded %2e%2e is already
	// normalized to ".." here. Clean + join + containment check instead of
	// string concatenation so "/%2e%2e/" and symlink-adjacent tricks fail.
	if strings.Contains(path, "..") || strings.Contains(path, "\\") || !strings.HasPrefix(path, "/") {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("forbidden"))
		return
	}
	var clean = filepath.Clean(path)
	if clean == "." || strings.HasPrefix(clean, "..") {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("forbidden"))
		return
	}
	var full = filepath.Join(filepath.Clean(root), filepath.Clean("/"+clean))
	// filepath.Join cleans, but verify containment explicitly for clarity.
	if rel, rerr := filepath.Rel(filepath.Clean(root), full); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("forbidden"))
		return
	}
	// Streamed with validators instead of buffered with os.ReadFile: assets are
	// re-requested on every page load and come back as 304 when unchanged.
	var fh, err = os.Open(full)
	if err != nil {
		// SPA fallback: extensionless client routes (e.g. /recordings) serve
		// index.html so a refresh doesn't land on a bare 404.
		if !strings.Contains(filepath.Base(clean), ".") {
			if fh2, err2 := os.Open(filepath.Join(filepath.Clean(root), "index.html")); err2 == nil {
				if st2, serr2 := fh2.Stat(); serr2 == nil && !st2.IsDir() {
					defer fh2.Close()
					w.Header().Set("Content-Type", "text/html")
					setStaticSecurityHeaders(w)
					w.Header().Set("Cache-Control", "no-cache")
					http.ServeContent(w, r, st2.Name(), st2.ModTime(), fh2)
					return
				}
				fh2.Close()
			}
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not found"))
		return
	}
	defer fh.Close()
	var st, serr = fh.Stat()
	if serr != nil || st.IsDir() {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not found"))
		return
	}
	var ctype = "text/html"
	if strings.HasSuffix(path, ".js") {
		ctype = "application/javascript"
	}
	if strings.HasSuffix(path, ".css") {
		ctype = "text/css"
	}
	if strings.HasSuffix(path, ".svg") {
		ctype = "image/svg+xml"
	}
	if strings.HasSuffix(path, ".json") {
		ctype = "application/json"
	}
	if strings.HasSuffix(path, ".png") {
		ctype = "image/png"
	}
	if strings.HasSuffix(path, ".jpg") || strings.HasSuffix(path, ".jpeg") {
		ctype = "image/jpeg"
	}
	if strings.HasSuffix(path, ".mp4") {
		ctype = "video/mp4"
	}
	w.Header().Set("Content-Type", ctype)
	setStaticSecurityHeaders(w)
	// Always revalidate static assets: a redeploy must show up on the next
	// reload (the 300 s max-age previously kept stale app.js/style.css for
	// minutes after an update). ETag/Last-Modified still make this a cheap 304
	// when the file is unchanged. index.html was already no-cache.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, st.Name(), st.ModTime(), fh)
}
