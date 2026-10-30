package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------- alerts: Pushover ---------------------------

// pushRateMtx guards lastPushUnix: global send throttle shared by all cameras.
var pushRateMtx sync.Mutex
var lastPushUnix int64

// pushAttachMaxBytes caps the Pushover attachment size. A compressed copy
// still larger than this falls back to a text-only alert so the notification
// itself is never lost (Pushover rejects oversized attachments outright).
const pushAttachMaxBytes = 2 << 20

// compressSnapshotForPush decodes src (JPEG), downscales it so the long edge
// is at most maxPx pixels (0 keeps the original size, aspect preserved), and
// re-encodes it at quality. Only the returned copy is sent; the stored file
// is never modified. Returns nil when decoding/encoding fails so the caller
// can fall back to a text-only alert.
func compressSnapshotForPush(src []byte, maxPx int, quality int) []byte {
	if len(src) == 0 {
		return nil
	}
	if quality < 1 || quality > 100 {
		quality = 70
	}
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil
	}
	var b = img.Bounds()
	var w, h = b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	var dw, dh = w, h
	if maxPx > 0 && (w > maxPx || h > maxPx) {
		if w >= h {
			dw = maxPx
			dh = (h*maxPx + w/2) / w
		} else {
			dh = maxPx
			dw = (w*maxPx + h/2) / h
		}
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
	}
	var out image.Image = img
	if dw != w || dh != h {
		out = boxDownscale(img, b, dw, dh)
	}
	var buf bytes.Buffer
	if jerr := jpeg.Encode(&buf, out, &jpeg.Options{Quality: quality}); jerr != nil || buf.Len() == 0 {
		return nil
	}
	return buf.Bytes()
}

// boxDownscale averages each destination pixel's source block (area sampling).
// Good enough for alert thumbnails with no extra dependencies.
func boxDownscale(src image.Image, b image.Rectangle, dw, dh int) *image.RGBA {
	var dst = image.NewRGBA(image.Rect(0, 0, dw, dh))
	var sw, sh = b.Dx(), b.Dy()
	for y := 0; y < dh; y++ {
		var y0 = b.Min.Y + (y*sh)/dh
		var y1 = b.Min.Y + ((y+1)*sh)/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			var x0 = b.Min.X + (x*sw)/dw
			var x1 = b.Min.X + ((x+1)*sw)/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					var cr, cg, cb, _ = src.At(sx, sy).RGBA()
					r += uint64(cr >> 8)
					g += uint64(cg >> 8)
					bl += uint64(cb >> 8)
					n++
				}
			}
			dst.SetRGBA(x, y, color.RGBA{R: uint8(r / n), G: uint8(g / n), B: uint8(bl / n), A: 0xff})
		}
	}
	return dst
}

// pushAttachment returns the compressed snapshot bytes to attach, or nil for
// a text-only alert (send_snapshot off, unreadable file, undecodable image,
// or still oversized after compression). The stored file is never modified.
func pushAttachment(p PushoverCfg, snapPath string) []byte {
	if !p.SendSnapshot || snapPath == "" {
		return nil
	}
	// Size-capped: camera-supplied JPEGs are small; never buffer a huge file
	// into RAM for an alert attachment.
	if s, err := os.Stat(snapPath); err != nil || s.Size() == 0 || s.Size() > 20<<20 {
		return nil
	}
	raw, rerr := os.ReadFile(snapPath)
	if rerr != nil || len(raw) == 0 || len(raw) > 20<<20 {
		return nil
	}
	small := compressSnapshotForPush(raw, p.SnapshotMaxPx, p.SnapshotQuality)
	if small == nil || len(small) > pushAttachMaxBytes {
		if small != nil {
			logIt(LWARN, "pushover attachment too large, sending text-only", map[string]string{"bytes": fmt.Sprintf("%d", len(small))})
		}
		return nil
	}
	return small
}

// pushoverEventAllowed reports whether eventType may trigger a notification
// according to alerts.pushover.events. An empty list allows every type, so
// configs written before the filter existed keep notifying on all detections.
func pushoverEventAllowed(p PushoverCfg, eventType string) bool {
	if len(p.Events) == 0 {
		return true
	}
	var t = strings.ToLower(strings.TrimSpace(eventType))
	for _, e := range p.Events {
		if strings.EqualFold(strings.TrimSpace(e), t) {
			return true
		}
	}
	return false
}

