package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Eyevinn/hi264/pkg/decoder"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
	"github.com/pion/rtp"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------- records: FFmpeg capture ---------------------

// snapshotJpg returns a placeholder JPEG when real snapshot fails.
// It draws a 640x360 dark image with "NO SIGNAL" text so the user sees the failure is obvious.
func snapshotJpg() []byte {
	w, h := 640, 360
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{0x1a, 0x1a, 0x1a, 0xff}
	fg := color.RGBA{0xc0, 0xc0, 0xc0, 0xff}
	red := color.RGBA{0xaa, 0x2a, 0x2a, 0xff}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, bg)
		}
	}
	// border
	for x := 0; x < w; x++ {
		img.Set(x, 0, red)
		img.Set(x, 1, red)
		img.Set(x, h-1, red)
		img.Set(x, h-2, red)
	}
	for y := 0; y < h; y++ {
		img.Set(0, y, red)
		img.Set(1, y, red)
		img.Set(w-1, y, red)
		img.Set(w-2, y, red)
	}
	// simple 5x7 pixel font for "NO SIGNAL"
	font := map[rune][]string{
		'N': {"10001", "11001", "10101", "10011", "10001", "10001", "10001"},
		'O': {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
		' ': {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
		'S': {"01111", "10000", "01110", "00001", "00001", "10001", "01110"},
		'I': {"01110", "00100", "00100", "00100", "00100", "00100", "01110"},
		'G': {"01110", "10001", "10000", "10111", "10001", "10001", "01110"},
		'A': {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
		'L': {"10000", "10000", "10000", "10000", "10000", "10000", "01111"},
	}
	text := "NO SIGNAL"
	scale := 4
	startX := (w - len(text)*(6*scale)) / 2
	startY := h/2 - (7*scale)/2
	for i, ch := range text {
		rows, ok := font[ch]
		if !ok {
			continue
		}
		ox := startX + i*6*scale
		for row, bits := range rows {
			for col, bit := range bits {
				if bit == '1' {
					for sy := 0; sy < scale; sy++ {
						for sx := 0; sx < scale; sx++ {
							img.Set(ox+col*scale+sx, startY+row*scale+sy, fg)
						}
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	if buf.Len() > 0 {
		return buf.Bytes()
	}
	return []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01, 0x01, 0x00, 0x00, 0x01,
		0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43, 0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08,
		0x07, 0x07, 0x07, 0x09, 0x09, 0x08, 0x0a, 0x0c, 0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
		0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d, 0x1a, 0x1c, 0x1c, 0x20, 0x24, 0x2e, 0x27, 0x20,
		0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29, 0x2c, 0x30, 0x31, 0x33, 0x2f, 0x35, 0x39, 0x3c,
		0x38, 0x34, 0x34, 0x38, 0x3e, 0x3e, 0xfd, 0xdc, 0x00, 0x04, 0x01, 0x03, 0x02, 0x02, 0x03, 0x02,
		0x02, 0x03, 0x06, 0x08, 0x08, 0x06, 0x08, 0x0c, 0x0a, 0x0a, 0x0c, 0x1b, 0x11, 0x0c, 0x10, 0x1b,
		0x1f, 0x1a, 0x14, 0x0e, 0x1f, 0x20, 0x23, 0x1e, 0x23, 0x2f, 0x28, 0x24, 0x2f, 0x33, 0x34, 0x35,
		0x2b, 0x34, 0x2c, 0x3a, 0x3b, 0x3b, 0x40, 0x3c, 0x3b, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40,
		0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0xff, 0xc2, 0x00, 0x0b, 0x08, 0x00, 0x01, 0x00,
		0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4, 0x00, 0x1f, 0x00, 0x00, 0x01, 0x05, 0x01, 0x02, 0x03,
		0x04, 0x05, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00, 0x93, 0xff, 0xd9}
}

// ensureDir creates dir (and parents) and reports failure so callers never
// silently write into a missing tree.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		logIt(LWARN, "mkdir failed", map[string]string{"dir": dir, "error": err.Error()})
		return err
	}
	return nil
}

// writePlaceholderJpg writes the "NO SIGNAL" placeholder to dest.
func writePlaceholderJpg(dest string) error {
	return os.WriteFile(dest, snapshotJpg(), 0o640)
}

// cameraByID resolves camN to its config entry (nil when out of range).
func cameraByID(app *App, camID string) (*CameraCfg, int) {
	if app == nil || app.cfg == nil {
		return nil, -1
	}
	idx := -1
	if strings.HasPrefix(camID, "cam") {
		if n, err := strconv.Atoi(strings.TrimPrefix(camID, "cam")); err == nil {
			idx = n
		}
	}
	if idx < 0 || idx >= len(app.cfg.Cameras) {
		return nil, -1
	}
	return &app.cfg.Cameras[idx], idx
}

// isJPEG reports whether fetched bytes look like a JPEG (magic or content type).
func isJPEG(data []byte, ctype string) bool {
	if len(data) > 2 && data[0] == 0xff && data[1] == 0xd8 {
		return true
	}
	ct := strings.ToLower(ctype)
	return ct == "" || strings.Contains(ct, "jpeg") || strings.Contains(ct, "image")
}

// fetchSnapshotWithAuth GETs url with digest then basic auth (size-capped).
func fetchSnapshotWithAuth(url, user, pass string, timeout time.Duration) ([]byte, string, error) {
	if data, ctype, err := httpGetDigest(url, user, pass, timeout); err == nil && len(data) > 2 && isJPEG(data, ctype) {
		return data, ctype, nil
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.SetBasicAuth(user, pass)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("basic status %d", resp.StatusCode)
	}
	b, berr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if berr != nil {
		return nil, "", berr
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// snapshotCandidates returns the common HTTP snapshot URLs for a camera IP.
func snapshotCandidates(ip string) []string {
	if ip == "" {
		return nil
	}
	return []string{
		"http://" + ip + "/onvif/snapshot",
		"http://" + ip + "/snapshot.jpg",
		"http://" + ip + "/image.jpg",
		"http://" + ip + "/stream/snapshot.jpg",
		"http://" + ip + ":80/snapshot.jpg",
		"http://" + ip + ":8800/snapshot.jpg",
	}
}

// rtspProtoFor maps a transport name to a gortsplib protocol (default TCP).
func rtspProtoFor(transport string) *gortsplib.Protocol {
	p := gortsplib.ProtocolTCP
	if strings.EqualFold(strings.TrimSpace(transport), "udp") {
		p = gortsplib.ProtocolUDP
	}
	return &p
}

// newRTSPClient builds a gortsplib client with bounded timeouts and an
// explicit transport (TCP unless udp is requested).
func newRTSPClient(u *base.URL, transport string) gortsplib.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return gortsplib.Client{
		Scheme:       u.Scheme,
		Host:         u.Host,
		Protocol:     rtspProtoFor(transport),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		DialContext:  dialer.DialContext,
	}
}

// rtspOpTimeout caps one blocking RTSP operation (Describe/Setup/Play) so a
// hung camera cannot pin the caller: the op runs async and the first result
// or the timeout wins. NOTE: the op itself is not cancellable in gortsplib
// v5 (no ctx param); the timeout only bounds the wait, the client is closed
// by the caller afterwards.
func rtspOpTimeout(timeout time.Duration, op func() error) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("rtsp operation timeout after %s", timeout)
	}
}

// recordingsSource returns the stream URL to use for recording a camera. When
// go2rtc is running, its re-broadcast endpoint (rtsp://127.0.0.1:<port>/camN) is
// used so recording does NOT open an extra direct RTSP connection to the camera;
// otherwise it falls back to the camera's own rtsp_url.
func recordingsSource(app *App, cam *CameraCfg, camID string) string {
	if getStreamCmd() != nil {
		return fmt.Sprintf("rtsp://127.0.0.1:%d/%s", app.cfg.Stream.RtspPort, camID)
	}
	return cam.RtspURL
}

// ---------------- records: rolling video buffer ----------------

// BuffSeg is a single completed recording segment in the rolling buffer.
type BuffSeg struct {
	path  string
	start int64
}

// CameraBuffer holds the rolling ring-buffer state for one camera stream.
type CameraBuffer struct {
	camID  string
	src    string
	dir    string
	segSec int
	maxSeg int
	// pinKeep is the segment budget retained while a clip assembly is pinned:
	// enough to cover the whole window assembleClip may ask for, so pruning can
	// continue without deleting under an active assembly.
	pinKeep int
	mu      sync.Mutex
	segs    []BuffSeg
	run     int
	pins    int
}

var bufMtx sync.Mutex
var bufMap = map[string]*CameraBuffer{}

// manualRecState holds an ongoing manual video recording (toggle start/stop)
type manualRecState struct {
	cancel context.CancelFunc
	dest   string
	tmp    string
	start  int64
	id     string
	// done is closed once manualRecordLoop has flushed and closed its writer,
	// so stopManualRecord waits for the flush instead of guessing with a sleep.
	done chan struct{}
}

var manualRecMtx sync.Mutex
var manualRecMap = map[string]*manualRecState{}

func cameraBuffer(camID string) *CameraBuffer {
	bufMtx.Lock()
	defer bufMtx.Unlock()
	b, ok := bufMap[camID]
	if !ok {
		b = &CameraBuffer{camID: camID}
		bufMap[camID] = b
	}
	return b
}

// bufferBaseDir resolves where the rolling buffer lives. When record.buffer_dir is
// set it is used verbatim; otherwise we prefer a RAM-backed tmpfs so the
// continuously-recycled segments never write to durable flash.
func bufferBaseDir(app *App) string {
	if app.cfg.Record.BufferDir != "" {
		return app.cfg.Record.BufferDir
	}
	if _, e := os.Stat("/dev/shm"); e == nil {
		return "/dev/shm/taponvr/buffer"
	}
	return app.cfg.Server.DataDir + "/buffer"
}

// buffersStart launches one background recorder + watcher per buffer-mode camera.
func buffersStart(app *App) {
	if app.cfg.Record.Mode != "buffer" {
		return
	}
	var base = bufferBaseDir(app)
	for i := range app.cfg.Cameras {
		var cam = &app.cfg.Cameras[i]
		if !cam.Enabled || cam.RtspURL == "" {
			continue
		}
		var camID = fmt.Sprintf("cam%d", i)
		var b = cameraBuffer(camID)
		b.src = recordingsSource(app, cam, camID)
		b.dir = base + "/" + camID
		var seg = app.cfg.Record.PreDetection / 4
		if seg < 1 {
			seg = 1
		}
		if seg > 5 {
			seg = 5
		}
		b.segSec = seg
		b.maxSeg = (app.cfg.Record.PreDetection / seg) + 3
		// While a clip assembly is running we must retain the whole window it
		// may ask for: pre-roll + post-detection + assembleClip's safety waits
		// (up to ~120s while the camera reports the detection as active).
		b.pinKeep = ((app.cfg.Record.PreDetection + app.cfg.Record.PostDetection + 150) / seg) + 3
		// The ring is disposable: drop stale segments from a previous run so
		// fresh run/segIdx numbering can't collide with (and truncate) them.
		_ = os.RemoveAll(b.dir)
		if err := ensureDir(b.dir); err != nil {
			logIt(LWARN, "recording buffer dir unavailable, retrying", map[string]string{"camera": camID, "error": err.Error()})
			time.Sleep(3 * time.Second)
			continue
		}
		go bufferRecorder(app, b)
		go bufferWatcher(b)
	}
}

// tsSegmentWriter writes a single MPEG-TS segment file (video-only, no audio transcode).
// mu serializes the gortsplib RTP callback against teardown: the writer is
// closed from another goroutine and a packet in flight must not touch a closed
// bufio.Writer/File.
type tsSegmentWriter struct {
	mu           sync.Mutex
	f            *os.File
	b            *bufio.Writer
	w            *mpegts.Writer
	track        *mpegts.Track
	dtsExtractor *h264.DTSExtractor
	sps          []byte
	pps          []byte
}

func newTsSegmentWriter(path string, sps, pps []byte) (*tsSegmentWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	b := bufio.NewWriter(f)
	track := &mpegts.Track{Codec: &tscodecs.H264{}}
	w := &mpegts.Writer{W: b, Tracks: []*mpegts.Track{track}}
	if err := w.Initialize(); err != nil {
		f.Close()
		return nil, err
	}
	return &tsSegmentWriter{f: f, b: b, w: w, track: track, sps: sps, pps: pps}, nil
}

func (s *tsSegmentWriter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.b != nil {
		s.b.Flush()
		s.b = nil
	}
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

func (s *tsSegmentWriter) writeH264(au [][]byte, pts int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil || s.b == nil {
		return nil // already closed
	}
	var filtered [][]byte
	idrPresent := false
	nonIDRPresent := false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		typ := h264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case h264.NALUTypeSPS:
			s.sps = nalu
			continue
		case h264.NALUTypePPS:
			s.pps = nalu
			continue
		case h264.NALUTypeIDR:
			idrPresent = true
		case h264.NALUTypeNonIDR:
			nonIDRPresent = true
		}
		filtered = append(filtered, nalu)
	}
	au = filtered
	if au == nil || (!idrPresent && !nonIDRPresent) {
		return nil
	}
	if idrPresent && s.sps != nil && s.pps != nil {
		au = append([][]byte{s.sps, s.pps}, au...)
	}
	if s.dtsExtractor == nil {
		if !idrPresent {
			return nil
		}
		s.dtsExtractor = &h264.DTSExtractor{}
		s.dtsExtractor.Initialize()
	}
	dts, err := s.dtsExtractor.Extract(au, pts)
	if err != nil {
		return err
	}
	return s.w.WriteH264(s.track, pts, dts, au)
}

// bufferRecorder continuously writes the live RTSP stream as MPEG-TS segments (Go, no ffmpeg).
// Audio is dropped (PCMA cannot be muxed cleanly without AAC transcode).
func bufferRecorder(app *App, b *CameraBuffer) {
	for {
		b.mu.Lock()
		b.run++
		run := b.run
		b.mu.Unlock()

		u, err := base.ParseURL(b.src)
		if err != nil {
			logIt(LWARN, "recording start failed", map[string]string{"camera": b.camID, "error": err.Error()})
			time.Sleep(3 * time.Second)
			continue
		}
		c := newRTSPClient(u, app.cfg.Detection.RTSP.Transport)
		if err := rtspOpTimeout(10*time.Second, func() error { return c.Start() }); err != nil {
			logIt(LWARN, "recording start failed", map[string]string{"camera": b.camID, "error": err.Error()})
			time.Sleep(3 * time.Second)
			continue
		}
		var desc *description.Session
		derr := rtspOpTimeout(10*time.Second, func() error {
			d, _, e := c.Describe(u)
			if e == nil {
				desc = d
			}
			return e
		})
		if derr != nil {
			c.Close()
			logIt(LWARN, "recording describe failed", map[string]string{"camera": b.camID, "error": derr.Error()})
			time.Sleep(3 * time.Second)
			continue
		}
		var forma *format.H264
		medi := desc.FindFormat(&forma)
		if medi == nil {
			c.Close()
			logIt(LWARN, "recording no H264", map[string]string{"camera": b.camID})
			time.Sleep(3 * time.Second)
			continue
		}
		rtpDec, err := forma.CreateDecoder()
		if err != nil {
			c.Close()
			logIt(LWARN, "recording decoder failed", map[string]string{"camera": b.camID, "error": err.Error()})
			time.Sleep(3 * time.Second)
			continue
		}
		// segment state: segMu guards segWriter/segStart between the RTP
		// callback goroutine and teardown below (pointer race).
		var segIdx int
		var segMu sync.Mutex
		var segWriter *tsSegmentWriter
		var segStart time.Time
		var sps, pps = forma.SPS, forma.PPS
		segGet := func() (*tsSegmentWriter, time.Time) {
			segMu.Lock()
			defer segMu.Unlock()
			return segWriter, segStart
		}
		openSegment := func() error {
			segMu.Lock()
			defer segMu.Unlock()
			if segWriter != nil {
				segWriter.close()
				segWriter = nil
			}
			path := fmt.Sprintf("%s/%d_%05d.ts", b.dir, run, segIdx)
			w, err := newTsSegmentWriter(path, sps, pps)
			if err != nil {
				// Leave segWriter nil: writing to the closed old writer would
				// silently drop every packet until the next successful rotation.
				return err
			}
			segWriter = w
			segStart = time.Now()
			if sps != nil {
				w.sps = sps
			}
			if pps != nil {
				w.pps = pps
			}
			segIdx++
			return nil
		}
		closeSegment := func() {
			segMu.Lock()
			w := segWriter
			segWriter = nil
			segMu.Unlock()
			if w != nil {
				w.close()
			}
		}
		if err := openSegment(); err != nil {
			c.Close()
			logIt(LWARN, "recording segment open failed", map[string]string{"camera": b.camID, "error": err.Error()})
			time.Sleep(2 * time.Second)
			continue
		}
		if serr := rtspOpTimeout(10*time.Second, func() error {
			_, e := c.Setup(desc.BaseURL, medi, 0, 0)
			return e
		}); serr != nil {
			closeSegment()
			c.Close()
			logIt(LWARN, "recording setup failed", map[string]string{"camera": b.camID, "error": serr.Error()})
			time.Sleep(2 * time.Second)
			continue
		}
		c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
			pts, ok := c.PacketPTS(medi, pkt)
			if !ok {
				return
			}
			au, err := rtpDec.Decode(pkt)
			if err != nil {
				if !errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) && !errors.Is(err, rtph264.ErrMorePacketsNeeded) {
					// ignore
				}
				return
			}
			// rotate segment on wall-clock segSec
			_, start := segGet()
			var segAge = time.Since(start)
			if segAge >= time.Duration(b.segSec)*time.Second {
				// only rotate on IDR to keep segments decodable
				hasIDR := false
				for _, nalu := range au {
					if len(nalu) == 0 {
						continue
					}
					if h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeIDR {
						hasIDR = true
						break
					}
				}
				if hasIDR {
					if oerr := openSegment(); oerr != nil {
						logIt(LWARN, "recording segment rotate failed", map[string]string{"camera": b.camID, "error": oerr.Error()})
					}
				} else if segAge >= 3*time.Duration(b.segSec)*time.Second {
					// No IDR for 3 windows (very long GOP): force-rotate to
					// bound tmpfs growth. The new segment stays empty until
					// the next IDR arrives.
					logIt(LDEBUG, "recording force-rotate (no IDR)", map[string]string{"camera": b.camID})
					if oerr := openSegment(); oerr != nil {
						logIt(LWARN, "recording segment rotate failed", map[string]string{"camera": b.camID, "error": oerr.Error()})
					}
				}
			}
			w, _ := segGet()
			if w != nil {
				_ = w.writeH264(au, pts)
			}
		})
		if perr := rtspOpTimeout(10*time.Second, func() error {
			_, e := c.Play(nil)
			return e
		}); perr != nil {
			closeSegment()
			c.Close()
			logIt(LWARN, "recording play failed", map[string]string{"camera": b.camID, "error": perr.Error()})
			time.Sleep(2 * time.Second)
			continue
		}
		logIt(LINFO, "recording started", map[string]string{"camera": b.camID, "run": strconv.Itoa(run)})
		err = c.Wait()
		closeSegment()
		c.Close()
		logIt(LINFO, "recording stopped", map[string]string{"camera": b.camID, "run": strconv.Itoa(run), "error": fmt.Sprintf("%v", err)})
		time.Sleep(2 * time.Second)
	}
}

