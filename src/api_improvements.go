package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// workersJSON exposes pool depths for the Status tab (no /metrics endpoint).
func workersJSON() string {
	cq, cr, cd := clipStats()
	vq, vd, vw, ve := visionStats()
	vb, vs, vu := visionBackoffStats()
	return `"workers":{` +
		`"clip_queue":` + strconv.FormatInt(cq, 10) +
		`,"clip_running":` + strconv.FormatInt(cr, 10) +
		`,"clip_dropped":` + strconv.FormatInt(cd, 10) +
		`,"vision_queue":` + strconv.Itoa(vq) +
		`,"vision_dropped":` + strconv.FormatInt(vd, 10) +
		`,"vision_workers":` + strconv.Itoa(vw) +
		`,"vision_enabled":` + boolJSON(ve) +
		`,"vision_backoff":` + boolJSON(vb) +
		`,"vision_fail_streak":` + strconv.Itoa(vs) +
		`,"vision_backoff_until":` + strconv.FormatInt(vu, 10) +
		`}`
}

// handleSystemBackup serves GET /api/system/backup -> redacted config + db stats.
// No secrets: passwords, tokens, S3 keys are replaced with ***.
func handleSystemBackup(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	var redacted = "(unavailable)"
	if app != nil && app.cfg != nil {
		cp := *app.cfg
		cp.Server.Auth.Password = "***"
		cp.Server.OnvifNotifyToken = "***"
		cp.Storage.S3.AccessKey = "***"
		cp.Storage.S3.SecretKey = "***"
		cp.Alerts.Pushover.AppToken = "***"
		cp.Alerts.Pushover.UserKey = "***"
		cp.Vision.APIKey = "***"
		cp.Mqtt.Password = "***"
		for i := range cp.Cameras {
			cp.Cameras[i].Onvif.Password = "***"
			cp.Cameras[i].RtspURL = redactRTSPUserInfo(cp.Cameras[i].RtspURL)
		}
		if b, err := json.Marshal(cp); err == nil {
			redacted = string(b)
		}
	}
	var stats = "{}"
	if app != nil {
		stats = dbBackupJSON(app.db)
	}
	writeJSON(w, 200, `{"config":`+redacted+`,"db":`+stats+`,"data_dir_hint":"db.sqlite lives in server.data_dir; copy the whole data_dir for a full restore"}`)
}

// handleDetectionTimeline serves GET /api/detections/{id}/timeline ->
// {detection, events[], vision[]}: single payload for the event timeline view.
func handleDetectionTimeline(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) < 5 || seg[3] == "" {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	d, err := getDetection(app.db, id)
	if err != nil || d == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	events, _ := listOnvifEventsForDetection(app.db, id)
	var earr []string
	for i := range events {
		earr = append(earr, onvifEventJSON(&events[i]))
	}
	vres, _ := dbListVisionResults(app.db, id)
	var varr []string
	for i := range vres {
		varr = append(varr, visionResultJSON(d, &vres[i]))
	}
	writeJSON(w, 200, "{\"detection\":"+detJSON(d)+",\"events\":["+strings.Join(earr, ", ")+"],\"vision\":["+strings.Join(varr, ", ")+"]}")
}

// handleVisionExclude implements POST /api/detections/{id}/vision/exclude
// body {"snap_index":N,"label":"person"}: stores the override, removes matching
// boxes from stored vision JSON, updates count/top. Idempotent.
func handleVisionExclude(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) < 5 || seg[3] == "" {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		SnapIndex int    `json:"snap_index"`
		Label     string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Label) == "" {
		writeJSON(w, 400, "{\"error\":\"snap_index and label required\"}")
		return
	}
	d, err := getDetection(app.db, id)
	if err != nil || d == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	if err := dbAddVisionOverride(app.db, id, body.SnapIndex, strings.TrimSpace(body.Label)); err != nil {
		writeJSON(w, 500, "{\"error\":\"db\"}")
		return
	}
	overrides, _ := dbListVisionOverrides(app.db, id)
	if err := dbApplyVisionExclusion(app.db, id, body.SnapIndex, overrides); err != nil {
		writeJSON(w, 500, "{\"error\":\"db\"}")
		return
	}
	writeJSON(w, 200, "{\"ok\":true}")
}

// handleCameraDetection implements GET/POST /api/cameras/{id}/detection
// {source?, debounce_s?, hold_s?}: per-camera tuning with inherit semantics.
// debounce_s/hold_s null or absent = inherit global. Stored in runtime override
// maps (immediate effect) and in config when the keys exist in YAML.
func handleCameraDetection(app *App, w http.ResponseWriter, r *http.Request, path string) {
	var seg = strings.Split(path, "/")
	if len(seg) < 5 || seg[3] == "" {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var camID = seg[3]
	idx := -1
	for i := range app.cfg.Cameras {
		if fmt.Sprintf("cam%d", i) == camID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, 404, "{\"error\":\"no such camera\"}")
		return
	}
	cam := &app.cfg.Cameras[idx]
	if r.Method == "GET" {
		var body = "{" + kv("id", camID) +
			"," + kv("source", camDetectionSource(app.cfg, cam)) +
			`,"debounce_s":` + strconv.Itoa(camDebounceS(app.cfg, cam)) +
			`,"hold_s":` + strconv.Itoa(camHoldS(app.cfg, cam)) +
			`,"debounce_inherit":` + boolJSON(cam.DetectionDebounceS == nil) +
			`,"hold_inherit":` + boolJSON(cam.DetectionHoldS == nil) + "}"
		writeJSON(w, 200, body)
		return
	}
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Source    *string `json:"source"`
		DebounceS *int    `json:"debounce_s"`
		HoldS     *int    `json:"hold_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "{\"error\":\"bad json\"}")
		return
	}
	if body.Source != nil {
		s := strings.ToLower(strings.TrimSpace(*body.Source))
		switch s {
		case "onvif", "rtsp", "both", "off":
			cam.DetectionSource = s
		default:
			writeJSON(w, 400, "{\"error\":\"source must be onvif|rtsp|both|off\"}")
			return
		}
	}
	if body.DebounceS != nil {
		v := *body.DebounceS
		if v < 0 || v > 3600 {
			writeJSON(w, 400, "{\"error\":\"debounce_s must be 0..3600\"}")
			return
		}
		setCamDebounce(camID, v)
	}
	if body.HoldS != nil {
		v := *body.HoldS
		if v < 1 || v > 60 {
			writeJSON(w, 400, "{\"error\":\"hold_s must be 1..60\"}")
			return
		}
		setCamHold(camID, v)
	}
	logIt(LINFO, "camera detection tuning updated", map[string]string{"camera": cam.Name, "id": camID})
	var out = "{" + kv("id", camID) +
		"," + kv("source", camDetectionSource(app.cfg, cam)) +
		`,"debounce_s":` + strconv.Itoa(camDebounceS(app.cfg, cam)) +
		`,"hold_s":` + strconv.Itoa(camHoldS(app.cfg, cam)) + "}"
	writeJSON(w, 200, out)
}
