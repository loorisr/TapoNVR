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

// pushNotify sends a Pushover notification. When send_snapshot is on and
// snapPath is a non-empty path to a readable JPEG, a compressed copy of the
// image is attached to the message via multipart/form-data.
// Sends are throttled to one per Pushover.MinIntervalS seconds (0 disables
// throttling). Errors are logged and swallowed. The throttle slot is reserved
// before the send (so concurrent bursts still collapse to one) but rolled back
// when the send fails, so a failure doesn't silence the next window.
func pushNotify(app *App, title string, message string, snapPath string) {
	var p = app.cfg.Alerts.Pushover
	if !pushoverEnabled() {
		return
	}
	if strings.TrimSpace(p.AppToken) == "" || strings.TrimSpace(p.UserKey) == "" {
		logIt(LWARN, "pushover not configured (app_token or user_key empty)", map[string]string{"title": title})
		return
	}
	var reserved int64
	if p.MinIntervalS > 0 {
		var now = time.Now().Unix()
		pushRateMtx.Lock()
		var elapsed = now - lastPushUnix
		if lastPushUnix != 0 && elapsed < int64(p.MinIntervalS) {
			pushRateMtx.Unlock()
			logIt(LDEBUG, "pushover throttled", map[string]string{"title": title, "elapsed": fmt.Sprintf("%d", elapsed)})
			return
		}
		reserved = now
		lastPushUnix = now
		pushRateMtx.Unlock()
	}
	// rollback releases a reserved throttle slot when the send did not succeed.
	var rollback = func() {
		if reserved == 0 {
			return
		}
		pushRateMtx.Lock()
		if lastPushUnix == reserved {
			lastPushUnix = 0
		}
		pushRateMtx.Unlock()
	}
	var snap = pushAttachment(p, snapPath)
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
		rollback()
		return
	}
	var respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		logIt(LWARN, "pushover send rejected", map[string]string{"status": resp.Status, "body": truncateForLog(string(respBody), 512)})
		rollback()
		return
	}
	logIt(LINFO, "pushover message sent", map[string]string{"title": title, "message": message, "with_snapshot": map[bool]string{true: "true", false: "false"}[snapOK], "attach_bytes": fmt.Sprintf("%d", len(snap))})
}