// bufferWatcher tracks new segments in delivery order and prunes the oldest so
// the ring stays bounded to roughly pre_detection seconds.
func bufferWatcher(b *CameraBuffer) {
	for {
		time.Sleep(400 * time.Millisecond)
		var entries, err = os.ReadDir(b.dir)
		if err != nil {
			continue
		}
		b.mu.Lock()
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".ts") {
				continue
			}
			var p = b.dir + "/" + e.Name()
			var have = false
			for _, s := range b.segs {
				if s.path == p {
					have = true
					break
				}
			}
			if !have {
				b.segs = append(b.segs, BuffSeg{path: p, start: time.Now().Unix()})
			}
		}
		// Prune the oldest segments (bound by count). While a clip is being
		// assembled (pins > 0) a bigger floor is kept so the segments that
		// assembly still needs are not deleted under it - but pruning must not
		// stop entirely: a long or stuck assembly would otherwise fill /dev/shm
		// until segment writes fail and recording silently drops.
		var keep = b.maxSeg
		if b.pins > 0 && b.pinKeep > keep {
			keep = b.pinKeep
		}
		for len(b.segs) > keep && len(b.segs) > 0 {
			_ = os.Remove(b.segs[0].path)
			b.segs = b.segs[1:]
		}
		b.mu.Unlock()
	}
}

// segmentEnd returns the wall-clock end time of a segment file (when its last
// bytes were written), used to place it on the recording timeline.
func segmentEnd(path string) int64 {
	if s, e := os.Stat(path); e == nil {
		return s.ModTime().Unix()
	}
	return time.Now().Unix()
}

// assembleClip joins the pre-event buffer window with the post-event footage
// into a single clip. Runs in the background; the clip is written to `out` and
// clipPending tracks detections whose buffer-mode clip is being assembled
// right now (assembleClip runs async after the post window). The UI shows a
// "assembling…" indicator instead of a dead Video button.
var clipPendingMtx sync.Mutex
var clipPending = map[string]bool{}

func markClipPending(id string) {
	if id == "" {
		return
	}
	clipPendingMtx.Lock()
	clipPending[id] = true
	clipPendingMtx.Unlock()
}

