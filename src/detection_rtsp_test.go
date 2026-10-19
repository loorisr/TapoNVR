package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// ---- SEI test builders (synthetic H.264 SEI NAL units) ----

// encSEIVar encodes a SEI payloadType/payloadSize value (0xFF run + terminator).
func encSEIVar(v int) []byte {
	var out []byte
	for v >= 255 {
		out = append(out, 0xFF)
		v -= 255
	}
	return append(out, byte(v))
}

// escapeEPB inserts H.264 emulation-prevention bytes (00 00 03) like an encoder.
func escapeEPB(b []byte) []byte {
	var out []byte
	var zeros int
	for _, c := range b {
		if zeros >= 2 && c <= 0x03 {
			out = append(out, 0x03)
			zeros = 0
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

// buildSEINAL builds a complete SEI NAL (header + one payload + stop bit).
func buildSEINAL(pte int, rawPayload []byte) []byte {
	var rbsp []byte
	rbsp = append(rbsp, encSEIVar(pte)...)
	rbsp = append(rbsp, encSEIVar(len(rawPayload))...)
	rbsp = append(rbsp, rawPayload...)
	rbsp = append(rbsp, 0x80) // rbsp trailing bits
	return append([]byte{0x06}, escapeEPB(rbsp)...)
}

func u16put(v float64) []byte {
	var n = int(v*10000 + 0.5)
	return []byte{byte(n >> 8), byte(n)}
}

func markerUnit(id int, x1, y1, x2, y2 float64) []byte {
	var u = []byte{byte(id)}
	u = append(u, u16put(x1)...)
	u = append(u, u16put(y1)...)
	u = append(u, u16put(x2)...)
	u = append(u, u16put(y2)...)
	return u
}

func markerPayload(units ...[]byte) []byte {
	var rec = []byte{0x01, byte(len(units)), 0xFF, 0xFF}
	for _, u := range units {
		rec = append(rec, u...)
	}
	return append([]byte(seiTagMarker), rec...)
}

func detectedRecord(x1, y1, x2, y2 float64, score float32) []byte {
	var r = make([]byte, 34)
	r[0], r[1] = 0x01, 0x01
	var put = func(off int, v float64) {
		var n = int(v*10000 + 0.5)
		r[off], r[off+1] = byte(n>>8), byte(n)
	}
	put(19, x1)
	put(21, y1)
	put(23, x2)
	put(25, y2)
	var bits = math.Float32bits(score)
	r[27], r[28], r[29], r[30] = byte(bits>>24), byte(bits>>16), byte(bits>>8), byte(bits)
	return append([]byte(seiTagDetectedBox), r...)
}

// ---- parser tests ----

func TestSEIMarkerBoxes(t *testing.T) {
	var nalu = buildSEINAL(5, markerPayload(
		markerUnit(4, 0.1, 0.2, 0.3, 0.4), // PEOPLE
		markerUnit(1, 0.0, 0.0, 1.0, 1.0), // MOTION
	))
	var boxes = seiBoxesFromAU([][]byte{{0x65, 0x01}, nalu}) // IDR NAL + SEI NAL
	if len(boxes) != 2 {
		t.Fatalf("boxes=%d want 2: %+v", len(boxes), boxes)
	}
	if boxes[0].Type != "person" || boxes[1].Type != "motion" {
		t.Fatalf("types wrong: %+v", boxes)
	}
	var b = boxes[0]
	if math.Abs(b.X1-0.1) > 1e-9 || math.Abs(b.Y1-0.2) > 1e-9 || math.Abs(b.X2-0.3) > 1e-9 || math.Abs(b.Y2-0.4) > 1e-9 {
		t.Fatalf("coords wrong: %+v", b)
	}
	if b.Score != 0 {
		t.Fatalf("marker boxes must have no score: %+v", b)
	}
}

func TestSEIMarkerSwappedCorners(t *testing.T) {
	var boxes = seiBoxesFromAU([][]byte{buildSEINAL(5, markerPayload(markerUnit(6, 0.9, 0.9, 0.1, 0.1)))})
	if len(boxes) != 1 {
		t.Fatalf("boxes=%d want 1", len(boxes))
	}
	var b = boxes[0]
	if b.Type != "vehicle" || b.X1 != 0.1 || b.Y1 != 0.1 || b.X2 != 0.9 || b.Y2 != 0.9 {
		t.Fatalf("corners not ordered: %+v", b)
	}
}

func TestSEIDetectedBox(t *testing.T) {
	var nalu = buildSEINAL(764, detectedRecord(0.0004, 0.741, 0.0606, 0.9991, 0.3074))
	var boxes = seiBoxesFromAU([][]byte{nalu})
	if len(boxes) != 1 {
		t.Fatalf("boxes=%d want 1: %+v", len(boxes), boxes)
	}
	var b = boxes[0]
	if b.Type != "motion" {
		t.Fatalf("detected-box type=%q want motion", b.Type)
	}
	if math.Abs(b.X2-0.0606) > 0.001 || math.Abs(b.Y2-0.9991) > 0.001 {
		t.Fatalf("coords wrong: %+v", b)
	}
	if math.Abs(b.Score-0.3074) > 0.001 {
		t.Fatalf("score wrong: %v", b.Score)
	}
}

func TestSEIDetectedBoxHeartbeatIgnored(t *testing.T) {
	// 18-byte heartbeat: rec[0]=1 rec[1]=0 -> no box.
	var heartbeat = []byte{0x01, 0x00, 0xff, 0xff, 0x70, 0x00, 0x00, 0x00, 0x00, 0x21, 0xd5, 0x03, 0x00, 0x00, 0x03, 0x00, 0x80, 0x00}
	var payload = append([]byte(seiTagDetectedBox), heartbeat...)
	if boxes := seiBoxesFromAU([][]byte{buildSEINAL(764, payload)}); len(boxes) != 0 {
		t.Fatalf("heartbeat should yield no boxes: %+v", boxes)
	}
}

func TestSEITypeLabel(t *testing.T) {
	cases := map[int]string{1: "motion", 2: "motion", 3: "motion", 4: "person", 5: "pet", 6: "vehicle", 7: "other", 8: "person", 11: "pet", 12: "other", 99: "other"}
	for id, want := range cases {
		if got := seiTypeLabel(id); got != want {
			t.Fatalf("seiTypeLabel(%d)=%q want %q", id, got, want)
		}
	}
}

func TestRBSPUnescape(t *testing.T) {
	if got := seiRBSPUnescape([]byte{0x00, 0x00, 0x03, 0x01}); string(got) != string([]byte{0x00, 0x00, 0x01}) {
		t.Fatalf("unescape wrong: % x", got)
	}
	if got := seiRBSPUnescape([]byte{0x00, 0x00, 0x03, 0x03}); string(got) != string([]byte{0x00, 0x00, 0x03}) {
		t.Fatalf("double EPB wrong: % x", got)
	}
}

func TestSEIIgnoresNonSEINAL(t *testing.T) {
	// An IDR slice containing bytes that look like a marker tag must be ignored.
	var fake = append([]byte{0x65}, []byte(seiTagMarker)...)
	if boxes := seiBoxesFromAU([][]byte{fake}); len(boxes) != 0 {
		t.Fatalf("non-SEI NAL parsed: %+v", boxes)
	}
}

func TestBoxesJSONRoundTrip(t *testing.T) {
	var boxes = []detBox{{Type: "person", X1: 0.1, Y1: 0.2, X2: 0.3, Y2: 0.4, Score: 0.5}}
	var raw = boxesJSON(boxes)
	if raw == "" {
		t.Fatal("boxesJSON empty")
	}
	var back = parseBoxesJSON(raw)
	if len(back) != 1 || back[0].Type != "person" || math.Abs(back[0].Score-0.5) > 1e-9 {
		t.Fatalf("round trip mismatch: %+v", back)
	}
	if parseBoxesJSON("") != nil || parseBoxesJSON("not json") != nil {
		t.Fatal("tolerant parse failed")
	}
}

// ---- RTP SEI depacketizer tests (Tapo sends SEI in their own AUs) ----

func TestSEIDepacketizerSingleNAL(t *testing.T) {
	var nalu = buildSEINAL(5, markerPayload(markerUnit(4, 0.1, 0.2, 0.3, 0.4)))
	var d seiRTPDepacketizer
	var boxes []detBox
	for _, n := range d.decode(nalu) {
		boxes = append(boxes, seiBoxesFromNAL(n)...)
	}
	if len(boxes) != 1 || boxes[0].Type != "person" {
		t.Fatalf("single-NAL SEI not parsed: %+v", boxes)
	}
}

func TestSEIDepacketizerFUA(t *testing.T) {
	var nalu = buildSEINAL(5, markerPayload(markerUnit(6, 0.5, 0.5, 0.9, 0.9)))
	// Split the NAL in two FU-A fragments (indicator 28, header S/E bits).
	var body = nalu[1:]
	var mid = len(body) / 2
	var f1 = append([]byte{0x7C, 0x80 | 0x06}, body[:mid]...) // start, type 6
	var f2 = append([]byte{0x7C, 0x40 | 0x06}, body[mid:]...) // end, type 6
	var d seiRTPDepacketizer
	if got := d.decode(f1); len(got) != 0 {
		t.Fatalf("start fragment must not yield a NAL: %+v", got)
	}
	var boxes []detBox
	for _, n := range d.decode(f2) {
		boxes = append(boxes, seiBoxesFromNAL(n)...)
	}
	if len(boxes) != 1 || boxes[0].Type != "vehicle" {
		t.Fatalf("FU-A SEI not reassembled: %+v", boxes)
	}
}

func TestSEIDepacketizerSTAPA(t *testing.T) {
	var nalu = buildSEINAL(5, markerPayload(markerUnit(1, 0, 0, 1, 1)))
	var stap = []byte{24, 0, byte(len(nalu))}
	stap = append(stap, nalu...)
	stap = append(stap, 0x65, 0xAA, 0xBB) // trailing non-SEI NAL: ignored
	var d seiRTPDepacketizer
	var boxes []detBox
	for _, n := range d.decode(stap) {
		boxes = append(boxes, seiBoxesFromNAL(n)...)
	}
	if len(boxes) != 1 || boxes[0].Type != "motion" {
		t.Fatalf("STAP-A SEI not parsed: %+v", boxes)
	}
}

func TestSEIDepacketizerIgnoresNonSEIFragments(t *testing.T) {
	var d seiRTPDepacketizer
	// A fragmented IDR (type 5) must not be buffered into a SEI NAL.
	var f = append([]byte{0x7C, 0x80 | 0x05}, []byte{0x01, 0x02, 0x03}...)
	if got := d.decode(f); len(got) != 0 || d.fu != nil {
		t.Fatalf("non-SEI fragment buffered: %+v fu=%x", got, d.fu)
	}
}

func TestSEIDepacketizerSTAPB(t *testing.T) {
	var nalu = buildSEINAL(5, markerPayload(markerUnit(4, 0.1, 0.1, 0.2, 0.2)))
	var stap = []byte{25, 0x00, 0x01} // STAP-B + DON
	stap = append(stap, 0, byte(len(nalu)))
	stap = append(stap, nalu...)
	var d seiRTPDepacketizer
	var boxes []detBox
	for _, n := range d.decode(stap) {
		boxes = append(boxes, seiBoxesFromNAL(n)...)
	}
	if len(boxes) != 1 || boxes[0].Type != "person" {
		t.Fatalf("STAP-B SEI not parsed: %+v", boxes)
	}
}

func TestSEIDepacketizerMTAP16(t *testing.T) {
	var nalu = buildSEINAL(5, markerPayload(markerUnit(6, 0.2, 0.2, 0.4, 0.4)))
	var mtap = []byte{26, 0, byte(len(nalu))}
	mtap = append(mtap, nalu...)
	mtap = append(mtap, 0x00, 0x00, 0x00) // DONB + 2-byte timestamp offset
	var d seiRTPDepacketizer
	var boxes []detBox
	for _, n := range d.decode(mtap) {
		boxes = append(boxes, seiBoxesFromNAL(n)...)
	}
	if len(boxes) != 1 || boxes[0].Type != "vehicle" {
		t.Fatalf("MTAP16 SEI not parsed: %+v", boxes)
	}
}

func TestSEIDepacketizerFUB(t *testing.T) {
	var nalu = buildSEINAL(5, markerPayload(markerUnit(5, 0.3, 0.2, 0.5, 0.6)))
	var body = nalu[1:]
	var mid = len(body) / 2
	var f1 = append([]byte{0x7D, 0x80 | 0x06, 0x00, 0x01}, body[:mid]...) // FU-B + DON
	var f2 = append([]byte{0x7D, 0x40 | 0x06, 0x00, 0x01}, body[mid:]...)
	var d seiRTPDepacketizer
	if got := d.decode(f1); len(got) != 0 {
		t.Fatalf("FU-B start fragment must not yield a NAL: %+v", got)
	}
	var boxes []detBox
	for _, n := range d.decode(f2) {
		boxes = append(boxes, seiBoxesFromNAL(n)...)
	}
	if len(boxes) != 1 || boxes[0].Type != "pet" {
		t.Fatalf("FU-B SEI not reassembled: %+v", boxes)
	}
}

// ---- detector state machine ----

func TestRTSPDetectorEmitOnce(t *testing.T) {
	withToggles(t, func() {
		setDetectionEnabled(false) // raw event only, no clip/snapshot
		setPushoverEnabled(false)
		var app = testGroupingApp(t)
		var d = newRTSPDetector(app, &CameraCfg{Name: "c"}, "camRTSP1")
		d.types = map[string]bool{"person": true}
		d.debounce = time.Hour // suppress re-emits within the window
		setCamActive("camRTSP1", false)
		defer setCamActive("camRTSP1", false)

		var boxes = []detBox{{Type: "person", X1: 0.1, Y1: 0.1, X2: 0.2, Y2: 0.2}}
		d.onBoxes(boxes)
		d.onBoxes(boxes) // within debounce: no second emit
		d.onBoxes(boxes)
		if !camIsActive("camRTSP1") {
			t.Fatal("camera should be active after boxes")
		}
		// The emit runs in a goroutine; poll for the single raw event.
		var got int64
		for i := 0; i < 50; i++ {
			got = dbCount(app.db, "SELECT COUNT(*) FROM onvif_events WHERE camera_id='camRTSP1'")
			if got >= 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if got != 1 {
			t.Fatalf("raw events=%d want exactly 1 (debounced)", got)
		}
		// A type not in the allow-list must be ignored entirely.
		d.onBoxes([]detBox{{Type: "vehicle", X1: 0, Y1: 0, X2: 1, Y2: 1}})
		if n := dbCount(app.db, "SELECT COUNT(*) FROM onvif_events WHERE camera_id='camRTSP1'"); n != 1 {
			t.Fatalf("filtered type produced an event: %d", n)
		}
	})
}

func TestRTSPDetectorReemitsAfterDebounce(t *testing.T) {
	withToggles(t, func() {
		setDetectionEnabled(false)
		setPushoverEnabled(false)
		var app = testGroupingApp(t)
		var d = newRTSPDetector(app, &CameraCfg{Name: "c"}, "camRTSP2")
		d.types = map[string]bool{"person": true}
		d.debounce = 20 * time.Millisecond
		setCamActive("camRTSP2", false)
		defer setCamActive("camRTSP2", false)
		var boxes = []detBox{{Type: "person", X1: 0.1, Y1: 0.1, X2: 0.2, Y2: 0.2}}
		d.onBoxes(boxes)
		time.Sleep(40 * time.Millisecond)
		d.onBoxes(boxes)
		var got int64
		for i := 0; i < 50; i++ {
			got = dbCount(app.db, "SELECT COUNT(*) FROM onvif_events WHERE camera_id='camRTSP2'")
			if got >= 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if got != 2 {
			t.Fatalf("raw events=%d want 2 after debounce elapsed", got)
		}
	})
}

func TestCamDetectionSource(t *testing.T) {
	var cfg = &Config{}
	cfg.Detection.Source = "onvif"
	if s := camDetectionSource(cfg, &CameraCfg{}); s != "onvif" {
		t.Fatalf("default=%q", s)
	}
	if s := camDetectionSource(cfg, &CameraCfg{DetectionSource: "rtsp"}); s != "rtsp" {
		t.Fatalf("override=%q", s)
	}
	if s := camDetectionSource(cfg, &CameraCfg{DetectionSource: "off"}); s != "off" {
		t.Fatalf("off=%q", s)
	}
	cfg.Detection.Source = "both"
	if s := camDetectionSource(cfg, &CameraCfg{}); s != "both" {
		t.Fatalf("global both=%q", s)
	}
	if !sourceHas("both", "rtsp") || !sourceHas("both", "onvif") || sourceHas("onvif", "rtsp") {
		t.Fatal("sourceHas wrong")
	}
}

// ---- live overlay broadcast ----

func TestLiveBoxesBroadcastThrottled(t *testing.T) {
	var d = &rtspDetector{camID: "camLive", camName: "live"}
	var ch = hub.subscribe()
	defer hub.unsubscribe(ch)

	var boxes = []detBox{{Type: "person", X1: 0.1, Y1: 0.2, X2: 0.3, Y2: 0.4}}
	var t0 = time.Now()
	d.broadcastLive(boxes, t0)
	d.broadcastLive(boxes, t0.Add(50*time.Millisecond)) // inside the window: dropped
	select {
	case msg := <-ch:
		if !strings.Contains(msg, `"kind":"boxes"`) || !strings.Contains(msg, `"camera_id":"camLive"`) || !strings.Contains(msg, `"type":"person"`) {
			t.Fatalf("unexpected payload: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no live boxes event")
	}
	select {
	case msg := <-ch:
		t.Fatalf("throttled event leaked: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
	// After the interval a new frame is broadcast.
	d.broadcastLive(boxes, t0.Add(liveBoxInterval))
	select {
	case msg := <-ch:
		if !strings.Contains(msg, `"kind":"boxes"`) {
			t.Fatalf("unexpected payload: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no live boxes event after the throttle window")
	}
}

func TestLiveBoxesIgnoresEmptyAndBadJSON(t *testing.T) {
	var ch = hub.subscribe()
	defer hub.unsubscribe(ch)
	var d = &rtspDetector{camID: "camLive2"}
	d.broadcastLive(nil, time.Now())
	d.broadcastLive([]detBox{}, time.Now())
	select {
	case msg := <-ch:
		t.Fatalf("empty boxes must not broadcast: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
	broadcastLiveBoxes("camLive2", "live", "not json")
	select {
	case msg := <-ch:
		t.Fatalf("invalid bbox must not broadcast: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}