// pushoverPickBest reports whether the attached frame should be chosen after
// the timed snapshot burst (YOLO class/confidence when vision is enabled,
// otherwise the largest matching SEI box with a sharpness fallback) instead of
// sending the trigger-frame snapshot immediately.
func pushoverPickBest(p PushoverCfg) bool {
	return strings.EqualFold(strings.TrimSpace(p.SnapshotPick), "best")
}

// detSourceLabel renders the stored detection source list for humans:
// "onvif,rtsp" -> "ONVIF + RTSP". Empty/unknown values are labelled so an old
// row never produces a blank "Source:" line.
func detSourceLabel(raw string) string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		switch strings.ToLower(s) {
		case "onvif":
			out = append(out, "ONVIF")
		case "rtsp":
			out = append(out, "RTSP")
		case "manual":
			out = append(out, "Manual")
		case "test":
			out = append(out, "Test")
		default:
			out = append(out, strings.ToUpper(s[:1])+s[1:])
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return strings.Join(out, " + ")
}

// ---------------- alerts: YOLO-assisted best snapshot ----------------

// visionVehicleLabels / visionPetLabels map COCO labels to the camera's coarse
// detection tags so a "vehicle" event can match car/truck/bus boxes.
var visionVehicleLabels = map[string]bool{
	"car": true, "truck": true, "bus": true, "motorcycle": true,
	"bicycle": true, "train": true, "boat": true, "airplane": true,
	"van": true, "suv": true,
}

var visionPetLabels = map[string]bool{
	"dog": true, "cat": true, "bird": true, "horse": true, "sheep": true,
	"cow": true, "elephant": true, "bear": true, "zebra": true, "giraffe": true,
}

// visionTagRank scores how well a YOLO label matches the detection's camera
// tags: 2 = specific match (person/vehicle/pet/custom), 1 = generic
// motion/other wildcard, 0 = no match. A specific match always beats a
// wildcard, so a merged "motion,person" event prefers a person box over a
// higher-confidence car box.
func visionTagRank(label string, tags []string) int {
	label = normalizeDetType(label)
	if label == "" {
		return 0
	}
	if len(tags) == 0 {
		return 1
	}
	var best int
	for _, tag := range tags {
		switch normalizeDetType(tag) {
		case "person":
			if label == "person" && best < 2 {
				best = 2
			}
		case "vehicle":
			if visionVehicleLabels[label] && best < 2 {
				best = 2
			}
		case "pet":
			if visionPetLabels[label] && best < 2 {
				best = 2
			}
		case "motion", "other":
			if best < 1 {
				best = 1
			}
		default:
			if label == normalizeDetType(tag) && best < 2 {
				best = 2
			}
		}
	}
	return best
}

// visionBoxArea returns the normalized box area (0..1).
func visionBoxArea(b visionBox) float64 {
	var x1, y1 = clamp01f(b.XMin), clamp01f(b.YMin)
	var x2, y2 = clamp01f(b.XMax), clamp01f(b.YMax)
	var w, h = x2 - x1, y2 - y1
	if w <= 0 || h <= 0 {
		return 0
	}
	return w * h
}

// visionMatch is the best YOLO detection for the camera tags of one snapshot.
type visionMatch struct {
	label   string
	conf    float64
	area    float64
	rank    int
	matched bool
}

// visionBestMatch returns the most relevant detection for tags: specific class
// matches first, then confidence, then box area.
func visionBestMatch(dets []visionDetection, tags []string) visionMatch {
	var best visionMatch
	for _, d := range dets {
		var rank = visionTagRank(d.Label, tags)
		if rank == 0 {
			continue
		}
		var area = visionBoxArea(d.BoxNorm)
		var better bool
		if !best.matched {
			better = true
		} else if rank != best.rank {
			better = rank > best.rank
		} else if d.Confidence > best.conf+0.005 {
			better = true
		} else if d.Confidence >= best.conf-0.005 && area > best.area {
			better = true
		}
		if better {
			best = visionMatch{label: d.Label, conf: d.Confidence, area: area, rank: rank, matched: true}
		}
	}
	return best
}