func unmarkClipPending(id string) {
	if id == "" {
		return
	}
	clipPendingMtx.Lock()
	delete(clipPending, id)
	clipPendingMtx.Unlock()
}

func isClipPending(id string) bool {
	clipPendingMtx.Lock()
	defer clipPendingMtx.Unlock()
	return clipPending[id]
}

// ---------------- records: bounded clip assembly pool -------------
//
// assembleClip runs async per detection and each instance pins the ring buffer
// plus tmpfs budget for up to ~2min. The pool bounds concurrent assemblies to
// 4; overflow waits in a bounded pending queue (cap 16) and is dropped with a
// WARN once the queue is full so a detection storm can't exhaust RAM.
var (
	clipPoolMtx    sync.Mutex
	clipPool       chan struct{}
	clipQueueDepth int64 // atomic: assemblies waiting for a pool slot
	clipRunning    int64 // atomic: assemblies holding a pool slot
	clipDropped    int64 // atomic: assemblies dropped (pending overflow)
)

const (
	clipPoolSize   = 4
	clipMaxPending = 16
)

func clipPoolInit() chan struct{} {
	clipPoolMtx.Lock()
	defer clipPoolMtx.Unlock()
	if clipPool == nil {
		clipPool = make(chan struct{}, clipPoolSize)
	}
	return clipPool
}

// clipPoolTryAcquire takes a pool slot without blocking. True = caller owns
// a slot and must call clipPoolRelease when done.
func clipPoolTryAcquire() bool {
	p := clipPoolInit()
	select {
	case p <- struct{}{}:
		atomic.AddInt64(&clipRunning, 1)
		return true
	default:
		return false
	}
}

// clipPoolRelease returns a previously acquired slot.
func clipPoolRelease() {
	clipPoolMtx.Lock()
	p := clipPool
	clipPoolMtx.Unlock()
	if p == nil {
		return
	}
	select {
	case <-p:
		atomic.AddInt64(&clipRunning, -1)
	default:
	}
}

// clipStats reports (pending queue depth, running assemblies, total dropped)
// for the status API.
func clipStats() (queue, running, dropped int64) {
	return atomic.LoadInt64(&clipQueueDepth),
		atomic.LoadInt64(&clipRunning),
		atomic.LoadInt64(&clipDropped)
}

// clipAssembleAsync runs assembleClip under the bounded pool: fast path takes
// a slot immediately; overflow waits for a slot while pending <= 16, and is
// dropped with a WARN beyond that.
func clipAssembleAsync(app *App, b *CameraBuffer, detID string, out string, eventT int64) {
	if clipPoolTryAcquire() {
		go func() {
			defer clipPoolRelease()
			assembleClip(app, b, detID, out, eventT)
		}()
		return
	}
	pending := atomic.AddInt64(&clipQueueDepth, 1)
	if pending > clipMaxPending {
		atomic.AddInt64(&clipQueueDepth, -1)
		atomic.AddInt64(&clipDropped, 1)
		logIt(LWARN, "clip assembly dropped (pool saturated)", map[string]string{"id": detID, "pending": strconv.FormatInt(pending, 10)})
		return
	}
	go func() {
		p := clipPoolInit()
		p <- struct{}{} // block until a slot frees
		atomic.AddInt64(&clipQueueDepth, -1)
		atomic.AddInt64(&clipRunning, 1)
		defer clipPoolRelease()
		assembleClip(app, b, detID, out, eventT)
	}()
}

