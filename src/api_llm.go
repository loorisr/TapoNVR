package main

import (
	"archive/zip"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// handleDetectionLLM runs or reads the vision-LLM description of a detection.
//
//	GET  /api/detections/{id}/llm                 -> {"llm":{...}|null,"pending":bool}
//	POST /api/detections/{id}/llm[?source=snapshots|video|both]
//	     -> 202 {"ok":true,"queued":true}; the result lands on the row (SSE
//	        "llm" event, MQTT republish). Bypasses the failure backoff and the
//	        llm.events filter, but not the llm on/off switch.
func handleDetectionLLM(app *App, w http.ResponseWriter, r *http.Request, path string) {
	var seg = strings.Split(path, "/")
	if len(seg) != 5 || seg[3] == "" {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	var id = seg[3]
	d, err := getDetection(app.db, id)
	if err != nil {
		writeJSON(w, 500, "{\"error\":\"db\"}")
		return
	}
	if d == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	switch r.Method {
	case "GET":
		writeJSON(w, 200, "{"+llmJSON(d)+`,"pending":`+boolJSON(llmIsPending(id))+"}")
	case "POST":
		if !llmEnabled() {
			writeJSON(w, 400, "{\"error\":\"llm analysis is disabled (llm.enabled / /api/llm/state)\"}")
			return
		}
		var source = app.cfg.LLM.Source
		if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source"))); q != "" {
			switch q {
			case "snapshots", "video", "both":
				source = q
			default:
				writeJSON(w, 400, "{\"error\":\"source must be snapshots, video or both\"}")
				return
			}
		}
		if !llmEnqueue(app, id, source) {
			if llmIsPending(id) {
				writeJSON(w, 202, "{\"ok\":true,\"queued\":true,\"already\":true}")
				return
			}
			writeJSON(w, 503, "{\"error\":\"llm queue full, retry later\"}")
			return
		}
		writeJSON(w, 202, "{\"ok\":true,\"queued\":true}")
	default:
		methodNotAllowed(w)
	}
}

// handleLLMState returns or toggles the vision-LLM switch.
//
//	GET  /api/llm/state  -> {"enabled":bool}
//	POST /api/llm/state?enabled=true|false
func handleLLMState(app *App, w http.ResponseWriter, r *http.Request) {
	serveBoolToggle(w, r, llmEnabled, setLLMEnabled)
}

// setAttachment marks a response as a download named after the file.
func setAttachment(w http.ResponseWriter, name string) {
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": name}); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
}

// wantsDownload reports whether ?download=1 (or true) was passed.
func wantsDownload(r *http.Request) bool {
	var v = r.URL.Query().Get("download")
	return v == "1" || strings.EqualFold(v, "true")
}

// safeFileStem keeps a camera name usable in a download file name.
func safeFileStem(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case unicode.IsLetter(c), unicode.IsDigit(c), c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "camera"
	}
	return b.String()
}

// handleDetectionDownload streams a zip with every media file of a detection
// (snapshots, extras, clip) plus detection.json (metadata, YOLO and LLM
// results). Media is stored uncompressed: JPEG/MP4 do not shrink further.
//
//	GET /api/detections/{id}/download
func handleDetectionDownload(app *App, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "GET" && r.Method != "HEAD" {
		methodNotAllowed(w)
		return
	}
	var seg = strings.Split(path, "/")
	if len(seg) != 5 || seg[3] == "" {
		writeJSON(w, 400, "{\"error\":\"bad path\"}")
		return
	}
	d, err := getDetection(app.db, seg[3])
	if err != nil {
		writeJSON(w, 500, "{\"error\":\"db\"}")
		return
	}
	if d == nil {
		writeJSON(w, 404, "{\"error\":\"not found\"}")
		return
	}
	var dataDir = app.cfg.Server.DataDir
	if dataDir == "" {
		dataDir = "./data"
	}
	var files []string
	for _, p := range append([]string{d.snapshotPath}, strings.Split(d.extraSnap, ",")...) {
		if p != "" {
			files = append(files, p)
		}
	}
	if d.clipPath != "" {
		files = append(files, d.clipPath)
	}
	var name = safeFileStem(d.camName) + "-" + friendlyTS(d.startedAt) + "-" + d.id + ".zip"
	secureHeaders(w)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "no-store")
	setAttachment(w, name)
	if r.Method == "HEAD" {
		w.WriteHeader(200)
		return
	}
	var zw = zip.NewWriter(w)
	defer zw.Close()
	var seen = map[string]bool{}
	for _, p := range files {
		// Same jail as the media endpoints: a poisoned row must not turn
		// into an arbitrary file read.
		if !pathInsideDir(dataDir, p) {
			continue
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			continue
		}
		st, serr := f.Stat()
		if serr != nil || st.IsDir() {
			f.Close()
			continue
		}
		var base = filepath.Base(p)
		if seen[base] {
			base = d.id + "-" + base
		}
		seen[base] = true
		hdr := &zip.FileHeader{Name: base, Method: zip.Store, Modified: st.ModTime()}
		zf, zerr := zw.CreateHeader(hdr)
		if zerr == nil {
			_, _ = io.Copy(zf, f)
		}
		f.Close()
		if zerr != nil {
			return
		}
	}
	if zf, zerr := zw.CreateHeader(&zip.FileHeader{Name: "detection.json", Method: zip.Deflate, Modified: time.Now()}); zerr == nil {
		_, _ = io.WriteString(zf, detJSON(d)+"\n")
	}
}