// visionTopDetection returns the highest-confidence box overall (used for the
// message when no box matches the camera tags).
func visionTopDetection(dets []visionDetection) (string, float64, float64, bool) {
	var label string
	var conf, area float64
	var ok bool
	for _, d := range dets {
		if !ok || d.Confidence > conf {
			label, conf, area, ok = d.Label, d.Confidence, visionBoxArea(d.BoxNorm), true
		}
	}
	return label, conf, area, ok
}

// Bounds for YOLO-assisted frame selection: analyze at most 12 candidates,
// four at a time, and never hold the alert longer than the budget.
const (
	visionPushMaxCandidates = 12
	visionPushWorkers       = 4
)

// visionResultsForPush returns a YOLO result per candidate path, reusing
// stored per-snapshot results and analyzing the rest concurrently. The alert
// is bounded by a short budget; late analyses still persist in the background.
func visionResultsForPush(app *App, det *Detection, cands []snapCandidate) map[string]*visionResult {
	var out = map[string]*visionResult{}
	if app == nil || det == nil || len(cands) == 0 {
		return out
	}
	var todo []snapCandidate
	for _, c := range cands {
		if c.path == "" || c.placeholder {
			continue
		}
		if _, ok := out[c.path]; ok {
			continue
		}
		if vr, err := dbGetVisionResult(app.db, det.id, c.path); err == nil && vr != nil && vr.err == "" {
			out[c.path] = &visionResult{Detections: parseVisionDets(vr.json), InferenceMs: vr.ms}
			continue
		}
		if len(todo) < visionPushMaxCandidates {
			todo = append(todo, c)
		}
	}
	if len(todo) == 0 {
		return out
	}
	// Per-request timeouts are bounded by vision.timeout_s; cap the whole
	// selection phase so a slow sidecar cannot delay the alert for minutes.
	var budget = time.Duration(app.cfg.Vision.TimeoutS) * time.Second
	if budget <= 0 || budget > 6*time.Second {
		budget = 6 * time.Second
	}
	type result struct {
		path string
		vr   *visionResult
	}
	var ch = make(chan result, len(todo))
	var sem = make(chan struct{}, visionPushWorkers)
	for _, c := range todo {
		go func(c snapCandidate) {
			sem <- struct{}{}
			defer func() { <-sem }()
			var vr, err = visionAnalyzeFile(app, c.path)
			if err != nil {
				visionNoteFailure()
				logIt(LDEBUG, "vision best-frame analyze failed", map[string]string{"id": det.id, "path": c.path, "error": err.Error()})
				return
			}
			visionNoteSuccess()
			if err := dbUpsertVisionResult(app.db, det.id, c.index, c.path, vr, "", time.Now().Unix()); err != nil {
				logIt(LWARN, "vision best-frame store failed", map[string]string{"id": det.id, "error": err.Error()})
			}
			ch <- result{path: c.path, vr: vr}
		}(c)
	}
	var deadline = time.After(budget)
	for i := 0; i < len(todo); i++ {
		select {
		case r := <-ch:
			out[r.path] = r.vr
		case <-deadline:
			logIt(LWARN, "vision best-frame selection timed out", map[string]string{"id": det.id, "analyzed": fmt.Sprintf("%d/%d", len(out), len(todo))})
			return out
		}
	}
	return out
}

// pickBestFromVisionResults ranks candidates by their YOLO match. It returns
// ok=false when no candidate has a detection matching the camera tags.
func pickBestFromVisionResults(cands []snapCandidate, tags []string, results map[string]*visionResult) (snapCandidate, *visionResult, bool) {
	var best snapCandidate
	var bestMatch visionMatch
	var bestOK bool
	for _, c := range cands {
		if c.path == "" || c.placeholder {
			continue
		}
		var vr = results[c.path]
		if vr == nil {
			continue
		}
		var m = visionBestMatch(vr.Detections, tags)
		if !m.matched {
			continue
		}
		var better bool
		if !bestOK {
			better = true
		} else if m.rank != bestMatch.rank {
			better = m.rank > bestMatch.rank
		} else if m.conf > bestMatch.conf+0.005 {
			better = true
		} else if m.conf >= bestMatch.conf-0.005 && m.area > bestMatch.area {
			better = true
		}
		if better {
			best, bestMatch, bestOK = c, m, true
		}
	}
	if !bestOK {
		return snapCandidate{}, nil, false
	}
	best.visionLabel = bestMatch.label
	best.visionConf = bestMatch.conf
	best.visionCount = len(results[best.path].Detections)
	return best, results[best.path], true
}