// the detection row updated on success. `eventT` is the wall-clock detection
// time so the pre-roll is measured against the true event, not assembly time.
// While the camera reports the detection as active, the post-detection timer
// does not start - it begins only once the camera sends an inactive event.
func assembleClip(app *App, b *CameraBuffer, detID string, out string, eventT int64) {
	markClipPending(detID)
	defer unmarkClipPending(detID)
	var preSec = int64(app.cfg.Record.PreDetection)
	var afterSec = int64(app.cfg.Record.PostDetection)
	var preStart = eventT - preSec
	var segSec = int64(b.segSec)

	b.mu.Lock()
	b.pins++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.pins--
		b.mu.Unlock()
	}()

	// If the camera is still reporting the detection as active, defer the
	// post-detection countdown until it becomes inactive.
	var wasActive = false
	for {
		// Abort early if the detection was deleted meanwhile (cancellable
		// wait): no point holding the pin and tmpfs budget for a dead row.
		if cur, derr := getDetection(app.db, detID); derr == nil && cur == nil {
			logIt(LINFO, "clip discarded (detection deleted while waiting)", map[string]string{"id": detID})
			return
		}
		if !camIsActive(b.camID) {
			break
		}
		wasActive = true
		// safety: don't wait forever if camera never sends inactive (e.g. only true events)
		if time.Now().Unix()-eventT > 90 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	var afterStop int64
	if wasActive {
		afterStop = time.Now().Unix() + afterSec
	} else {
		afterStop = eventT + afterSec
	}
	// Extend with any grouped detections: if a new ONVIF event arrived while
	// still recording (now < activeRec.endAt), use the extended end time.
	if rec := getActiveRec(b.camID); rec != nil && rec.id == detID {
		if rec.endAt > afterStop {
			afterStop = rec.endAt
		}
	}

	// Wait until the post-event footage has been recorded, plus one extra
	// segment so the segment overlapping `afterStop` is complete.
	// Poll for extensions: while a new raw event extends the recording,
	// the activeRec.endAt moves forward and we keep waiting.
	var waitUntil = afterStop + segSec
	for {
		// check for extension from grouped events
		if rec := getActiveRec(b.camID); rec != nil && rec.id == detID {
			if rec.endAt+segSec > waitUntil {
				waitUntil = rec.endAt + segSec
				afterStop = rec.endAt
				// also update DB ended_at if it drifted (e.g. active wait)
				if _, err := app.db.db.Exec("UPDATE detections SET ended_at=? WHERE id=?", afterStop, detID); err != nil {
					logIt(LWARN, "db extend failed", map[string]string{"error": err.Error()})
				}
			}
		}
		// if motion becomes active again during the post window, defer post until it clears
		if camIsActive(b.camID) {
			var cand = time.Now().Unix() + afterSec + segSec
			if cand > waitUntil {
				waitUntil = cand
				afterStop = cand - segSec
				if _, err := app.db.db.Exec("UPDATE detections SET ended_at=? WHERE id=?", afterStop, detID); err != nil {
					logIt(LWARN, "db extend failed", map[string]string{"error": err.Error()})
				}
				// also bump activeRec so further extensions still work
				activeRecMtx.Lock()
				if r := activeRecMap[b.camID]; r != nil && r.id == detID {
					r.endAt = afterStop
					r.lastAt = time.Now().Unix()
				}
				activeRecMtx.Unlock()
			}
			// safety: don't wait forever if camera stays active
			if time.Now().Unix()-eventT > 120 {
				break
			}
		}
		if time.Now().Unix() >= waitUntil {
			break
		}
		// Cancellable wait: abort if the detection was deleted meanwhile.
		if cur, derr := getDetection(app.db, detID); derr == nil && cur == nil {
			logIt(LINFO, "clip discarded (detection deleted while waiting)", map[string]string{"id": detID})
			clearActiveRec(b.camID, detID)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Recording window finished; clear grouping state so next detection starts a new clip.
	clearActiveRec(b.camID, detID)

	// Select full segments that overlap the [preStart, afterStop] window.
	// A segment ending at `end` covers roughly [end-segSec, end]: the upper
	// bound admits one extra segment so the tail footage assembleClip waited
	// for (until afterStop+segSec) is actually included in the clip.
	var chosen []string
	b.mu.Lock()
	for _, s := range b.segs {
		var end = segmentEnd(s.path)
		if end >= preStart-segSec && end <= afterStop+int64(b.segSec) {
			chosen = append(chosen, s.path)
		}
	}
	b.mu.Unlock()
	if len(chosen) == 0 {
		return
	}
	// Try to transmux TS segments to MP4 (browser playable), fallback to TS concat.
	// The TS fallback keeps the real .ts extension so the file is served as
	// video/mp2t instead of unreadable TS bytes in a .mp4.
	if err := transmuxTsSegmentsToMp4(chosen, out); err != nil {
		var tsOut = out
		if i := strings.LastIndex(out, "."); i >= 0 {
			tsOut = out[:i] + ".ts"
		} else {
			tsOut = out + ".ts"
		}
		logIt(LWARN, "clip transmux failed, falling back to TS concat", map[string]string{"id": detID, "error": err.Error()})
		if err2 := concatTsSegments(chosen, tsOut); err2 != nil {
			_ = os.Remove(tsOut)
			_ = os.Remove(out)
			logIt(LWARN, "clip assemble failed", map[string]string{"id": detID, "segments": strconv.Itoa(len(chosen)), "error": err2.Error()})
			return
		}
		_ = os.Remove(out)
		out = tsOut
	}
	// The detection may have been deleted while we were recording (e.g. user
	// hit "delete all"): don't orphan a clip file with no DB row behind it.
	// A lookup *error* (transient SQLITE_BUSY/I/O) must not be treated as
	// "deleted": that would throw away a perfectly good clip while the row
	// still exists, leaving it with no media. Keep the file on error.
	if cur, derr := getDetection(app.db, detID); derr != nil {
		logIt(LWARN, "clip kept: detection lookup failed", map[string]string{"id": detID, "error": derr.Error()})
	} else if cur == nil {
		_ = os.Remove(out)
		logIt(LINFO, "clip discarded (detection deleted during recording)", map[string]string{"id": detID})
		return
	}
	logIt(LINFO, "clip assembled", map[string]string{"id": detID, "path": out, "segments": strconv.Itoa(len(chosen))})
	_ = dbSetClipPath(app.db, detID, out)
	// Refresh the cached total size now that the clip exists.
	if det, derr := getDetection(app.db, detID); derr == nil && det != nil {
		_ = dbSetSize(app.db, detID, detectionSize(det))
	}
	// Republish the full detection so MQTT subscribers receive the recording link.
	if det, derr := getDetection(app.db, detID); derr == nil && det != nil {
		mqttPublishDetection(det)
	}
	if s3UploadEnabled() {
		// Streamed from disk: a clip can be hundreds of MB and must never be
		// buffered as a whole just to be uploaded.
		if s3PutFile(app, s3KeyForPath(out), out, clipContentType(out)) == nil {
			if err := dbSetUploaded(app.db, detID); err != nil {
				logIt(LWARN, "db uploaded flag failed", map[string]string{"error": err.Error()})
			}
		} else {
			if _, err := app.db.db.Exec("UPDATE detections SET uploaded=0 WHERE id=?", detID); err != nil {
				logIt(LWARN, "db uploaded reset failed", map[string]string{"error": err.Error()})
			}
		}
	}
}

// clipContentType derives the S3/HTTP MIME type from the real file extension
// so a TS fallback (.ts) is never advertised as video/mp4.
func clipContentType(path string) string {
	if strings.HasSuffix(strings.ToLower(path), ".ts") {
		return "video/mp2t"
	}
	return "video/mp4"
}

// concatTsSegments concatenates TS files via io.Copy (MPEG-TS is designed for this).
func concatTsSegments(chosen []string, out string) error {
	fout, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fout.Close()
	for _, p := range chosen {
		fin, err := os.Open(p)
		if err != nil {
			continue
		}
		if _, err := io.Copy(fout, fin); err != nil {
			fin.Close()
			return err
		}
		fin.Close()
	}
	return nil
}

// transmuxTsSegmentsToMp4 reads H264 AUs from MPEG-TS segments and writes a single MP4 via pmp4.
// Video-only, no audio (PCMA dropped). Uses 90000 timescale, fixed frame duration.
// transmuxPayloadCap bounds transmux RAM: post-detection windows are short by
// config, so hitting this means a runaway window — truncate, don't OOM.
const transmuxPayloadCap = 512 << 20

func transmuxTsSegmentsToMp4(chosen []string, out string) error {
	type sample struct {
		pts     int64
		dts     int64
		isIDR   bool
		payload []byte // AVCC (the only retained copy; annexB AUs are dropped)
	}
	var samples []sample
	var payloadBytes int64
	var truncated bool
	var sps, pps []byte
	// Collect all AUs from all segments in order
	for _, p := range chosen {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		r := &mpegts.Reader{R: f}
		if err := r.Initialize(); err != nil {
			f.Close()
			continue
		}
		// Find H264 track (first one)
		tracks := r.Tracks()
		var h264Track *mpegts.Track
		for _, t := range tracks {
			if _, ok := t.Codec.(*tscodecs.H264); ok {
				h264Track = t
				break
			}
		}
		if h264Track == nil {
			f.Close()
			continue
		}
		r.OnDataH264(h264Track, func(pts int64, dts int64, au [][]byte) error {
			// capture sps/pps from AU for MP4 header
			for _, nalu := range au {
				if len(nalu) == 0 {
					continue
				}
				typ := h264.NALUType(nalu[0] & 0x1F)
				if typ == h264.NALUTypeSPS {
					sps = append([]byte(nil), nalu...)
				} else if typ == h264.NALUTypePPS {
					pps = append([]byte(nil), nalu...)
				}
			}
			// check IDR
			isIDR := false
			for _, nalu := range au {
				if len(nalu) == 0 {
					continue
				}
				if h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeIDR {
					isIDR = true
					break
				}
			}
			// convert annexB AU to AVCC payload for MP4 (4-byte length prefix)
			var avcc []byte
			for _, nalu := range au {
				// skip SPS/PPS – they go in codec header, not samples (but keep IDR filtered AU)
				t := h264.NALUType(nalu[0] & 0x1F)
				if t == h264.NALUTypeSPS || t == h264.NALUTypePPS {
					continue
				}
				avcc = append(avcc, byte(len(nalu)>>24), byte(len(nalu)>>16), byte(len(nalu)>>8), byte(len(nalu)))
				avcc = append(avcc, nalu...)
			}
			if len(avcc) == 0 {
				return nil
			}
			if payloadBytes+int64(len(avcc)) > transmuxPayloadCap {
				truncated = true
				return nil
			}
			payloadBytes += int64(len(avcc))
			samples = append(samples, sample{pts: pts, dts: dts, isIDR: isIDR, payload: avcc})
			return nil
		})
		// Read all packets
		for {
			err := r.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				break
			}
		}
		f.Close()
	}
	if len(samples) == 0 {
		return fmt.Errorf("no H264 samples found")
	}
	if sps == nil || pps == nil {
		return fmt.Errorf("missing SPS/PPS")
	}
	if truncated {
		logIt(LWARN, "transmux truncated at payload cap", map[string]string{"out": out, "cap_mb": "512"})
	}
	// Build pmp4 samples with durations derived from DTS diff
	// Timescale 90000, assume 30fps fallback 3000 if no diff
	var pmp4Samples []*pmp4.Sample
	for i, s := range samples {
		var dur uint32 = 3000
		if i+1 < len(samples) {
			diff := samples[i+1].dts - s.dts
			if diff > 0 && diff < 90000 {
				dur = uint32(diff)
			} else if diff > 0 {
				// clamp large gaps (e.g., across segments) to 3000
				dur = 3000
			}
		}
		ptsOffset := int32(s.pts - s.dts)
		pl := s.payload // capture
		pmp4Samples = append(pmp4Samples, &pmp4.Sample{
			Duration:        dur,
			PTSOffset:       ptsOffset,
			IsNonSyncSample: !s.isIDR,
			PayloadSize:     uint32(len(pl)),
			GetPayload: func() ([]byte, error) {
				return pl, nil
			},
		})
	}
	pres := &pmp4.Presentation{
		Tracks: []*pmp4.Track{{
			ID:        1,
			TimeScale: 90000,
			Codec: &mp4codecs.H264{
				SPS: sps,
				PPS: pps,
			},
			Samples: pmp4Samples,
		}},
	}
	fout, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fout.Close()
	return pres.Marshal(fout)
}

// recordClipGo records the RTSP stream for `durSec` seconds directly to MP4 (event mode, no buffer).
func recordClipGo(src string, durSec int, out string) error {
	if src == "" || durSec <= 0 {
		return fmt.Errorf("bad src/duration")
	}
	u, err := base.ParseURL(src)
	if err != nil {
		return err
	}
	c := newRTSPClient(u, "")
	if err := rtspOpTimeout(10*time.Second, func() error { return c.Start() }); err != nil {
		return err
	}
	defer c.Close()
	var desc *description.Session
	if derr := rtspOpTimeout(10*time.Second, func() error {
		d, _, e := c.Describe(u)
		if e == nil {
			desc = d
		}
		return e
	}); derr != nil {
		return derr
	}
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		return fmt.Errorf("no H264")
	}
	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		return err
	}
	_, err = func() (any, error) {
		var setupErr error
		setupErr = rtspOpTimeout(10*time.Second, func() error {
			_, e := c.Setup(desc.BaseURL, medi, 0, 0)
			return e
		})
		return nil, setupErr
	}()
	if err != nil {
		return err
	}
	type capSample struct {
		pts   int64
		dts   int64
		au    [][]byte
		isIDR bool
	}
	var samples []capSample
	var sps, pps = forma.SPS, forma.PPS
	var dtsExt *h264.DTSExtractor
	var mu sync.Mutex
	var done = make(chan error, 1)
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
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
		// capture sps/pps + DTS init/extract + append are a single critical
		// section: the timeout path reads samples/sps/pps/dtsExt under mu
		// after Close, so an in-flight callback must not touch them
		// concurrently.
		mu.Lock()
		for _, nalu := range au {
			typ := h264.NALUType(nalu[0] & 0x1F)
			if typ == h264.NALUTypeSPS {
				sps = append([]byte(nil), nalu...)
			} else if typ == h264.NALUTypePPS {
				pps = append([]byte(nil), nalu...)
			}
		}
		// filter empty
		hasVideo := false
		for _, nalu := range au {
			t := h264.NALUType(nalu[0] & 0x1F)
			if t == h264.NALUTypeIDR || t == h264.NALUTypeNonIDR {
				hasVideo = true
				break
			}
		}
		if !hasVideo {
			mu.Unlock()
			return
		}
		if dtsExt == nil {
			isIDR := false
			for _, nalu := range au {
				if h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeIDR {
					isIDR = true
					break
				}
			}
			if !isIDR {
				mu.Unlock()
				return
			}
			dtsExt = &h264.DTSExtractor{}
			dtsExt.Initialize()
		}
		dts, _ := dtsExt.Extract(au, pts)
		isIDR := false
		for _, nalu := range au {
			if h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeIDR {
				isIDR = true
				break
			}
		}
		// Bound retained samples with the transmux payload cap: a runaway
		// window truncates instead of OOMing (same budget as transmux).
		var auBytes int64
		for _, nalu := range au {
			auBytes += int64(len(nalu) + 4)
		}
		var curBytes int64
		for _, s := range samples {
			for _, nalu := range s.au {
				curBytes += int64(len(nalu) + 4)
			}
		}
		if curBytes+auBytes > transmuxPayloadCap {
			mu.Unlock()
			logIt(LWARN, "event capture truncated at payload cap", map[string]string{"cap_mb": "512"})
			return
		}
		samples = append(samples, capSample{pts: pts, dts: dts, au: au, isIDR: isIDR})
		mu.Unlock()
	})
	if perr := rtspOpTimeout(10*time.Second, func() error {
		_, e := c.Play(nil)
		return e
	}); perr != nil {
		return perr
	}
	// wait duration or error
	go func() {
		done <- c.Wait()
	}()
	select {
	case <-time.After(time.Duration(durSec) * time.Second):
		c.Close()
	case err := <-done:
		if err != nil {
			return err
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(samples) == 0 {
		return fmt.Errorf("no samples captured")
	}
	if sps == nil || pps == nil {
		return fmt.Errorf("missing SPS/PPS")
	}
	var pmp4Samples []*pmp4.Sample
	for i, s := range samples {
		var dur uint32 = 3000
		if i+1 < len(samples) {
			diff := samples[i+1].dts - s.dts
			if diff > 0 && diff < 90000 {
				dur = uint32(diff)
			}
		}
		// AVCC payload
		var avcc []byte
		for _, nalu := range s.au {
			t := h264.NALUType(nalu[0] & 0x1F)
			if t == h264.NALUTypeSPS || t == h264.NALUTypePPS {
				continue
			}
			avcc = append(avcc, byte(len(nalu)>>24), byte(len(nalu)>>16), byte(len(nalu)>>8), byte(len(nalu)))
			avcc = append(avcc, nalu...)
		}
		if len(avcc) == 0 {
			continue
		}
		pl := avcc
		pmp4Samples = append(pmp4Samples, &pmp4.Sample{
			Duration:        dur,
			PTSOffset:       int32(s.pts - s.dts),
			IsNonSyncSample: !s.isIDR,
			PayloadSize:     uint32(len(pl)),
			GetPayload:      func() ([]byte, error) { return pl, nil },
		})
	}
	if len(pmp4Samples) == 0 {
		return fmt.Errorf("no video samples")
	}
	pres := &pmp4.Presentation{
		Tracks: []*pmp4.Track{{
			ID:        1,
			TimeScale: 90000,
			Codec: &mp4codecs.H264{
				SPS: sps,
				PPS: pps,
			},
			Samples: pmp4Samples,
		}},
	}
	fout, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fout.Close()
	return pres.Marshal(fout)
}

// grabSnapshotNative takes a JPEG via ONVIF GetSnapshotUri + HTTP Digest.
// It tries multiple auth methods and fallback URLs, logs failures, and returns true on success.
func grabSnapshotNative(app *App, camID string, dest string) bool {
	cam, _ := cameraByID(app, camID)
	if cam == nil {
		return false
	}
	dev := onvifGet(cam)
	var uri = dev.snapshotURI()
	if uri == "" && dev.cachedErr() == nil && dev.profileToken() != "" {
		if raw, serr := onvifSoap(dev.mediaEndpoint(), onvifNsMedia+"/GetSnapshotUri",
			"<m:GetSnapshotUri xmlns:m=\""+onvifNsMedia+"\"><m:ProfileToken>"+xmlEscape(dev.profileToken())+"</m:ProfileToken></m:GetSnapshotUri>", cam.Onvif.Username, cam.Onvif.Password); serr == nil {
			if u := onvifSnapshotURI(raw); u != "" {
				dev.setSnapshotURI(u)
				uri = u
			}
		}
	}

	// fetch with digest then basic (timeout increased for Tapo which is slow to answer digest)
	fetchWithAuth := func(u string) ([]byte, string, error) {
		return fetchSnapshotWithAuth(u, cam.Onvif.Username, cam.Onvif.Password, 3500*time.Millisecond)
	}

	// 1) try ONVIF discovered URI
	if uri != "" {
		if data, ctype, err := fetchWithAuth(uri); err == nil && len(data) > 0 && isJPEG(data, ctype) {
			if err := os.WriteFile(dest, data, 0o640); err == nil {
				return true
			}
		} else {
			logIt(LDEBUG, "snapshot native uri failed", map[string]string{"camera": camID, "uri": uri, "error": fmt.Sprintf("%v", err)})
		}
	}

	// 2) try common fallback URLs derived from camera IP
	var ip = cameraHost(cam)
	if ip != "" {
		for _, cand := range snapshotCandidates(ip) {
			if data, ctype, err := fetchWithAuth(cand); err == nil && len(data) > 2 && isJPEG(data, ctype) {
				if err := os.WriteFile(dest, data, 0o640); err == nil {
					logIt(LDEBUG, "snapshot fallback url succeeded", map[string]string{"camera": camID, "url": cand})
					// cache it
					dev.setSnapshotURI(cand)
					return true
				}
			}
		}
	}

	// 3) last try: RTSP fallback will be tried by caller, so just fail and let caller fallback
	return false
}

// decodeH264AUToJpeg decodes a single complete H264 access unit (SPS/PPS+IDR)
// via Eyevinn hi264 to a JPEG file. Shared by the live-RTSP and buffer paths.
func decodeH264AUToJpeg(au [][]byte, camID, dest string) bool {
	if len(au) == 0 {
		return false
	}
	annexb, err := h264.AnnexB(au).Marshal()
	if err != nil {
		return false
	}
	hidDec := decoder.New()
	frame, err := hidDec.DecodeAnnexB(annexb)
	if err != nil || frame == nil {
		logIt(LDEBUG, "snapshot hi264 decode failed", map[string]string{"camera": camID, "error": fmt.Sprintf("%v", err)})
		return false
	}
	// The hi264 decoder outputs 4:2:0 only. Guard the plane geometry before
	// tagging it 420: a non-420 stream must fail loudly, not encode with
	// swapped chroma (false colors).
	var w2 = (frame.Width + 1) / 2
	var h2 = (frame.Height + 1) / 2
	if frame.StrideC < w2 || len(frame.Cb) < frame.StrideC*(h2-1)+w2 || len(frame.Cr) < frame.StrideC*(h2-1)+w2 || len(frame.Y) < frame.StrideY*(frame.Height-1)+frame.Width {
		logIt(LDEBUG, "snapshot chroma geometry not 4:2:0, skipping", map[string]string{"camera": camID, "w": strconv.Itoa(frame.Width), "h": strconv.Itoa(frame.Height)})
		return false
	}
	rect := image.Rect(0, 0, frame.Width, frame.Height)
	ycbcr := &image.YCbCr{
		Y:              frame.Y,
		Cb:             frame.Cb,
		Cr:             frame.Cr,
		YStride:        frame.StrideY,
		CStride:        frame.StrideC,
		Rect:           rect,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
	}
	f, err := os.Create(dest)
	if err != nil {
		return false
	}
	err = jpeg.Encode(f, ycbcr, &jpeg.Options{Quality: 85})
	f.Close()
	if err != nil {
		_ = os.Remove(dest)
		logIt(LDEBUG, "snapshot jpeg encode failed", map[string]string{"camera": camID, "error": err.Error()})
		return false
	}
	if s, e := os.Stat(dest); e != nil || s.Size() == 0 {
		_ = os.Remove(dest)
		return false
	}
	logIt(LDEBUG, "snapshot hi264 ok", map[string]string{"camera": camID, "path": dest, "w": strconv.Itoa(frame.Width), "h": strconv.Itoa(frame.Height)})
	return true
}

// grabSnapshotFromBuffer extracts the latest decodable IDR from the rolling
// MPEG-TS ring buffer (local tmpfs, no camera RTT) and decodes it to JPEG.
// Zero network delay and temporally aligned to the pre-event window.
// Returns false when buffer mode is off, buffer is empty, or decode fails
// so the caller falls through to the live chain.
func grabSnapshotFromBuffer(app *App, camID string, dest string) bool {
	if app.cfg.Record.Mode != "buffer" {
		return false
	}
	b := cameraBuffer(camID)
	// Pin the ring so the watcher does not prune segments while we read.
	b.mu.Lock()
	b.pins++
	segs := make([]BuffSeg, len(b.segs))
	copy(segs, b.segs)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.pins--
		b.mu.Unlock()
	}()
	if len(segs) == 0 {
		return false
	}
	// Newest first, try at most the last 3 complete segments.
	start := 0
	if len(segs) > 3 {
		start = len(segs) - 3
	}
	for i := len(segs) - 1; i >= start; i-- {
		if decodeLatestIDRFromTs(segs[i].path, camID, dest) {
			logIt(LDEBUG, "snapshot captured", map[string]string{"camera": camID, "snapshot": "buffer", "path": dest, "source": "buffer", "segment": segs[i].path})
			return true
		}
	}
	return false
}

