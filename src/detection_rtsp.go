package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// ---------------- detection: RTSP SEI markers -------------------
//
// Tapo cameras embed the AI detection type + bounding box as per-frame H.264
// SEI metadata (NAL type 6) inside the RTSP video stream, next to the decoded
// pixels — it is the same data the mobile app draws on the live view. Two
// SEI formats exist:
//
//   - "TPLINKMARKERBOX." (SEI payload type 5): type enum + normalized box.
//   - "IPC-DETECTED-BOX" (SEI payload type 764): box + score, no type enum.
//
// Coordinates are normalized to [0,1] and encoded as u16 BE / 10000. The
// bytes survive RTP depacketization unchanged (the gortsplib H.264 decoder
// keeps every NALU, SEI included), so no ffmpeg/re-encode is needed: we reuse
// the same pure-Go decode path as the rolling-buffer recorder.
//
// Reference: rtsp-detection-boxes.md.

const (
	seiTagMarker      = "TPLINKMARKERBOX."
	seiTagDetectedBox = "IPC-DETECTED-BOX"
)

// detBox is one parsed detection box (normalized coordinates).
type detBox struct {
	Type  string  `json:"type"`
	X1    float64 `json:"x1"`
	Y1    float64 `json:"y1"`
	X2    float64 `json:"x2"`
	Y2    float64 `json:"y2"`
	Score float64 `json:"score,omitempty"`
}

// seiDetectTypes maps the TPLINKMARKERBOX type enum to the detection labels
// already used across TapoNVR (onvifTopicType / DB / UI).
var seiDetectTypes = map[int]string{
	1:  "motion",  // MOTION
	2:  "motion",  // LINE_CROSS
	3:  "motion",  // AREA
	4:  "person",  // PEOPLE
	5:  "pet",     // PET
	6:  "vehicle", // VEHICLE
	7:  "other",   // PACKAGE
	8:  "person",  // VISITOR
	9:  "other",   // BABY
	10: "other",   // AIPIP
	11: "pet",     // ANIMAL
	12: "other",   // FACE
}

// seiTypeLabel returns the project label for a SEI detection type id.
func seiTypeLabel(id int) string {
	if t, ok := seiDetectTypes[id]; ok {
		return t
	}
	return "other"
}