// pickBestSnapshotWithVision chooses the frame using YOLO when it is enabled:
// among candidates with a matching detection, the most specific class tag wins,
// then confidence, then box area. Without a usable YOLO match it falls back to
// the existing SEI-box/sharpness heuristic.
func pickBestSnapshotWithVision(app *App, det *Detection, cands []snapCandidate, tags []string) (snapCandidate, *visionResult) {
	if app == nil || det == nil || !visionShouldAnalyze(app) || len(cands) == 0 {
		return pickBestSnapshot(cands, tags), nil
	}
	var results = visionResultsForPush(app, det, cands)
	if len(results) == 0 {
		return pickBestSnapshot(cands, tags), nil
	}
	if best, vr, ok := pickBestFromVisionResults(cands, tags, results); ok {
		return best, vr
	}
	// No candidate matched the camera tags: keep the legacy pick and only
	// report a YOLO result when one exists for that exact frame.
	var fallback = pickBestSnapshot(cands, tags)
	var vr = results[fallback.path]
	if vr != nil {
		if label, conf, _, ok := visionTopDetection(vr.Detections); ok {
			fallback.visionLabel, fallback.visionConf = label, conf
		}
		fallback.visionCount = len(vr.Detections)
	}
	return fallback, vr
}

// pushDetailMessage appends the detection source, camera identification and
// YOLO result (with confidence) to the human message.
func pushDetailMessage(message string, d *Detection, pick snapCandidate) string {
	if d == nil {
		return message
	}
	var lines []string
	lines = append(lines, "Detection source: "+detSourceLabel(d.detSource))
	var parts []string
	if tags := detectionTags(d); len(tags) > 0 {
		parts = append(parts, "camera="+strings.Join(tags, ","))
	}
	if pick.visionLabel != "" && pick.visionConf > 0 {
		var p = fmt.Sprintf("YOLO=%s %.0f%%", pick.visionLabel, pick.visionConf*100)
		if pick.visionCount > 1 {
			p += fmt.Sprintf(" (%d boxes)", pick.visionCount)
		}
		parts = append(parts, p)
	}
	if len(parts) > 0 {
		lines = append(lines, "Identification: "+strings.Join(parts, " | "))
	}
	return message + "\n" + strings.Join(lines, "\n")
}

// pushTicket is a prepared Pushover send: config snapshot + reserved throttle
// slot. prepare returns ok=false when the alert is disabled, filtered or not
// configured, so callers never wait for a snapshot that will not be sent.
type pushTicket struct {
	cfg      PushoverCfg
	reserved int64
	ok       bool
}

func pushPrepare(app *App, eventType string, title string) pushTicket {
	var p = app.cfg.Alerts.Pushover
	if !pushoverEnabled() {
		return pushTicket{}
	}
	if !pushoverEventAllowed(p, eventType) {
		logIt(LDEBUG, "pushover event filtered", map[string]string{"type": eventType, "title": title})
		return pushTicket{}
	}
	if strings.TrimSpace(p.AppToken) == "" || strings.TrimSpace(p.UserKey) == "" {
		logIt(LWARN, "pushover not configured (app_token or user_key empty)", map[string]string{"title": title})
		return pushTicket{}
	}
	var reserved int64
	if p.MinIntervalS > 0 {
		var now = time.Now().Unix()
		pushRateMtx.Lock()
		var elapsed = now - lastPushUnix
		if lastPushUnix != 0 && elapsed < int64(p.MinIntervalS) {
			pushRateMtx.Unlock()
			logIt(LDEBUG, "pushover throttled", map[string]string{"title": title, "elapsed": fmt.Sprintf("%d", elapsed)})
			return pushTicket{}
		}
		reserved = now
		lastPushUnix = now
		pushRateMtx.Unlock()
	}
	return pushTicket{cfg: p, reserved: reserved, ok: true}
}

// rollback releases a reserved throttle slot when the send did not succeed.
func (t pushTicket) rollback() {
	if t.reserved == 0 {
		return
	}
	pushRateMtx.Lock()
	if lastPushUnix == t.reserved {
		lastPushUnix = 0
	}
	pushRateMtx.Unlock()
}