// decodeLatestIDRFromTs demuxes one MPEG-TS segment, keeps the last random-access
// AU (with its SPS/PPS) and decodes it to JPEG. Tolerates partially-written
// (still-recording) segments by decoding whatever IDRs are complete so far.
// TODO: avoid the per-AU deep copy (keep NALU offsets into a shared arena and
// decode once); the copy is bounded (one AU) so it stays as-is for now.
func decodeLatestIDRFromTs(path, camID, dest string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	r := &mpegts.Reader{R: f}
	if err := r.Initialize(); err != nil {
		return false
	}
	var h264Track *mpegts.Track
	for _, t := range r.Tracks() {
		if _, ok := t.Codec.(*tscodecs.H264); ok {
			h264Track = t
			break
		}
	}
	if h264Track == nil {
		return false
	}
	var sps, pps []byte
	var lastAU [][]byte
	var lastSPS, lastPPS []byte
	r.OnDataH264(h264Track, func(pts int64, dts int64, au [][]byte) error {
		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}
			switch h264.NALUType(nalu[0] & 0x1F) {
			case h264.NALUTypeSPS:
				sps = append([]byte(nil), nalu...)
			case h264.NALUTypePPS:
				pps = append([]byte(nil), nalu...)
			}
		}
		if !h264.IsRandomAccess(au) {
			return nil
		}
		cp := make([][]byte, 0, len(au))
		for _, nalu := range au {
			cp = append(cp, append([]byte(nil), nalu...))
		}
		lastAU = cp
		if sps != nil {
			lastSPS = append([]byte(nil), sps...)
		}
		if pps != nil {
			lastPPS = append([]byte(nil), pps...)
		}
		return nil
	})
	for {
		if err := r.Read(); err != nil {
			break // EOF or short read on in-progress segment: decode what we have
		}
	}
	if len(lastAU) == 0 {
		return false
	}
	hasSPS, hasPPS := false, false
	for _, nalu := range lastAU {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1F) {
		case h264.NALUTypeSPS:
			hasSPS = true
		case h264.NALUTypePPS:
			hasPPS = true
		}
	}
	var complete [][]byte
	if !hasSPS && lastSPS != nil {
		complete = append(complete, lastSPS)
	}
	if !hasPPS && lastPPS != nil {
		complete = append(complete, lastPPS)
	}
	complete = append(complete, lastAU...)
	return decodeH264AUToJpeg(complete, camID, dest)
}