// seiRBSPUnescape removes H.264 emulation-prevention bytes (00 00 03 -> 00 00)
// from a NAL body. Skipping this corrupts every payload straddling the pattern.
func seiRBSPUnescape(b []byte) []byte {
	var out = make([]byte, 0, len(b))
	var zeros int
	for _, c := range b {
		if zeros >= 2 && c == 0x03 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// u16beFloat decodes a big-endian u16 scaled to [0,1] (raw / 10000).
func u16beFloat(b []byte) float64 {
	if len(b) < 2 {
		return 0
	}
	return float64(int(b[0])<<8|int(b[1])) / 10000.0
}

// normBox orders the corners so left<=right and top<=bottom.
func normBox(b detBox) detBox {
	if b.X2 < b.X1 {
		b.X1, b.X2 = b.X2, b.X1
	}
	if b.Y2 < b.Y1 {
		b.Y1, b.Y2 = b.Y2, b.Y1
	}
	return b
}

// parseMarkerBoxes parses the bytes after the "TPLINKMARKERBOX." tag.
func parseMarkerBoxes(rec []byte) []detBox {
	if len(rec) < 4 || rec[0] != 0x01 {
		return nil
	}
	var n = int(rec[1])
	if avail := (len(rec) - 4) / 9; n > avail {
		n = avail
	}
	var out []detBox
	for i := 0; i < n; i++ {
		var u = rec[4+9*i : 13+9*i]
		if len(u) < 9 {
			break
		}
		out = append(out, normBox(detBox{
			Type: seiTypeLabel(int(u[0])),
			X1:   u16beFloat(u[1:3]),
			Y1:   u16beFloat(u[3:5]),
			X2:   u16beFloat(u[5:7]),
			Y2:   u16beFloat(u[7:9]),
		}))
	}
	return out
}

// parseDetectedBox parses the bytes after the "IPC-DETECTED-BOX" tag. Only the
// 34-byte detection record carries a box (the 18-byte heartbeat has none).
func parseDetectedBox(rec []byte) (detBox, bool) {
	if len(rec) < 34 || rec[0] != 0x01 || rec[1] != 0x01 {
		return detBox{}, false
	}
	var score = float32(0)
	var bits = uint32(rec[27])<<24 | uint32(rec[28])<<16 | uint32(rec[29])<<8 | uint32(rec[30])
	score = math.Float32frombits(bits)
	return normBox(detBox{
		// This format carries no type enum; report it as motion.
		Type:  "motion",
		X1:    u16beFloat(rec[19:21]),
		Y1:    u16beFloat(rec[21:23]),
		X2:    u16beFloat(rec[23:25]),
		Y2:    u16beFloat(rec[25:27]),
		Score: float64(score),
	}), true
}

// seiBoxesFromNAL decodes every supported SEI payload in one NAL unit. The NAL
// must include its 1-byte header (nalu[0]&0x1F == 6).
func seiBoxesFromNAL(nalu []byte) []detBox {
	if len(nalu) < 2 || nalu[0]&0x1F != 6 {
		return nil
	}
	var d = seiRBSPUnescape(nalu[1:])
	var out []detBox
	var i int
	for i < len(d) {
		var pt int
		for i < len(d) && d[i] == 0xFF {
			pt += 0xFF
			i++
		}
		if i >= len(d) {
			break
		}
		pt += int(d[i])
		i++
		var ps int
		for i < len(d) && d[i] == 0xFF {
			ps += 0xFF
			i++
		}
		if i >= len(d) {
			break
		}
		ps += int(d[i])
		i++
		if ps <= 0 || i+ps > len(d) {
			break
		}
		var p = d[i : i+ps]
		i += ps
		switch {
		case pt == 5 && strings.HasPrefix(string(p), seiTagMarker):
			out = append(out, parseMarkerBoxes(p[len(seiTagMarker):])...)
		case pt == 764 && strings.HasPrefix(string(p), seiTagDetectedBox):
			if b, ok := parseDetectedBox(p[len(seiTagDetectedBox):]); ok {
				out = append(out, b)
			}
		}
	}
	return out
}

// seiBoxesFromAU scans a decoded access unit (list of NAL units) for SEI boxes.
func seiBoxesFromAU(au [][]byte) []detBox {
	var out []detBox
	for _, nalu := range au {
		if len(nalu) < 2 || nalu[0]&0x1F != 6 {
			continue
		}
		out = append(out, seiBoxesFromNAL(nalu)...)
	}
	return out
}

// boxesJSON marshals parsed boxes to the detection bbox_json column ("" when
// there is nothing to store).
func boxesJSON(boxes []detBox) string {
	if len(boxes) == 0 {
		return ""
	}
	raw, err := json.Marshal(boxes)
	if err != nil {
		return ""
	}
	return string(raw)
}

// parseBoxesJSON decodes the stored bbox_json (tolerant: "" / garbage -> nil).
func parseBoxesJSON(raw string) []detBox {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var out []detBox
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// ---------------- detection: RTSP SEI reader -------------------

// rtspCamStat tracks live RTSP-detection counters per camera for the status API.
type rtspCamStat struct {
	boxes     int64
	lastBox   int64
	lastType  string
	connected bool
}

var rtspStatsMtx sync.Mutex
var rtspStats = map[string]*rtspCamStat{}

func rtspStatGet(camID string) rtspCamStat {
	rtspStatsMtx.Lock()
	defer rtspStatsMtx.Unlock()
	if s, ok := rtspStats[camID]; ok {
		return *s
	}
	return rtspCamStat{}
}

func rtspStatBoxes(camID string, boxes []detBox, connected bool) {
	rtspStatsMtx.Lock()
	defer rtspStatsMtx.Unlock()
	s, ok := rtspStats[camID]
	if !ok {
		s = &rtspCamStat{}
		rtspStats[camID] = s
	}
	if len(boxes) > 0 {
		s.boxes += int64(len(boxes))
		s.lastBox = time.Now().Unix()
		s.lastType = boxes[0].Type
	}
	s.connected = connected
}

// detEventSem bounds concurrent handleRawDetectionEvent goroutines per camera
// (cap 4): a per-frame SEI flood must not spawn unbounded goroutines (each
// does slow snapshot capture). When busy, events are dropped + logged.
var detEventSemMtx sync.Mutex
var detEventSem = map[string]chan struct{}{}

func detEventAcquire(camID string) bool {
	detEventSemMtx.Lock()
	s, ok := detEventSem[camID]
	if !ok {
		s = make(chan struct{}, 4)
		detEventSem[camID] = s
	}
	detEventSemMtx.Unlock()
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func detEventRelease(camID string) {
	detEventSemMtx.Lock()
	s := detEventSem[camID]
	detEventSemMtx.Unlock()
	if s == nil {
		return
	}
	select {
	case <-s:
	default:
	}
}

// detectionRTSPStop closes the package-level stop channel so watchdog/readLoop
// goroutines exit on shutdown. Safe to call once via sync.Once semantics.
var detectionRTSPStopMtx sync.Mutex
var detectionRTSPStop = make(chan struct{})
var detectionRTSPStopped bool

// stopDetectionRTSP signals all RTSP detection loops to exit.
func stopDetectionRTSP() {
	detectionRTSPStopMtx.Lock()
	defer detectionRTSPStopMtx.Unlock()
	if detectionRTSPStopped {
		return
	}
	detectionRTSPStopped = true
	close(detectionRTSPStop)
	rtspDetMtx.Lock()
	rtspDetectors = map[string]*rtspDetector{}
	rtspDetMtx.Unlock()
}

func detectionRTSPStoppedCh() <-chan struct{} {
	return detectionRTSPStop
}

// rtspDetector holds the per-camera SEI state machine. A type is emitted at
// most once per debounce window; the active set (refreshed on every box) keeps
// the camera "active" until boxes stop for hold, so a scene that carries
// markers on every frame (e.g. a parked car) is bounded to one detection per
// window instead of a per-frame flood.
type rtspDetector struct {
	app         *App
	camID       string
	camName     string
	src         string
	preferRelay bool
	types       map[string]bool
	transport   string
	debounce    time.Duration
	hold        time.Duration

	mu       sync.Mutex
	active   map[string]time.Time
	lastEmit map[string]time.Time
	// lastBoxes/lastBoxesAt keep the most recent frame's boxes so a snapshot
	// captured by another goroutine can be paired with the SEI state at that
	// moment (Pushover "best frame" selection). All reported boxes are kept;
	// callers filter by detection type.
	lastBoxes   []detBox
	lastBoxesAt time.Time
	// boxHist keeps the last few seconds of SEI samples. A rolling-buffer
	// snapshot is the last IDR and can be one GOP older than the live frame;
	// seiBoxesAt uses this history to pair it with the boxes that were on
	// screen when that image was actually taken.
	boxHist []seiBoxSample
	// liveAt throttles the SSE "boxes" events feeding the live overlay:
	// SEI markers can arrive on every frame, but ~5 updates/s is plenty to
	// draw boxes without flooding the browser.
	liveAt time.Time
}

// liveBoxInterval is the minimum delay between two live-overlay box broadcasts
// for one camera (5 updates/s).
const liveBoxInterval = 200 * time.Millisecond

// SEI box history bounds: long enough to cover a typical GOP plus network
// jitter, capped so a high-frame-rate stream cannot grow it without bound.
const (
	seiBoxHistoryTTL = 30 * time.Second
	seiBoxHistoryMax = 2048
)

type seiBoxSample struct {
	at    time.Time
	boxes []detBox
}

// pushBoxHistoryLocked appends one SEI sample and prunes expired/excess
// entries. boxes must be an immutable copy owned by the detector (onBoxes
// passes d.lastBoxes), so no second copy is made per frame. Caller holds d.mu.
func (d *rtspDetector) pushBoxHistoryLocked(boxes []detBox, now time.Time) {
	d.boxHist = append(d.boxHist, seiBoxSample{at: now, boxes: boxes})
	var drop int
	for drop < len(d.boxHist) && now.Sub(d.boxHist[drop].at) > seiBoxHistoryTTL {
		d.boxHist[drop] = seiBoxSample{}
		drop++
	}
	if drop > 0 {
		copy(d.boxHist, d.boxHist[drop:])
		d.boxHist = d.boxHist[:len(d.boxHist)-drop]
	}
	if extra := len(d.boxHist) - seiBoxHistoryMax; extra > 0 {
		for i := 0; i < extra; i++ {
			d.boxHist[i] = seiBoxSample{}
		}
		copy(d.boxHist, d.boxHist[extra:])
		d.boxHist = d.boxHist[:seiBoxHistoryMax]
	}
}

// rtspDetMtx guards rtspDetectors: the running detector per camera, so the
// snapshot pipeline can read the current SEI boxes.
var rtspDetMtx sync.Mutex
var rtspDetectors = map[string]*rtspDetector{}

func registerRTSPDetector(d *rtspDetector) {
	if d == nil || d.camID == "" {
		return
	}
	rtspDetMtx.Lock()
	rtspDetectors[d.camID] = d
	rtspDetMtx.Unlock()
}

func unregisterRTSPDetector(camID string) {
	rtspDetMtx.Lock()
	delete(rtspDetectors, camID)
	rtspDetMtx.Unlock()
}

// latestSEIBoxes returns a copy of the last boxes seen for a camera and when
// they were seen. ok is false when no detector is running, no box was ever
// seen, or the newest box is older than maxAge (0 disables the age check).
// The caller pairs them with a snapshot: when the two are more than ~1s apart
// the boxes are considered unrelated to that image.
func latestSEIBoxes(camID string, maxAge time.Duration) ([]detBox, time.Time, bool) {
	rtspDetMtx.Lock()
	var d = rtspDetectors[camID]
	rtspDetMtx.Unlock()
	if d == nil {
		return nil, time.Time{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastBoxesAt.IsZero() || len(d.lastBoxes) == 0 {
		return nil, time.Time{}, false
	}
	if maxAge > 0 && time.Since(d.lastBoxesAt) > maxAge {
		return nil, d.lastBoxesAt, false
	}
	return append([]detBox(nil), d.lastBoxes...), d.lastBoxesAt, true
}

// seiBoxesAt returns the SEI sample closest to at within tolerance. Unlike
// latestSEIBoxes it can pair a rolling-buffer frame, whose image time is one
// GOP behind the live stream, with the boxes that were on screen at capture.
// ok is false with no detector/history or when the closest sample is too far.
func seiBoxesAt(camID string, at time.Time, tolerance time.Duration) ([]detBox, time.Time, bool) {
	if at.IsZero() {
		return nil, time.Time{}, false
	}
	rtspDetMtx.Lock()
	var d = rtspDetectors[camID]
	rtspDetMtx.Unlock()
	if d == nil {
		return nil, time.Time{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var best seiBoxSample
	var bestDelta time.Duration
	var found bool
	for _, s := range d.boxHist {
		var delta = s.at.Sub(at)
		if delta < 0 {
			delta = -delta
		}
		if !found || delta < bestDelta {
			best, bestDelta, found = s, delta, true
		}
	}
	if !found || len(best.boxes) == 0 {
		return nil, time.Time{}, false
	}
	if tolerance > 0 && bestDelta > tolerance {
		return nil, best.at, false
	}
	return append([]detBox(nil), best.boxes...), best.at, true
}

// streamSrc resolves the stream URL to read at this moment: the go2rtc relay
// when preferRelay is set and go2rtc is running (dynamic, so a late go2rtc
// start or a restart is picked up without recreating the detector), else the
// camera's own rtsp_url.
func (d *rtspDetector) streamSrc() string {
	if d.preferRelay && d.app != nil {
		if cam, _ := cameraByID(d.app, d.camID); cam != nil {
			return recordingsSource(d.app, cam, d.camID)
		}
	}
	return d.src
}

// broadcastLive sends the current frame's boxes to connected WebUI clients so
// the live view can draw them over the video. Throttled to liveBoxInterval;
// callers pass all parsed boxes (not just the detection allow-list) because
// the overlay is informational.
func (d *rtspDetector) broadcastLive(boxes []detBox, now time.Time) {
	// Nobody listening (or nothing to send): skip before touching the
	// throttle, so the first frame after a client connects goes out at once.
	if len(boxes) == 0 || !hub.hasSubscribers() {
		return
	}
	d.mu.Lock()
	if !d.liveAt.IsZero() && now.Sub(d.liveAt) < liveBoxInterval {
		d.mu.Unlock()
		return
	}
	d.liveAt = now
	d.mu.Unlock()
	broadcastLiveBoxes(d.camID, d.camName, boxesJSON(boxes))
}

func newRTSPDetector(app *App, cam *CameraCfg, camID string) *rtspDetector {
	var types = map[string]bool{}
	for _, t := range app.cfg.Detection.RTSP.Types {
		types[strings.ToLower(strings.TrimSpace(t))] = true
	}
	if len(types) == 0 {
		types = map[string]bool{"motion": true, "person": true, "vehicle": true, "pet": true}
	}
	var d = time.Duration(getCamDebounce(camID, camDebounceS(app.cfg, cam))) * time.Second
	var h = time.Duration(getCamHold(camID, camHoldS(app.cfg, cam))) * time.Second
	return &rtspDetector{
		app:         app,
		camID:       camID,
		camName:     cam.Name,
		src:         cam.RtspURL,
		preferRelay: app.cfg.Detection.RTSP.UseGo2rtc,
		types:       types,
		transport:   app.cfg.Detection.RTSP.Transport,
		debounce:    d,
		hold:        h,
		active:      map[string]time.Time{},
		lastEmit:    map[string]time.Time{},
	}
}

// ---------------- detection: per-camera runtime overrides -----------

// debounceOverride/holdOverride hold per-camera runtime overrides (seconds)
// for the RTSP debounce/hold windows. Unset cameras fall back to the global
// config default. No API here; callers set these programmatically.
var camOverrideMtx sync.Mutex
var debounceOverride = map[string]int{}
var holdOverride = map[string]int{}

// setCamDebounce sets the per-camera debounce window (seconds). Negative
// values are clamped to 0.
func setCamDebounce(camID string, v int) {
	if v < 0 {
		v = 0
	}
	camOverrideMtx.Lock()
	debounceOverride[camID] = v
	camOverrideMtx.Unlock()
}

// setCamHold sets the per-camera hold window (seconds). Values below 1 are
// clamped to 1.
func setCamHold(camID string, v int) {
	if v < 1 {
		v = 1
	}
	camOverrideMtx.Lock()
	holdOverride[camID] = v
	camOverrideMtx.Unlock()
}

// getCamDebounce returns the per-camera debounce override, or def when unset.
func getCamDebounce(camID string, def int) int {
	camOverrideMtx.Lock()
	v, ok := debounceOverride[camID]
	camOverrideMtx.Unlock()
	if !ok {
		return def
	}
	return v
}

// getCamHold returns the per-camera hold override, or def when unset.
func getCamHold(camID string, def int) int {
	camOverrideMtx.Lock()
	v, ok := holdOverride[camID]
	camOverrideMtx.Unlock()
	if !ok {
		return def
	}
	return v
}

// onBoxes feeds one frame's worth of boxes to the state machine and emits a
// detection event for every accepted type, at most once per debounce window.
// The active map (refreshed on every box) drives the hold-based watchdog that
// ends the clip's post-detection window.
func (d *rtspDetector) onBoxes(boxes []detBox) {
	var emit []string
	var now = time.Now()
	var accepted []detBox
	// Live overlay first: the UI draws every box the camera reports (even a
	// type filtered out of the detection allow-list), throttled internally.
	d.broadcastLive(boxes, now)
	// Effective debounce honors the per-camera runtime override so tuning
	// applies without recreating the detector (override wins over config).
	var baseDeb = int(d.debounce / time.Second)
	if d.app != nil && d.app.cfg != nil {
		if cam, _ := cameraByID(d.app, d.camID); cam != nil {
			baseDeb = camDebounceS(d.app.cfg, cam)
		}
	}
	var debounce = time.Duration(getCamDebounce(d.camID, baseDeb)) * time.Second
	d.mu.Lock()
	// Remember the frame's boxes for snapshot pairing (Pushover best-frame
	// selection). Stored before the allow-list filter: callers select the type
	// they need and the live overlay already reports everything.
	d.lastBoxes = append([]detBox(nil), boxes...)
	d.lastBoxesAt = now
	if len(d.lastBoxes) > 0 {
		d.pushBoxHistoryLocked(d.lastBoxes, now)
	}
	for _, b := range boxes {
		if !d.types[b.Type] {
			continue
		}
		accepted = append(accepted, b)
		d.active[b.Type] = now
		if last, ok := d.lastEmit[b.Type]; ok && now.Sub(last) < debounce {
			continue
		}
		d.lastEmit[b.Type] = now
		emit = append(emit, b.Type)
	}
	var active = len(d.active) > 0
	d.mu.Unlock()
	if len(accepted) > 0 {
		rtspStatBoxes(d.camID, accepted, true)
	}
	if !active {
		return
	}
	setCamActiveSource(d.camID, "rtsp", true)
	if len(emit) == 0 {
		return
	}
	// Persist the triggering frame's boxes on the detection, then run the
	// existing grouping/snapshot/record pipeline off the decode goroutine:
	// collectDetection can take seconds and must not stall RTP reads.
	var bjson = boxesJSON(accepted)
	for _, t := range emit {
		var dtype = t
		if !detEventAcquire(d.camID) {
			logIt(LWARN, "rtsp detection event dropped (busy)", map[string]string{"camera": d.camID, "type": dtype})
			continue
		}
		go func(dt, bj string) {
			defer detEventRelease(d.camID)
			handleRawDetectionEvent(d.app, d.camID, d.camName, dt, d.streamSrc(), "rtsp", bj)
		}(dtype, bjson)
	}
}

// watchdog clears a type once no box of it has been seen for hold_s and, when
// nothing is left active, releases the camera's active flag so the clip's
// post-detection window can finish.
func (d *rtspDetector) watchdog() {
	var ticker = time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-detectionRTSPStoppedCh():
			return
		case now := <-ticker.C:
			// Effective hold honors the per-camera runtime override (override
			// wins over config).
			var baseHold = int(d.hold / time.Second)
			if d.app != nil && d.app.cfg != nil {
				if cam, _ := cameraByID(d.app, d.camID); cam != nil {
					baseHold = camHoldS(d.app.cfg, cam)
				}
			}
			var hold = time.Duration(getCamHold(d.camID, baseHold)) * time.Second
			d.mu.Lock()
			for t, seen := range d.active {
				if now.Sub(seen) > hold {
					delete(d.active, t)
				}
			}
			var empty = len(d.active) == 0
			d.mu.Unlock()
			if empty {
				if camIsActive(d.camID) {
					setCamActiveSource(d.camID, "rtsp", false)
				}
			} else {
				setCamActiveSource(d.camID, "rtsp", true)
			}
		}
	}
}

// readLoop reconnects the RTSP reader with a short backoff until stopped.
func (d *rtspDetector) readLoop() {
	for {
		select {
		case <-detectionRTSPStoppedCh():
			return
		default:
		}
		d.readOnce()
		rtspStatBoxes(d.camID, nil, false)
		select {
		case <-detectionRTSPStoppedCh():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// readOnce opens one RTSP session, decodes H.264 access units and feeds any
// SEI boxes to onBoxes until the connection ends.
func (d *rtspDetector) readOnce() {
	if d.src == "" {
		logIt(LWARN, "rtsp detection: no stream url", map[string]string{"camera": d.camName})
		return
	}
	var src = d.streamSrc()
	u, err := base.ParseURL(src)
	if err != nil {
		logIt(LWARN, "rtsp detection url invalid", map[string]string{"camera": d.camName, "error": err.Error()})
		return
	}
	var proto = gortsplib.ProtocolTCP
	if strings.EqualFold(d.transport, "udp") {
		proto = gortsplib.ProtocolUDP
	}
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &proto}
	if err := c.Start(); err != nil {
		logIt(LWARN, "rtsp detection start failed", map[string]string{"camera": d.camName, "error": err.Error()})
		return
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		logIt(LWARN, "rtsp detection describe failed", map[string]string{"camera": d.camName, "error": err.Error()})
		return
	}
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		logIt(LWARN, "rtsp detection: no H264 stream", map[string]string{"camera": d.camName})
		return
	}
	// gortsplib's H.264 decoder is not used here: since v5.6.7 it drops
	// SEI-only access units whose timestamp equals the preceding frame, which
	// is exactly how Tapo cameras deliver their boxes (see the depacketizer
	// doc). Grab SEI NALs from the RTP packets directly instead.
	var seiDec seiRTPDepacketizer
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		logIt(LWARN, "rtsp detection setup failed", map[string]string{"camera": d.camName, "error": err.Error()})
		return
	}
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		var boxes []detBox
		for _, nalu := range seiDec.decode(pkt.Payload) {
			boxes = append(boxes, seiBoxesFromNAL(nalu)...)
		}
		if len(boxes) > 0 {
			d.onBoxes(boxes)
		}
	})
	if _, err := c.Play(nil); err != nil {
		logIt(LWARN, "rtsp detection play failed", map[string]string{"camera": d.camName, "error": err.Error()})
		return
	}
	logIt(LINFO, "rtsp detection connected", map[string]string{"camera": d.camName, "url": redactRTSPUserInfo(src)})
	_ = c.Wait()
	logIt(LWARN, "rtsp detection disconnected", map[string]string{"camera": d.camName})
}

// seiRTPDepacketizer extracts H.264 SEI NAL units straight from RTP packets.
// It replaces gortsplib's format.H264 decoder in the detection reader: since
// v5.6.7 that decoder deliberately drops SEI-only access units whose RTP
// timestamp equals the preceding frame's (to keep recorders clean), and Tapo
// cameras emit their boxes exactly that way — so using it would silently hide
// every box. Only SEI matters here, so single-NAL (6), STAP-A (24) and FU-A
// (28) packets are handled and everything else is ignored.
type seiRTPDepacketizer struct {
	fu []byte // FU-A reassembly buffer (SEI only)
}

// maxSEINALU bounds the FU-A reassembly buffer: a malformed or hostile stream
// must not grow it without limit.
const maxSEINALU = 64 * 1024

func (s *seiRTPDepacketizer) decode(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	switch payload[0] & 0x1F {
	case 6: // single NAL unit: SEI
		return [][]byte{payload}
	case 24: // STAP-A: [size u16][NAL] repeated
		return s.stap(payload, 1)
	case 25: // STAP-B: [DON u16][size u16][NAL] repeated
		if len(payload) < 3 {
			return nil
		}
		return s.stap(payload, 3)
	case 26, 27: // MTAP16/MTAP24: [size u16][NAL][DON u8][TS offset 2|3]
		var off = 2
		if payload[0]&0x1F == 27 {
			off = 3
		}
		var out [][]byte
		for i := 1; i+3 <= len(payload); {
			var n = int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if n == 0 || i+n > len(payload) {
				break
			}
			if payload[i]&0x1F == 6 {
				out = append(out, payload[i:i+n])
			}
			i += n + 1 + off // NAL + DONB + TS offset
		}
		return out
	case 28: // FU-A: [indicator][header][fragments...]
		if len(payload) < 3 {
			return nil
		}
		return s.fragment(payload, 2)
	case 29: // FU-B: [indicator][header][DON u16][fragments...]
		if len(payload) < 5 {
			return nil
		}
		return s.fragment(payload, 4)
	default: // non-SEI packets: nothing to collect
		s.fu = nil
		return nil
	}
}

// stap extracts SEI NALs from a STAP-A/STAP-B aggregation. start is the offset
// where the first [size][NAL] unit begins (1 for A, 3 for B).
func (s *seiRTPDepacketizer) stap(payload []byte, start int) [][]byte {
	var out [][]byte
	for i := start; i+2 <= len(payload); {
		var n = int(payload[i])<<8 | int(payload[i+1])
		i += 2
		if n == 0 || i+n > len(payload) {
			break
		}
		if payload[i]&0x1F == 6 {
			out = append(out, payload[i:i+n])
		}
		i += n
	}
	return out
}

// fragment reassembles a fragmented SEI NAL (FU-A/FU-B). dataOff is where the
// fragment bytes start (2 for FU-A, 4 for FU-B: DON is skipped).
func (s *seiRTPDepacketizer) fragment(payload []byte, dataOff int) [][]byte {
	var start = payload[1]&0x80 != 0
	var end = payload[1]&0x40 != 0
	var typ = payload[1] & 0x1F
	if typ != 6 { // fragmentation of a non-SEI NAL: not ours
		s.fu = nil
		return nil
	}
	if start {
		s.fu = append(s.fu[:0], payload[dataOff:]...)
	} else if s.fu != nil {
		if len(s.fu)+len(payload)-dataOff > maxSEINALU {
			s.fu = nil
			return nil
		}
		s.fu = append(s.fu, payload[dataOff:]...)
	} else {
		return nil
	}
	if !end {
		return nil
	}
	var nalu = make([]byte, len(s.fu)+1)
	nalu[0] = payload[0]&0xE0 | typ // original F/NRI bits + the real type
	copy(nalu[1:], s.fu)
	s.fu = nil
	return [][]byte{nalu}
}

// redactRTSPUserInfo hides rtsp://user:pass@ credentials in logs.
func redactRTSPUserInfo(raw string) string {
	var at = strings.LastIndex(raw, "@")
	var scheme = strings.Index(raw, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return raw
	}
	return raw[:scheme+3] + "***@" + raw[at+1:]
}

// detectionRTSPStart launches one SEI reader per enabled camera whose effective
// detection source includes rtsp. Independent of record.mode (event-mode users
// need a continuous reader to detect at all).
func detectionRTSPStart(app *App) {
	// Without the relay, detection AND recording each open a direct camera
	// session (plus snapshot fallbacks), which can exceed the small RTSP
	// session table of TP-Link cameras and wedge the main stream.
	if !app.cfg.Detection.RTSP.UseGo2rtc {
		var direct int
		for i := range app.cfg.Cameras {
			var cam = &app.cfg.Cameras[i]
			if cam.Enabled && cam.RtspURL != "" && sourceHas(camDetectionSource(app.cfg, cam), "rtsp") {
				direct++
			}
		}
		if direct > 0 {
			logIt(LWARN, "detection.rtsp.use_go2rtc is false: detection and recording open direct camera sessions; TP-Link cameras allow very few and can wedge", map[string]string{"cameras": strconv.Itoa(direct), "hint": "prefer use_go2rtc: true (the local relay multiplexes all consumers)"})
		}
	}
	for i := range app.cfg.Cameras {
		var cam = &app.cfg.Cameras[i]
		if !cam.Enabled || cam.RtspURL == "" {
			continue
		}
		if !sourceHas(camDetectionSource(app.cfg, cam), "rtsp") {
			continue
		}
		var camID = fmt.Sprintf("cam%d", i)
		var d = newRTSPDetector(app, cam, camID)
		registerRTSPDetector(d)
		go d.watchdog()
		go d.readLoop()
		logIt(LINFO, "rtsp detection reader started", map[string]string{"camera": cam.Name, "transport": d.transport})
		if i > 0 {
			time.Sleep(1 * time.Second)
		} // stagger startups
	}
}

// ---------------- detection: box overlay on snapshots -------------

// serveRTSPBoxes serves the primary snapshot with the RTSP SEI boxes burned in
// (serve-time only, the stored file is untouched). Returns false when no boxes
// apply for this file so the caller falls back to the plain JPEG.
func serveRTSPBoxes(w http.ResponseWriter, r *http.Request, rec *Detection, f string) bool {
	if rec == nil || f == "" || rec.snapshotPath == "" || f != rec.snapshotPath {
		return false
	}
	var boxes = parseBoxesJSON(rec.bboxJSON)
	if len(boxes) == 0 {
		return false
	}
	var mt = recModTime(f)
	var maxPx = boxRenderMax(r)
	var etag = smallETag(f, mt.UTC().Format("20060102T150405"), rec.bboxJSON, strconv.Itoa(maxPx))
	w.Header().Set("Cache-Control", "private, max-age=30")
	if serveNotModified(w, r, time.Time{}, etag) {
		return true
	}
	cacheKey := f + "|" + mt.UTC().Format("20060102T150405") + "|rtsp|max=" + strconv.Itoa(maxPx) + "|" + rec.bboxJSON
	if cached, ok := boxesCacheGet(cacheKey); ok && len(cached) > 0 {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-RTSP-Boxes", strconv.Itoa(len(boxes)))
		w.Header().Set("X-Boxes-Cache", "hit")
		http.ServeContent(w, r, "snapshot-rtsp-boxes.jpg", time.Time{}, bytes.NewReader(cached))
		return true
	}
	boxesRenderAcquire()
	raw, err := os.ReadFile(f)
	if err != nil || len(raw) == 0 {
		boxesRenderRelease()
		return false
	}
	out, derr := drawDetectionBoxesMax(raw, boxes, maxPx)
	boxesRenderRelease()
	if derr != nil {
		return false
	}
	boxesCacheSet(cacheKey, out)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-RTSP-Boxes", strconv.Itoa(len(boxes)))
	w.Header().Set("X-Boxes-Cache", "miss")
	http.ServeContent(w, r, "snapshot-rtsp-boxes.jpg", time.Time{}, bytes.NewReader(out))
	return true
}

// drawDetectionBoxes returns a JPEG with the RTSP boxes + type labels burned in
// (no confidence: marker boxes carry none). Input JPEG/PNG, output JPEG.
func drawDetectionBoxes(imgBytes []byte, boxes []detBox) ([]byte, error) {
	return drawDetectionBoxesMax(imgBytes, boxes, 0)
}

// drawDetectionBoxesMax is drawDetectionBoxes with an optional long-edge cap
// (0 = original size). The cap keeps thumbnail renders small enough to cache.
func drawDetectionBoxesMax(imgBytes []byte, boxes []detBox, maxPx int) ([]byte, error) {
	if len(boxes) == 0 {
		return nil, fmt.Errorf("no boxes")
	}
	img, _, err := image.Decode(bytes.NewReader(imgBytes))
	if err != nil {
		return nil, err
	}
	var b = img.Bounds()
	var w, h = b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("bad image size")
	}
	if int64(w)*int64(h) > maxAnnotatePixels {
		return nil, fmt.Errorf("image too large to annotate (%dx%d)", w, h)
	}
	var rgba = image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)
	var ox, oy = b.Min.X, b.Min.Y
	var thick = (w + h) / 2 / 300
	if thick < 2 {
		thick = 2
	}
	if thick > 6 {
		thick = 6
	}
	var scale = 2
	if w < 500 {
		scale = 1
	}
	var white = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	for _, bx := range boxes {
		var x1 = int(bx.X1 * float64(w))
		var y1 = int(bx.Y1 * float64(h))
		var x2 = int(bx.X2*float64(w) + 0.5)
		var y2 = int(bx.Y2*float64(h) + 0.5)
		if x2 <= x1 || y2 <= y1 {
			continue
		}
		if x1 < ox {
			x1 = ox
		}
		if y1 < oy {
			y1 = oy
		}
		if x2 > ox+w {
			x2 = ox + w
		}
		if y2 > oy+h {
			y2 = oy + h
		}
		var c = visionLabelColor(bx.Type)
		strokeRect(rgba, x1, y1, x2, y2, thick, c)
		var label = strings.ToUpper(bx.Type)
		if bx.Score > 0 {
			label += fmt.Sprintf(" %.2f", bx.Score)
		}
		var tw = len(label)*6*scale - scale
		var th = 7*scale + 2*scale
		var ty = y1 - th
		if ty < oy {
			ty = y1 + thick // no room above: put the tag inside
		}
		if tw > w {
			tw = w
		}
		if x1+tw > ox+w {
			x1 = ox + w - tw
		}
		fillRect(rgba, x1, ty, x1+tw, ty+th, c)
		drawVisionText(rgba, x1+scale, ty+scale, scale, label, white)
	}
	return encodeRGBAJPEG(rgba, maxPx)
}