// pushNotify sends a Pushover notification for a detection event of type
// eventType, attaching snapPath when send_snapshot is on. Events not listed in
// alerts.pushover.events are silently skipped.
func pushNotify(app *App, eventType string, title string, message string, snapPath string) {
	var cands []snapCandidate
	if snapPath != "" {
		cands = append(cands, snapCandidate{path: snapPath})
	}
	pushNotifyCandidates(app, eventType, title, message, nil, cands)
}

func pushNotifyCandidates(app *App, eventType string, title string, message string, det *Detection, cands []snapCandidate) {
	var t = pushPrepare(app, eventType, title)
	if !t.ok {
		return
	}
	var tags = []string{eventType}
	if det != nil {
		if tl := detectionTagList(det); len(tl) > 0 {
			tags = tl
		}
	}
	pushSend(app, t, title, message, tags, cands, det)
}

// pushNotifyBest waits (bounded by maxWait) for the snapshot burst to finish,
// then attaches the best frame among the candidates. When YOLO is enabled the
// candidates are scored by class/confidence (fallback: SEI box/sharpness).
// When the burst/lookup is unavailable it falls back to the primary snapshot
// and finally to a text-only alert.
func pushNotifyBest(app *App, eventType string, title string, message string, det *Detection, set *detSnapSet, fallback string, maxWait time.Duration) {
	var t = pushPrepare(app, eventType, title)
	if !t.ok {
		return
	}
	var cands []snapCandidate
	if pushoverPickBest(t.cfg) && t.cfg.SendSnapshot && set != nil {
		set.wait(maxWait)
		cands = set.snapshot()
	}
	if len(cands) == 0 && fallback != "" {
		cands = append(cands, snapCandidate{path: fallback})
	}
	var tags = []string{eventType}
	if det != nil {
		if tl := detectionTagList(det); len(tl) > 0 {
			tags = tl
		}
	}
	pushSend(app, t, title, message, tags, cands, det)
}

// pushNotifyDetection is the detection-aware entry point used by the initial
// alert and by tag-promotion alerts. In "best" mode it reuses the in-flight
// candidate set (boxes included) or, when the set is gone, all stored snapshots
// (sharpness fallback). In "primary" mode it keeps the legacy single-snapshot
// behavior. Source and YOLO details are added to the message for both modes.
func pushNotifyDetection(app *App, eventType string, title string, message string, det *Detection, set *detSnapSet) {
	if det == nil {
		return
	}
	if !app.cfg.Alerts.Pushover.SendSnapshot {
		// No frame to pick: do not make the alert wait for the burst.
		pushNotifyCandidates(app, eventType, title, message, det, nil)
		return
	}
	if pushoverPickBest(app.cfg.Alerts.Pushover) {
		if set == nil {
			set = detSnapSetByID(det.id)
		}
		if set != nil {
			pushNotifyBest(app, eventType, title, message, det, set, det.snapshotPath, time.Duration(app.cfg.Alerts.Pushover.BestMaxWaitS)*time.Second)
			return
		}
		pushNotifyCandidates(app, eventType, title, message, det, detectionSnapshotCandidates(det))
		return
	}
	pushNotifyCandidates(app, eventType, title, message, det, []snapCandidate{{path: det.snapshotPath}})
}

// detectionTagList returns the detection's merged tags (primary first), used
// to choose which SEI box types matter for the attached frame.
func detectionTagList(d *Detection) []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, t := range strings.Split(d.detTypes, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 && d.detType != "" {
		out = append(out, d.detType)
	}
	return out
}

// Box-area bounds for frame selection: tiny boxes are noise, huge ones are a
// subject glued to the lens (or a foreground false positive).
const (
	pushMinBoxArea = 0.005
	pushMaxBoxArea = 0.85
	pushBoxAreaTie = 0.02
)