// grabSnapshotViaGortsplibHi264 is the pure-Go fallback when ffmpeg is not available.
// It decodes the first IDR via Eyevinn hi264 (IDR-only, no B-frames) – it works for
// hi264-generated streams but often fails on real Tapo Main/High profile (B-frames/CABAC),
// hence ffmpeg is tried first when available.
func grabSnapshotViaGortsplibHi264(rtspURL, camID, dest string) bool {
	u, err := base.ParseURL(rtspURL)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	c := newRTSPClient(u, "")
	// Single ctx timeout: closing the client unblocks Wait/reads when the
	// deadline fires (no second competing timer).
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	if serr := rtspOpTimeout(7*time.Second, func() error { return c.Start() }); serr != nil {
		logIt(LDEBUG, "snapshot rtsp start failed", map[string]string{"camera": camID, "error": serr.Error()})
		return false
	}
	defer c.Close()
	var desc *description.Session
	if derr := rtspOpTimeout(7*time.Second, func() error {
		d, _, e := c.Describe(u)
		if e == nil {
			desc = d
		}
		return e
	}); derr != nil {
		logIt(LDEBUG, "snapshot rtsp describe failed", map[string]string{"camera": camID, "error": derr.Error()})
		return false
	}
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		logIt(LDEBUG, "snapshot no H264", map[string]string{"camera": camID})
		return false
	}
	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		logIt(LDEBUG, "snapshot decoder failed", map[string]string{"camera": camID, "error": err.Error()})
		return false
	}
	hidDec := decoder.New()
	if serr := rtspOpTimeout(7*time.Second, func() error {
		_, e := c.Setup(desc.BaseURL, medi, 0, 0)
		return e
	}); serr != nil {
		logIt(LDEBUG, "snapshot setup failed", map[string]string{"camera": camID, "error": serr.Error()})
		return false
	}
	done := make(chan struct{})
	var once sync.Once
	var success bool
	var successMu sync.Mutex
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if !errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) && !errors.Is(err, rtph264.ErrMorePacketsNeeded) {
			}
			return
		}
		if !h264.IsRandomAccess(au) {
			return
		}
		hasSPS, hasPPS := false, false
		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}
			t := h264.NALUType(nalu[0] & 0x1F)
			if t == h264.NALUTypeSPS {
				hasSPS = true
			}
			if t == h264.NALUTypePPS {
				hasPPS = true
			}
		}
		var complete [][]byte
		if !hasSPS && forma.SPS != nil {
			complete = append(complete, forma.SPS)
		}
		if !hasPPS && forma.PPS != nil {
			complete = append(complete, forma.PPS)
		}
		complete = append(complete, au...)
		annexb, err := h264.AnnexB(complete).Marshal()
		if err != nil {
			return
		}
		frame, err := hidDec.DecodeAnnexB(annexb)
		if err != nil || frame == nil {
			logIt(LDEBUG, "snapshot hi264 decode failed", map[string]string{"camera": camID, "error": fmt.Sprintf("%v", err)})
			return
		}
		// Guard the plane geometry like decodeH264AUToJpeg: a non-420
		// stream must fail loudly, not encode with swapped chroma.
		var w2 = (frame.Width + 1) / 2
		var h2 = (frame.Height + 1) / 2
		if frame.StrideC < w2 || len(frame.Cb) < frame.StrideC*(h2-1)+w2 || len(frame.Cr) < frame.StrideC*(h2-1)+w2 || len(frame.Y) < frame.StrideY*(frame.Height-1)+frame.Width {
			logIt(LDEBUG, "snapshot chroma geometry not 4:2:0, skipping", map[string]string{"camera": camID, "w": strconv.Itoa(frame.Width), "h": strconv.Itoa(frame.Height)})
			return
		}
		rect := image.Rect(0, 0, frame.Width, frame.Height)
		ycbcr := &image.YCbCr{
			Y:              frame.Y,
			Cb:             frame.Cb,
			Cr:             frame.Cr,
			YStride:        frame.StrideY,
			CStride:        frame.StrideC,
			Rect:           rect,
			SubsampleRatio: image.YCbCrSubsampleRatio420,
		}
		f, err := os.Create(dest)
		if err != nil {
			return
		}
		err = jpeg.Encode(f, ycbcr, &jpeg.Options{Quality: 85})
		f.Close()
		if err != nil {
			_ = os.Remove(dest)
			logIt(LDEBUG, "snapshot jpeg encode failed", map[string]string{"camera": camID, "error": err.Error()})
			return
		}
		if s, e := os.Stat(dest); e != nil || s.Size() == 0 {
			_ = os.Remove(dest)
			return
		}
		successMu.Lock()
		success = true
		successMu.Unlock()
		logIt(LDEBUG, "snapshot rtsp hi264 ok", map[string]string{"camera": camID, "path": dest, "w": strconv.Itoa(frame.Width), "h": strconv.Itoa(frame.Height)})
		once.Do(func() { close(done) })
	})
	if perr := rtspOpTimeout(7*time.Second, func() error {
		_, e := c.Play(nil)
		return e
	}); perr != nil {
		logIt(LDEBUG, "snapshot play failed", map[string]string{"camera": camID, "error": perr.Error()})
		return false
	}
	select {
	case <-done:
		successMu.Lock()
		ok := success
		successMu.Unlock()
		return ok
	case <-ctx.Done():
		logIt(LDEBUG, "snapshot rtsp timeout", map[string]string{"camera": camID})
		return false
	}
}

// grabSnapshotFast takes a JPEG snapshot via direct RTSP (gortsplib) + pure-Go
// H.264 IDR decode (Eyevinn hi264). No ffmpeg, no go2rtc frame API. Returns true on success.
// It connects to the camera's RTSP URL, waits for the next IDR, decodes it to YUV and
// encodes to JPEG. Timeout ~6s.
func grabSnapshotFast(app *App, camID string, dest string) bool {
	cam, _ := cameraByID(app, camID)
	if cam == nil {
		return false
	}
	// When go2rtc runs, use the relay rtsp://127.0.0.1:8554/camN (a single connection to the camera), else direct.
	var rtspURL = recordingsSource(app, cam, camID)
	if rtspURL == "" {
		rtspURL = cam.RtspURL
	}
	if rtspURL == "" {
		return false
	}
	// pure-Go RTSP snapshot (hi264 IDR-only) via go2rtc relay — ONVIF HTTP stays primary
	if ok := grabSnapshotViaGortsplibHi264(rtspURL, camID, dest); ok {
		return true
	}
	logIt(LDEBUG, "snapshot rtsp hi264 failed (IDR-only, B-frames unsupported)", map[string]string{"camera": camID, "src": redactRTSPUserInfo(rtspURL)})
	return false
}

// grabSnapshotHttp tries common HTTP snapshot URLs for Tapo cameras (fallback when go2rtc fails).
func grabSnapshotHttp(app *App, camID string, dest string) bool {
	cam, _ := cameraByID(app, camID)
	if cam == nil {
		return false
	}
	var ip = cameraHost(cam)
	if ip == "" {
		return false
	}
	for _, cand := range snapshotCandidates(ip) {
		if data, ctype, err := fetchSnapshotWithAuth(cand, cam.Onvif.Username, cam.Onvif.Password, 1500*time.Millisecond); err == nil && len(data) > 2 && isJPEG(data, ctype) {
			if err := os.WriteFile(dest, data, 0o640); err == nil {
				logIt(LDEBUG, "snapshot http fallback ok", map[string]string{"camera": camID, "url": cand})
				return true
			}
		}
	}
	return false
}

// snapCamStat tracks per-camera snapshot provenance for /api/system/status:
// where snapshots come from and the last failure cause. A camera silently
// serving "NO SIGNAL" placeholders is otherwise invisible to the user.
type snapCamStat struct {
	counts map[string]int64
	last   string
	lastOK bool
	lastAt int64
	err    string
}

var snapStatMtx sync.Mutex
var snapStat = map[string]*snapCamStat{}

// snapRecord notes one snapshot attempt outcome for a camera.
func snapRecord(camID, source string, ok bool, errMsg string) {
	snapStatMtx.Lock()
	defer snapStatMtx.Unlock()
	var st = snapStat[camID]
	if st == nil {
		st = &snapCamStat{counts: map[string]int64{}}
		snapStat[camID] = st
	}
	if source == "" {
		source = "failed"
	}
	st.counts[source]++
	st.last = source
	st.lastOK = ok
	st.lastAt = time.Now().Unix()
	if ok {
		st.err = ""
	} else if errMsg != "" {
		st.err = errMsg
	}
}

// grabSnapshotChain tries every snapshot source in order (buffer IDR, native
// ONVIF, pure-Go RTSP, http fallback) and records the outcome for status.
// Returns the winning source name ("" when all failed).
func grabSnapshotChain(app *App, camID, dest string) (string, bool) {
	if grabSnapshotFromBuffer(app, camID, dest) {
		snapRecord(camID, "buffer", true, "")
		return "buffer", true
	}
	if grabSnapshotNative(app, camID, dest) {
		snapRecord(camID, "native", true, "")
		return "native", true
	}
	if grabSnapshotFast(app, camID, dest) {
		snapRecord(camID, "rtsp/gortsplib", true, "")
		return "rtsp/gortsplib", true
	}
	if grabSnapshotHttp(app, camID, dest) {
		snapRecord(camID, "http-fallback", true, "")
		return "http-fallback", true
	}
	snapRecord(camID, "failed", false, "all snapshot sources failed")
	return "", false
}

// snapStatusJSON renders a camera's snapshot provenance for the status payload.
func snapStatusJSON(camID string) string {
	snapStatMtx.Lock()
	st := snapStat[camID]
	// Copy the fields while still holding the lock: st is mutated under
	// snapStatMtx by snapRecord, so reading st.last/st.err after Unlock raced
	// the writers (torn string headers).
	var last, lastErr string
	if st != nil {
		last, lastErr = st.last, st.err
	}
	snapStatMtx.Unlock()
	if st == nil {
		return kv("snapshot_source", "none") + "," + kv("snapshot_last_error", "")
	}
	return kv("snapshot_source", last) + "," + kv("snapshot_last_error", lastErr)
}

// collectDetection writes snapshot+clip for a detection and records it.
// src is the RTSP URL, or "" to write test placeholder media.
func collectDetection(app *App, camID string, camName string, src string, dtype string) (*Detection, error) {
	return collectDetectionWithID(app, camID, camName, src, dtype, newID(), "")
}

