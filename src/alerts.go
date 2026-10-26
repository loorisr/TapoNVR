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
// the timed snapshot burst (largest matching SEI box, sharpness fallback)
// instead of sending the trigger-frame snapshot immediately.
func pushoverPickBest(p PushoverCfg) bool {
	return strings.EqualFold(strings.TrimSpace(p.SnapshotPick), "best")
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
	pushNotifyCandidates(app, eventType, title, message, cands)
}

func pushNotifyCandidates(app *App, eventType string, title string, message string, cands []snapCandidate) {
	var t = pushPrepare(app, eventType, title)
	if !t.ok {
		return
	}
	pushSend(app, t, title, message, []string{eventType}, cands)
}

// pushNotifyBest waits (bounded by maxWait) for the snapshot burst to finish,
// then attaches the best frame among the candidates. When the burst/lookup is
// unavailable it falls back to fallback (the primary snapshot) and finally to
// a text-only alert.
func pushNotifyBest(app *App, eventType string, title string, message string, detID string, set *detSnapSet, fallback string, maxWait time.Duration) {
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
	if detID != "" {
		if d, err := getDetection(app.db, detID); err == nil && d != nil {
			if tl := detectionTagList(d); len(tl) > 0 {
				tags = tl
			}
		}
	}
	pushSend(app, t, title, message, tags, cands)
}

// pushNotifyDetection is the detection-aware entry point used by the initial
// alert and by tag-promotion alerts. In "best" mode it reuses the in-flight
// candidate set (boxes included) or, when the set is gone, all stored snapshots
// (sharpness fallback). In "primary" mode it keeps the legacy single-snapshot
// behavior.
func pushNotifyDetection(app *App, eventType string, title string, message string, det *Detection, set *detSnapSet) {
	if det == nil {
		return
	}
	if !app.cfg.Alerts.Pushover.SendSnapshot {
		// No frame to pick: do not make the alert wait for the burst.
		pushNotify(app, eventType, title, message, "")
		return
	}
	if pushoverPickBest(app.cfg.Alerts.Pushover) {
		if set == nil {
			set = detSnapSetByID(det.id)
		}
		if set != nil {
			pushNotifyBest(app, eventType, title, message, det.id, set, det.snapshotPath, time.Duration(app.cfg.Alerts.Pushover.BestMaxWaitS)*time.Second)
			return
		}
		pushNotifyCandidates(app, eventType, title, message, detectionSnapshotCandidates(det))
		return
	}
	pushNotify(app, eventType, title, message, det.snapshotPath)
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
func pushSend(app *App, t pushTicket, title string, message string, tags []string, cands []snapCandidate) {
	var p = t.cfg
	var pick snapCandidate
	var snap []byte
	if p.SendSnapshot && len(cands) > 0 {
		if len(cands) == 1 {
			pick = cands[0] // nothing to choose: skip the extra decode
		} else {
			pick = pickBestSnapshot(cands, tags)
		}
		if pick.path != "" {
			snap = pushAttachment(p, pick.path)
		}
	}
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
	if snapOK && pick.path != "" {
		fields["picked"] = filepath.Base(pick.path)
	}
	logIt(LINFO, "pushover message sent", fields)
}