// pickBestSnapshot chooses the still that best shows the event: the largest
// matching SEI box wins; near-ties prefer a specific type (person/vehicle/pet)
// then the sharpest frame, then the most recent one. Without boxes it falls
// back to sharpness (exposure-penalized). Missing/unreadable files are ignored.
func pickBestSnapshot(cands []snapCandidate, tags []string) snapCandidate {
	var best snapCandidate
	var bestOK bool
	var bestArea float64
	var bestRank int
	var bestSharp float64
	var haveReal bool
	for _, c := range cands {
		if c.path != "" && !c.placeholder {
			haveReal = true
			break
		}
	}
	for _, c := range cands {
		if strings.TrimSpace(c.path) == "" {
			continue
		}
		if c.placeholder && haveReal {
			continue // never pick a "NO SIGNAL" placeholder over a real frame
		}
		if s, err := os.Stat(c.path); err != nil || s.Size() == 0 || s.Size() > 20<<20 {
			continue
		}
		raw, err := os.ReadFile(c.path)
		if err != nil || len(raw) == 0 {
			continue
		}
		var area, rank = bestSEIBoxArea(c.boxes, tags)
		var sharp = imageSharpness(raw)
		if !bestOK {
			best, bestOK, bestArea, bestRank, bestSharp = c, true, area, rank, sharp
			continue
		}
		var better bool
		switch {
		case bestArea == 0 && area > 0:
			better = true // a real box beats a frame with no detectable subject
		case bestArea > 0 && area == 0:
			better = false
		case area > bestArea+pushBoxAreaTie:
			better = true
		case area < bestArea-pushBoxAreaTie:
			better = false
		case rank != bestRank:
			better = rank > bestRank
		case sharp > bestSharp*1.02:
			better = true
		case sharp >= bestSharp*0.98 && c.at.After(best.at):
			better = true
		}
		if better {
			best, bestArea, bestRank, bestSharp = c, area, rank, sharp
		}
	}
	if !bestOK {
		return snapCandidate{}
	}
	return best
}

// bestSEIBoxArea returns the largest matching box area (normalized 0..1) and
// whether that box is a specific type (1) or a generic motion marker (0).
func bestSEIBoxArea(boxes []detBox, tags []string) (float64, int) {
	var best float64
	var rank int
	for _, b := range boxes {
		if len(tags) > 0 && !stringInFold(tags, b.Type) {
			continue
		}
		var x1, y1, x2, y2 = clamp01f(b.X1), clamp01f(b.Y1), clamp01f(b.X2), clamp01f(b.Y2)
		var w, h = x2 - x1, y2 - y1
		if w <= 0 || h <= 0 {
			continue
		}
		var area = w * h
		if area < pushMinBoxArea || area > pushMaxBoxArea {
			continue
		}
		var r int
		switch strings.ToLower(strings.TrimSpace(b.Type)) {
		case "person", "vehicle", "pet", "other":
			r = 1
		}
		if area > best || (area == best && r > rank) {
			best, rank = area, r
		}
	}
	return best, rank
}

func stringInFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

func clamp01f(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// imageSharpness returns a sharpness score (variance of the Laplacian) on a
// downscaled grayscale copy. A dark or blown-out frame is halved so a flat
// black frame cannot win a tie against a usable one.
func imageSharpness(raw []byte) float64 {
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return 0
	}
	var b = img.Bounds()
	var w, h = b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return 0
	}
	const maxSide = 320
	var nw, nh = w, h
	if w >= h && w > maxSide {
		nw = maxSide
		nh = h * maxSide / w
	} else if h > maxSide {
		nh = maxSide
		nw = w * maxSide / h
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	if nw != w || nh != h {
		img = boxDownscale(img, b, nw, nh)
		b = img.Bounds()
		w, h = b.Dx(), b.Dy()
	}
	if w < 3 || h < 3 {
		return 0
	}
	var gray = make([]float64, w*h)
	var sum float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, _ = img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			var l = 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(bl>>8)
			gray[y*w+x] = l
			sum += l
		}
	}
	var mean = sum / float64(w*h)
	var lapSum, lapSq float64
	var n int
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			var i = y*w + x
			var lap = 4*gray[i] - gray[i-w] - gray[i+w] - gray[i-1] - gray[i+1]
			lapSum += lap
			lapSq += lap * lap
			n++
		}
	}
	if n == 0 {
		return 0
	}
	var lapMean = lapSum / float64(n)
	var variance = lapSq/float64(n) - lapMean*lapMean
	if variance < 0 {
		variance = 0
	}
	if mean < 25 || mean > 230 {
		variance *= 0.5
	}
	return variance
}