// collectDetectionWithID is collectDetection with a caller-provided id. The
// grouping layer reserves the id (and its active-rec slot) BEFORE the slow
// snapshot capture so concurrent events extend this group instead of forking
// a second one. bbox is the triggering frame's RTSP SEI boxes (JSON, "" for
// ONVIF events).
func collectDetectionWithID(app *App, camID string, camName string, src string, dtype string, id string, bbox string) (*Detection, error) {
	var base = app.cfg.Server.DataDir + "/detections/" + time.Now().Local().Format("2006-01-02") + "/" + camID
	if err := ensureDir(base); err != nil {
		return nil, err
	}
	var now = time.Now().Unix()
	var snap = base + "/" + friendlyTS(now) + "-" + id + "-snap.jpg"
	var clip = base + "/" + friendlyTS(now) + "-" + id + "-" + dtype + ".mp4"
	// Separate switches so recording (clips) and snapshots can be enabled on their own.
	var snapOn = snapshotsEnabled()
	var recordOn = recordingEnabled()
	var count = app.cfg.Record.Snapshots.Count
	// count < 0 means continuous: keep shooting at interval_s while the
	// camera reports the detection as active (bounded, see burst caps).
	// count == 0 is treated as 1 (use snapshots.enabled=false for none).
	var continuous = count < 0
	if count == 0 {
		count = 1
	}
	var interval = app.cfg.Record.Snapshots.IntervalS
	if interval < 1 {
		interval = 1
	}
	var extraCount = count - 1
	if continuous {
		extraCount = -1 // sentinel understood by captureSnapshotBurst
	}

	if src == "" {
		// Test placeholder media (no-op rows for manual testing). The text
		// probe uses a .txt sidecar (never .mp4) so detectionClipURL never
		// advertises plain text as video/mp4; clipPath stays empty.
		_ = writePlaceholderJpg(snap)
		var probe = []byte("taponvr test clip placeholder")
		var txtClip = strings.TrimSuffix(clip, ".mp4") + ".txt"
		_ = os.WriteFile(txtClip, probe, 0o640)
	} else {
		if snapOn {
			// Capture the primary snapshot: rolling-buffer IDR first (zero delay,
			// pre-event aligned), then native ONVIF, pure-Go RTSP, http fallback,
			// then placeholder. No ffmpeg/go2rtc.
			var source, ok = grabSnapshotChain(app, camID, snap)
			if !ok {
				source = "failed"
			}
			if ok {
				if s, e := os.Stat(snap); e != nil || s.Size() == 0 {
					ok = false
				}
			}
			if !ok {
				_ = os.Remove(snap)
				_ = writePlaceholderJpg(snap)
				if s, e := os.Stat(snap); e == nil && s.Size() > 0 {
					source = "placeholder"
					ok = true
					snapRecord(camID, "placeholder", true, "live sources failed; placeholder served")
					logIt(LWARN, "snapshot fallback to placeholder", map[string]string{"camera": camID, "snapshot": "primary", "path": snap})
				}
			}
			if ok {
				logIt(LDEBUG, "snapshot captured", map[string]string{
					"camera": camID, "snapshot": "primary", "path": snap, "source": source,
				})
			} else {
				logIt(LWARN, "snapshot failed", map[string]string{"camera": camID, "snapshot": "primary"})
				_ = os.Remove(snap)
			}
			// The remaining snapshots are captured in the background, spaced at
			// least `interval` seconds apart. Kept async so it does not delay
			// the primary snapshot / detection.
			if extraCount > 0 || continuous {
				go func() {
					var extra = captureSnapshotBurst(app, base, id, camID, extraCount, interval, now)
					if len(extra) == 0 {
						return
					}
					_ = dbAppendSnapPaths(app.db, id, extra)
					// Refresh the cached total size with the extra snapshots.
					if det, derr := getDetection(app.db, id); derr == nil && det != nil {
						_ = dbSetSize(app.db, id, detectionSize(det))
					}
					if s3UploadEnabled() {
						for _, p := range extra {
							if uerr := s3PutFile(app, s3KeyForPath(p), p, "image/jpeg"); uerr != nil {
								logIt(LWARN, "s3 snapshot upload failed", map[string]string{"id": id, "path": p, "error": uerr.Error()})
							}
						}
					}
					// Vision on burst extras (only when vision.snapshots=all).
					queueVisionForExtras(app, id, extra)
				}()
			}
		}
		if recordOn && app.cfg.Record.Mode != "buffer" {
			// Legacy event mode: record clip via Go RTSP (no ffmpeg).
			if err := recordClipGo(src, app.cfg.Record.PostDetection, clip); err != nil {
				logIt(LWARN, "event clip failed", map[string]string{"camera": camID, "error": err.Error()})
				_ = os.Remove(clip)
			}
		}
	}
	// Merge extensions that landed while snapshots were being captured: the
	// reserved slot may already end later than this stale computation.
	var endedAt = now + int64(app.cfg.Record.PostDetection)
	if cur := getActiveRec(camID); cur != nil && cur.id == id && cur.endAt > endedAt {
		endedAt = cur.endAt
	}
	var d Detection
	d.id = id
	d.cameraID = camID
	d.camName = camName
	d.detType = normalizeDetType(dtype)
	d.detTypes = d.detType
	d.confidence = 0.9
	d.startedAt = now - int64(app.cfg.Record.PreDetection)
	d.endedAt = endedAt
	d.bboxJSON = bbox
	// A clip exists when recording is on and we are NOT in buffer mode (buffer
	// mode clip is assembled asynchronously by assembleClip and stored later).
	var clipPath string
	if recordOn && app.cfg.Record.Mode != "buffer" {
		if s, e := os.Stat(clip); e == nil && s.Size() > 0 {
			clipPath = clip
		}
	}
	// Only advertise a snapshot when the file really exists (e.g. snapshots
	// disabled means no file was written) so clients don't get a 404 link.
	var snapPath string
	if s, e := os.Stat(snap); e == nil && s.Size() > 0 {
		snapPath = snap
	}
	d.snapshotPath = snapPath
	d.clipPath = clipPath
	// Stamp S3 expectation at insert so the pending counter and retry job only
	// track rows that were meant to be uploaded (not ones made while S3 was off).
	if s3UploadEnabled() && src != "" {
		d.s3Expected = 1
	}
	// Cache the media size (snapshot now; clip refreshed by assembleClip in
	// buffer mode, extras refreshed when the burst completes).
	d.sizeBytes = detectionSize(&d)
	// Insert while holding the grouping lock: a concurrent extension either
	// lands in the in-memory slot before this read (and is persisted here) or
	// waits until the row is established and merges via the DB path. This
	// closes the reserved-but-uninserted race for merged tags.
	groupMtx.Lock()
	if cur := getActiveRec(camID); cur != nil && cur.id == id {
		if cur.endAt > d.endedAt {
			d.endedAt = cur.endAt
		}
		if cur.detType != "" {
			d.detType = cur.detType
		}
		if cur.detTypes != "" {
			d.detTypes = cur.detTypes
		}
	}
	if d.detTypes == "" {
		d.detTypes = d.detType
	}
	var derr = dbInsertDetection(app.db, &d)
	if derr == nil {
		// Row exists: mark the reserved slot established (keeps any end time
		// extensions that landed during the slow capture above).
		confirmActiveRec(camID, id, d.endedAt)
	}
	groupMtx.Unlock()
	if derr != nil {
		return nil, derr
	}
	// S3 upload + Pushover run in the background: both can block for seconds
	// (S3 up to minutes on a hung endpoint, Pushover 15s timeout) and must not
	// stall the ONVIF webhook response or the grouping path. Values are copied
	// for the goroutine; the DB row above is already committed.
	go func(app *App, snap, clipPath, camName, dtype, id, mode string, snapOn, recordOn bool, hasSrc bool) {
		if s3UploadEnabled() && hasSrc {
			// Upload each artifact independently: a failed/absent snapshot must
			// not suppress the clip upload (event mode has no later retry pass
			// unless the user triggers one) and snapshots can be disabled while
			// recording still produces a clip.
			var allOK = true
			if snapOn {
				if snap == "" {
					allOK = false
				} else if e2 := s3PutFile(app, s3KeyForPath(snap), snap, "image/jpeg"); e2 != nil {
					logIt(LWARN, "s3 snapshot upload failed", map[string]string{"id": id, "error": e2.Error()})
					allOK = false
				}
			}
			var clipPending = recordOn && mode == "buffer"
			if recordOn && mode != "buffer" {
				if clipPath == "" {
					allOK = false // expected clip absent
				} else if e2 := s3PutFile(app, s3KeyForPath(clipPath), clipPath, clipContentType(clipPath)); e2 != nil {
					logIt(LWARN, "s3 clip upload failed", map[string]string{"id": id, "error": e2.Error()})
					allOK = false
				}
			}
			// In buffer mode assembleClip uploads the clip and marks the row;
			// elsewhere mark it once every expected artifact is up.
			if allOK && !clipPending {
				_ = dbSetUploaded(app.db, id)
			}
		}
		if snapOn {
			pushNotify(app, dtype, "Detection: "+camName, dtype+" detected on "+camName, snap)
		} else {
			pushNotify(app, dtype, "Detection: "+camName, dtype+" detected on "+camName, "")
		}
	}(app, snap, clipPath, camName, d.detType, id, app.cfg.Record.Mode, snapOn, recordOn, src != "")
	// Notify connected WebUI clients immediately ("motion detected").
	broadcastDetection(camID, camName, d.detType, bbox)
	mqttPublishDetection(&d)
	// Vision: async YOLO on the primary snapshot (never blocks clip/S3/push).
	// Degraded-silent when the API is down (vision_error only).
	if snapPath != "" {
		queueVisionForDetection(app, id, snapPath)
	}
	if src != "" && recordOn && app.cfg.Record.Mode == "buffer" {
		// Slot already reserved + confirmed above; re-confirm to fold in any
		// extension that landed during the S3/notify calls, then assemble.
		confirmActiveRec(camID, id, d.endedAt)
		var b = cameraBuffer(camID)
		// Assemble the (pre-event + after) clip in parallel with snapshots.
		clipAssembleAsync(app, b, id, clip, now)
	}
	return &d, nil
}

// Bounds for continuous snapshot mode (count < 0): stop shooting when the
// camera reports inactive, but never exceed these even if it never does.
const maxContinuousSnaps = 60
const maxContinuousSecs = 180

// captureSnapshotBurst captures `count` trailing snapshots (numbered 1..count)
// for a detection. Snapshot i is scheduled at eventT + i*interval, and at
// least `interval` is enforced between two shots: when a grab (or the primary
// capture) overruns the schedule, the backlog shifts forward instead of
// firing back-to-back. With count < 0 (continuous mode) it keeps shooting
// until the camera reports the detection as inactive (bounded by the caps
// above). Each grab uses buffer IDR first, then native ONVIF, RTSP
// gortsplib+hi264, http fallback. No ffmpeg.
func captureSnapshotBurst(app *App, base string, detID string, camID string, count int, interval int, eventT int64) []string {
	var paths []string
	// Clamp the schedule inputs: count<=16, interval<=30s and total burst
	// duration<=10min, so a misconfigured interval_s/count can neither pin
	// this goroutine in sleep nor loop far past any sane burst.
	if interval < 1 {
		interval = 1
	}
	if interval > 30 {
		logIt(LWARN, "snapshot burst interval clamped", map[string]string{"camera": camID, "id": detID, "interval": strconv.Itoa(interval)})
		interval = 30
	}
	var continuous = count < 0
	var max = count
	if continuous {
		max = maxContinuousSnaps
	} else if max > 16 {
		logIt(LWARN, "snapshot burst count clamped", map[string]string{"camera": camID, "id": detID, "count": strconv.Itoa(max)})
		max = 16
	}
	// Total duration cap: 10 minutes regardless of count*interval.
	if int64(max)*int64(interval) > 600 {
		max = 600 / interval
		if max < 1 {
			max = 1
		}
		logIt(LWARN, "snapshot burst duration clamped", map[string]string{"camera": camID, "id": detID, "max": strconv.Itoa(max)})
	}
	if max < 1 {
		return paths
	}
	var t0 = time.Unix(eventT, 0)
	// Burst starts right after the primary snapshot: enforce spacing from here
	// so a slow primary doesn't collapse the first extras onto each other.
	var lastFire = time.Now()
	for i := 1; i <= max; i++ {
		if continuous && !camIsActive(camID) {
			break // already inactive: don't even wait out the first interval
		}
		var scheduled = t0.Add(time.Duration(i*interval) * time.Second)
		var target = scheduled
		if minNext := lastFire.Add(time.Duration(interval) * time.Second); target.Before(minNext) {
			target = minNext
			if d := target.Sub(scheduled); d > 500*time.Millisecond {
				logIt(LDEBUG, "snapshot rescheduled", map[string]string{
					"camera": camID, "snapshot": strconv.Itoa(i),
					"shift_ms": strconv.FormatInt(d.Milliseconds(), 10),
				})
			}
		}
		// Poll at 50ms until the precise deadline (avoids second-truncation drift).
		// Cancellable: abort if the detection was deleted meanwhile.
		var polls int
		var deleted bool
		for time.Now().Before(target) {
			time.Sleep(50 * time.Millisecond)
			if polls++; polls%10 == 0 {
				if cur, derr := getDetection(app.db, detID); derr == nil && cur == nil {
					deleted = true
					break
				}
			}
		}
		if deleted {
			return paths
		}
		if continuous {
			if time.Since(t0).Seconds() > maxContinuousSecs {
				logIt(LWARN, "snapshot burst capped (duration)", map[string]string{"camera": camID, "id": detID})
				break
			}
			if !camIsActive(camID) {
				break // motion over
			}
		}
		// Stop if the detection was deleted meanwhile (e.g. "delete all"):
		// don't orphan snapshot files with no DB row behind them.
		if cur, derr := getDetection(app.db, detID); derr != nil || cur == nil {
			return paths
		}
		var p = base + "/" + friendlyTS(target.Unix()) + "-" + detID + "-snap" + strconv.Itoa(i) + ".jpg"
		var source, ok = grabSnapshotChain(app, camID, p)
		if !ok {
			source = "failed"
		}
		lastFire = time.Now() // spacing applies from shot completion, success or not
		if s, e := os.Stat(p); e == nil && s.Mode().IsRegular() && s.Size() > 0 && ok {
			paths = append(paths, p)
			logIt(LDEBUG, "snapshot captured", map[string]string{
				"camera": camID, "snapshot": strconv.Itoa(i), "path": p, "source": source,
			})
		} else {
			_ = os.Remove(p)
		}
	}
	if continuous && len(paths) >= maxContinuousSnaps {
		logIt(LWARN, "snapshot burst capped (count)", map[string]string{"camera": camID, "id": detID})
	}
	return paths
}

// dbAppendSnapPaths appends extra snapshot paths to a detection row, preserving
// any already recorded.
func dbAppendSnapPaths(store *DB, id string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	// Single-statement append: a read-modify-write in Go loses paths when two
	// bursts for the same detection land concurrently (orphaned JPGs that are
	// never listed, never uploaded and never deleted).
	var add = strings.Join(paths, ",")
	_, e := store.db.Exec(
		"UPDATE detections SET extra_snapshots = CASE WHEN extra_snapshots IS NULL OR extra_snapshots='' THEN ? ELSE extra_snapshots || ',' || ? END WHERE id=?",
		add, add, id)
	return e
}

// ---------------- prune: retention ----------------------------

// pruneDeleteRow deletes one detection + its vision rows in a transaction,
// logging Exec/Commit failures instead of silently ignoring them.
func pruneDeleteRow(app *App, id string) {
	if tx, terr := app.db.db.Begin(); terr == nil {
		if _, e := tx.Exec("DELETE FROM detections WHERE id=?", id); e != nil {
			logIt(LERR, "prune delete failed", map[string]string{"id": id, "error": e.Error()})
		}
		if _, e := tx.Exec("DELETE FROM vision_results WHERE detection_id=?", id); e != nil {
			logIt(LERR, "prune delete vision failed", map[string]string{"id": id, "error": e.Error()})
		}
		if e := tx.Commit(); e != nil {
			logIt(LERR, "prune commit failed", map[string]string{"id": id, "error": e.Error()})
		}
	} else {
		logIt(LERR, "prune begin failed", map[string]string{"id": id, "error": terr.Error()})
		if _, e := app.db.db.Exec("DELETE FROM detections WHERE id=?", id); e != nil {
			logIt(LERR, "prune delete failed", map[string]string{"id": id, "error": e.Error()})
		}
		if e := dbDeleteVisionResults(app.db, id); e != nil {
			logIt(LERR, "prune delete vision failed", map[string]string{"id": id, "error": e.Error()})
		}
	}
}

func pruneOnce(app *App) {
	// retention_days <= 0 means "keep forever": a zero cutoff would match every
	// row and wipe the archive (plus its S3 objects) at boot and daily.
	// The disk guard still runs below so "keep everything" cannot fill the
	// disk until writes fail.
	if app.cfg.Server.RetentionDays <= 0 {
		logIt(LINFO, "prune skipped (retention_days <= 0 keeps everything)", nil)
		// Per-type windows ("keep this type for N days") are independent of the
		// global keep-forever switch and must still be honoured.
		pruneByType(app)
		diskGuardPrune(app)
		return
	}
	var cutoff = time.Now().Add(-time.Duration(app.cfg.Server.RetentionDays) * 24 * time.Hour).Unix()
	// Query old rows directly (not the newest-first listing) so the prune pass is
	// not capped by the 500-row limit and actually reaches aging detections.
	var old, err = listOldDetections(app.db, cutoff)
	if err != nil {
		return
	}
	// Bounded per pass: S3 deletes are sequential with 60s timeouts each, so
	// an unbounded first pass would stall for hours. The daily loop (and the
	// next boot pass) continues where this one stopped.
	const maxPrunePerPass = 500
	if len(old) > maxPrunePerPass {
		logIt(LWARN, "prune pass capped, remainder continues next pass", map[string]string{"total": strconv.Itoa(len(old)), "capped": strconv.Itoa(maxPrunePerPass)})
		old = old[:maxPrunePerPass]
	}
	for i := range old {
		var d = old[i]
		// Delete the DB rows first (detection + vision in one transaction):
		// a crash then leaves orphan files (reclaimed next pass / visible
		// on disk) instead of DB rows pointing at missing media.
		pruneDeleteRow(app, d.id)
		deleteDetectionFiles(app, &d)
	}
	// prune old raw ONVIF events as well
	var res, _ = app.db.db.Exec("DELETE FROM onvif_events WHERE created_at < ?", cutoff)
	var evRemoved int64 = 0
	if res != nil {
		evRemoved, _ = res.RowsAffected()
	}
	pruneEmptyDirs(app.cfg.Server.DataDir + "/detections")
	logIt(LINFO, "prune pass complete", map[string]string{"retention_days": strconv.Itoa(app.cfg.Server.RetentionDays), "removed": strconv.Itoa(len(old)), "events_removed": strconv.FormatInt(evRemoved, 10)})
	pruneByType(app)
	diskGuardPrune(app)
}

// pruneByType enforces record.retention_by_type overrides: for each type with
// a custom window, unlocked rows ended before now-window are deleted
// (DB-first, bounded per type per pass). Types absent from the map, or longer
// than the global retention, are already covered by the global pass.
func pruneByType(app *App) {
	if len(app.cfg.Record.RetentionByType) == 0 {
		return
	}
	var types []string
	for t := range app.cfg.Record.RetentionByType {
		types = append(types, t)
	}
	sort.Strings(types)
	const maxPerTypePerPass = 200
	for _, t := range types {
		var days = app.cfg.Record.RetentionByType[t]
		if days <= 0 {
			continue
		}
		var cutoff = time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
		cands, err := listOldDetectionsByType(app.db, cutoff, t)
		if err != nil || len(cands) == 0 {
			continue
		}
		var n int
		for i := range cands {
			if i >= maxPerTypePerPass {
				logIt(LWARN, "per-type prune capped, remainder continues next pass", map[string]string{"type": t, "capped": strconv.Itoa(maxPerTypePerPass)})
				break
			}
			var d = cands[i]
			pruneDeleteRow(app, d.id)
			deleteDetectionFiles(app, &d)
			n++
		}
		if n > 0 {
			logIt(LINFO, "per-type prune pass complete", map[string]string{"type": t, "days": strconv.Itoa(days), "removed": strconv.Itoa(n)})
		}
	}
}

// diskGuardPrune enforces storage.max_disk_pct / min_free_gb: when data_dir's
// filesystem breaches either threshold, oldest unlocked detections are deleted
// (DB-first, bounded per pass) until back under, or nothing prunable remains.
// Locked recordings are never touched.
func diskGuardPrune(app *App) {
	var maxPct = app.cfg.Storage.MaxDiskPct
	var minGB = app.cfg.Storage.MinFreeGB
	if maxPct <= 0 && minGB <= 0 {
		return
	}
	over := func() (bool, string) {
		free, total := fsFreeTotal(app.cfg.Server.DataDir)
		if total <= 0 {
			return false, ""
		}
		var usedPct = int((total - free) * 100 / total)
		if maxPct > 0 && usedPct >= maxPct {
			return true, "max_disk_pct"
		}
		if minGB > 0 && float64(free) < minGB*1e9 {
			return true, "min_free_gb"
		}
		return false, ""
	}
	ok, _ := over()
	if !ok {
		return
	}
	// All ended, oldest first (currently recording rows have future ended_at).
	cands, err := listOldDetections(app.db, time.Now().Unix()+1)
	if err != nil || len(cands) == 0 {
		logIt(LWARN, "disk guard: threshold breached but nothing prunable (all locked/recording?)", nil)
		return
	}
	const maxGuardPerPass = 500
	var removed int
	for i := range cands {
		if i >= maxGuardPerPass {
			break
		}
		if i%50 == 0 {
			if ok, _ := over(); !ok {
				break
			}
		}
		var d = cands[i]
		pruneDeleteRow(app, d.id)
		deleteDetectionFiles(app, &d)
		removed++
	}
	if ok, _ := over(); ok {
		logIt(LWARN, "disk guard: still over threshold after pass (locked rows or cap reached)", map[string]string{"removed": strconv.Itoa(removed)})
	} else {
		logIt(LWARN, "disk guard: pruned back under threshold", map[string]string{"removed": strconv.Itoa(removed)})
	}
}

// startPruneLoop reruns retention pruning daily (plus the boot pass in main).
// Without this, retention_days only takes effect on restart and disk grows
// unbounded between restarts.
func startPruneLoop(app *App) {
	go func() {
		var t = time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for range t.C {
			pruneOnce(app)
		}
	}()
}

// listOldDetectionsByType is listOldDetections restricted to one detection
// type (for record.retention_by_type overrides).
func listOldDetectionsByType(store *DB, cutoff int64, dtype string) ([]Detection, error) {
	var out []Detection
	var q = "SELECT id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, extra_snapshots, locked FROM detections WHERE ended_at < ? AND det_type = ? AND (locked IS NULL OR locked=0) ORDER BY ended_at ASC"
	var rs, e = store.db.Query(q, cutoff, dtype)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	for rs.Next() {
		var d Detection
		var locked sql.NullInt64
		var extra sql.NullString
		if serr := rs.Scan(&d.id, &d.cameraID, &d.camName, &d.detType, &d.confidence, &d.startedAt, &d.endedAt, &d.snapshotPath, &d.clipPath, &extra, &locked); serr != nil {
			return nil, serr
		}
		if extra.Valid {
			d.extraSnap = extra.String
		}
		if locked.Valid && locked.Int64 != 0 {
			d.locked = 1
		}
		out = append(out, d)
	}
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// listOldDetections returns rows ended before cutoff, oldest first, excluding locked recordings.
func listOldDetections(store *DB, cutoff int64) ([]Detection, error) {
	var out []Detection
	var q = "SELECT id, camera_id, name, det_type, confidence, started_at, ended_at, snapshot_path, clip_path, extra_snapshots, locked FROM detections WHERE ended_at < ? AND (locked IS NULL OR locked=0) ORDER BY ended_at ASC"
	var rs, e = store.db.Query(q, cutoff)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	for rs.Next() {
		var d Detection
		var locked sql.NullInt64
		var extra sql.NullString
		if serr := rs.Scan(&d.id, &d.cameraID, &d.camName, &d.detType, &d.confidence, &d.startedAt, &d.endedAt, &d.snapshotPath, &d.clipPath, &extra, &locked); serr != nil {
			return nil, serr
		}
		if extra.Valid {
			d.extraSnap = extra.String
		}
		if locked.Valid && locked.Int64 != 0 {
			d.locked = 1
		}
		out = append(out, d)
	}
	if rerr := rs.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}