// pushSend attaches the chosen frame and performs the HTTP request. Errors are
// logged and swallowed; the throttle slot is rolled back on failure.
func pushSend(app *App, t pushTicket, title string, message string, tags []string, cands []snapCandidate, det *Detection) {
	var p = t.cfg
	var pick snapCandidate
	var snap []byte
	if p.SendSnapshot && len(cands) > 0 {
		if det != nil && pushoverPickBest(p) && visionShouldAnalyze(app) {
			// Best mode with YOLO enabled: score the candidate frames and pick
			// the most relevant/highest-confidence match.
			pick, _ = pickBestSnapshotWithVision(app, det, cands, tags)
		} else if len(cands) == 1 {
			pick = cands[0] // nothing to choose: skip the extra decode
		} else {
			pick = pickBestSnapshot(cands, tags)
		}
		// Primary mode (or fallback): never block on a fresh analysis, but
		// report a result already stored by the async vision worker.
		if pick.path != "" && pick.visionLabel == "" && det != nil {
			if vr, err := dbGetVisionResult(app.db, det.id, pick.path); err == nil && vr != nil && vr.count > 0 {
				var dets = parseVisionDets(vr.json)
				pick.visionCount = len(dets)
				if m := visionBestMatch(dets, tags); m.matched {
					pick.visionLabel, pick.visionConf = m.label, m.conf
				} else if label, conf, _, ok := visionTopDetection(dets); ok {
					pick.visionLabel, pick.visionConf = label, conf
				}
			}
		}
		if pick.path != "" {
			snap = pushAttachment(p, pick.path)
		}
	}
	message = pushDetailMessage(message, det, pick)
	var snapOK = len(snap) > 0
	var ctype string
	var body io.Reader
	if snapOK {
		var boundary = fmt.Sprintf("taponvr-%d", time.Now().UnixNano())
		var buf = bytes.NewBufferString("")
		var part = func(name string, value string) {
			buf.WriteString("--" + boundary + "\r\n")
			buf.WriteString("Content-Disposition: form-data; name=\"" + name + "\"\r\n\r\n")
			buf.WriteString(value + "\r\n")
		}
		part("token", p.AppToken)
		part("user", p.UserKey)
		part("title", title)
		part("message", message)
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Disposition: form-data; name=\"attachment\"; filename=\"snapshot.jpg\"\r\n")
		buf.WriteString("Content-Type: image/jpeg\r\n\r\n")
		buf.Write(snap)
		buf.WriteString("\r\n--" + boundary + "--\r\n")
		ctype = "multipart/form-data; boundary=" + boundary
		body = buf
	} else {
		var f = url.Values{}
		f.Add("token", p.AppToken)
		f.Add("user", p.UserKey)
		f.Add("title", title)
		f.Add("message", message)
		ctype = "application/x-www-form-urlencoded"
		body = bytes.NewBufferString(f.Encode())
	}
	// Bounded client: a hung Pushover must not pin the alert goroutine, and the
	// response body must be closed+drained so connections are reused, not leaked.
	var client = &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post("https://api.pushover.net/1/messages.json", ctype, body)
	if err != nil {
		logIt(LWARN, "pushover send failed", map[string]string{"error": err.Error()})
		t.rollback()
		return
	}
	var respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		logIt(LWARN, "pushover send rejected", map[string]string{"status": resp.Status, "body": truncateForLog(string(respBody), 512)})
		t.rollback()
		return
	}
	var fields = map[string]string{
		"title":         title,
		"message":       message,
		"with_snapshot": map[bool]string{true: "true", false: "false"}[snapOK],
		"attach_bytes":  fmt.Sprintf("%d", len(snap)),
	}
	if det != nil {
		fields["det_source"] = det.detSource
	}
	if pick.visionLabel != "" {
		fields["vision_label"] = pick.visionLabel
		fields["vision_conf"] = fmt.Sprintf("%.2f", pick.visionConf)
		fields["vision_boxes"] = fmt.Sprintf("%d", pick.visionCount)
	}
	if snapOK && pick.path != "" {
		fields["picked"] = filepath.Base(pick.path)
		fields["picked_source"] = pick.source
		fields["picked_boxes"] = fmt.Sprintf("%d", len(pick.boxes))
		if !pick.at.IsZero() {
			fields["picked_age_ms"] = fmt.Sprintf("%d", time.Since(pick.at).Milliseconds())
		}
	}
	logIt(LINFO, "pushover message sent", fields)
}
