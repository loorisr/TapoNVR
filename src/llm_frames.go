package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
)

// ---------------- clip keyframes (for the vision LLM) -------------------
// Video analysis sends a handful of keyframes sampled evenly across the clip:
// vision models take images, not video files, and IDR frames decode on their
// own with the pure-Go hi264 decoder already used for snapshots (no ffmpeg).

// clipFrame is one decoded keyframe: JPEG bytes and its offset in the clip.
type clipFrame struct {
	jpeg []byte
	at   time.Duration
}

// maxClipKeyframes bounds how many IDR access units are kept in memory while
// scanning a TS clip (a few hundred KB each).
const maxClipKeyframes = 240

// keyframeAU is a candidate IDR access unit (Annex-B NALUs incl. SPS/PPS).
type keyframeAU struct {
	au [][]byte
	at time.Duration
}

// extractClipKeyframes decodes up to n keyframes spread evenly over the clip
// (mp4 or ts) and returns them as JPEG, long edge capped at maxPx.
func extractClipKeyframes(path string, n int, maxPx int) ([]clipFrame, error) {
	if n <= 0 {
		return nil, nil
	}
	var kfs []keyframeAU
	var err error
	if strings.HasSuffix(strings.ToLower(path), ".ts") {
		kfs, err = tsKeyframes(path)
	} else {
		kfs, err = mp4Keyframes(path, n)
	}
	if err != nil {
		return nil, err
	}
	if len(kfs) == 0 {
		return nil, fmt.Errorf("no keyframe in clip")
	}
	var out []clipFrame
	for _, i := range evenlySpaced(len(kfs), n) {
		img, ok := decodeH264AUToImage(kfs[i].au, "clip")
		if !ok {
			continue
		}
		if jb := encodeJPEGMax(img, maxPx, 80); jb != nil {
			out = append(out, clipFrame{jpeg: jb, at: kfs[i].at})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no decodable keyframe in clip")
	}
	return out, nil
}

// evenlySpaced picks up to n indices spread across [0,total): first and last
// included, in order, without duplicates.
func evenlySpaced(total, n int) []int {
	if total <= 0 || n <= 0 {
		return nil
	}
	if n >= total {
		var all = make([]int, total)
		for i := range all {
			all[i] = i
		}
		return all
	}
	if n == 1 {
		return []int{total / 2}
	}
	var out []int
	var last = -1
	for k := 0; k < n; k++ {
		var i = (k*(total-1) + (n-1)/2) / (n - 1)
		if i != last {
			out = append(out, i)
			last = i
		}
	}
	return out
}

// mp4Keyframes reads the clip's H264 track and returns its sync samples (the
// payloads are read only for the n samples that will be decoded).
func mp4Keyframes(path string, n int) ([]keyframeAU, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pres pmp4.Presentation
	if err := pres.Unmarshal(f); err != nil {
		return nil, fmt.Errorf("mp4: %w", err)
	}
	var track *pmp4.Track
	var codec *mp4codecs.H264
	for _, t := range pres.Tracks {
		if c, ok := t.Codec.(*mp4codecs.H264); ok {
			track, codec = t, c
			break
		}
	}
	if track == nil || len(codec.SPS) == 0 || len(codec.PPS) == 0 {
		return nil, fmt.Errorf("mp4: no H264 track")
	}
	type syncSample struct {
		s  *pmp4.Sample
		at time.Duration
	}
	var syncs []syncSample
	var dts uint64
	var scale = uint64(track.TimeScale)
	if scale == 0 {
		scale = 90000
	}
	for _, s := range track.Samples {
		if !s.IsNonSyncSample {
			syncs = append(syncs, syncSample{s: s, at: time.Duration(dts * uint64(time.Second) / scale)})
		}
		dts += uint64(s.Duration)
	}
	var out []keyframeAU
	for _, i := range evenlySpaced(len(syncs), n) {
		payload, perr := syncs[i].s.GetPayload()
		if perr != nil {
			continue
		}
		var au = [][]byte{codec.SPS, codec.PPS}
		au = append(au, splitAVCC(payload)...)
		out = append(out, keyframeAU{au: au, at: syncs[i].at})
	}
	return out, nil
}

// splitAVCC splits a length-prefixed (4-byte) AVCC payload into NALUs.
func splitAVCC(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 4 {
		var l = int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		b = b[4:]
		if l <= 0 || l > len(b) {
			break
		}
		out = append(out, b[:l])
		b = b[l:]
	}
	return out
}

// tsKeyframes demuxes a TS clip (the transmux fallback) and keeps every IDR
// access unit with the latest SPS/PPS prepended.
func tsKeyframes(path string) ([]keyframeAU, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := &mpegts.Reader{R: f}
	if err := r.Initialize(); err != nil {
		return nil, fmt.Errorf("ts: %w", err)
	}
	var track *mpegts.Track
	for _, t := range r.Tracks() {
		if _, ok := t.Codec.(*tscodecs.H264); ok {
			track = t
			break
		}
	}
	if track == nil {
		return nil, fmt.Errorf("ts: no H264 track")
	}
	var sps, pps []byte
	var first int64 = -1
	var out []keyframeAU
	r.OnDataH264(track, func(pts int64, _ int64, au [][]byte) error {
		var idr bool
		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}
			switch h264.NALUType(nalu[0] & 0x1F) {
			case h264.NALUTypeSPS:
				sps = append([]byte(nil), nalu...)
			case h264.NALUTypePPS:
				pps = append([]byte(nil), nalu...)
			case h264.NALUTypeIDR:
				idr = true
			}
		}
		if first < 0 {
			first = pts
		}
		if !idr || sps == nil || pps == nil || len(out) >= maxClipKeyframes {
			return nil
		}
		var cp = [][]byte{sps, pps}
		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}
			var t = h264.NALUType(nalu[0] & 0x1F)
			if t == h264.NALUTypeSPS || t == h264.NALUTypePPS {
				continue
			}
			cp = append(cp, append([]byte(nil), nalu...))
		}
		out = append(out, keyframeAU{au: cp, at: time.Duration((pts - first) * int64(time.Second) / 90000)})
		return nil
	})
	for {
		if rerr := r.Read(); rerr != nil {
			if !errors.Is(rerr, io.EOF) && len(out) == 0 {
				return nil, fmt.Errorf("ts: %w", rerr)
			}
			break
		}
	}
	return out, nil
}

// encodeJPEGMax encodes img as JPEG with its long edge capped at maxPx
// (aspect kept; 0 keeps the original size). Returns nil on failure.
func encodeJPEGMax(img image.Image, maxPx int, quality int) []byte {
	var b = img.Bounds()
	var w, h = b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	var out = img
	if maxPx > 0 && (w > maxPx || h > maxPx) {
		var dw, dh = maxPx, maxPx
		if w >= h {
			dh = max(1, (h*maxPx+w/2)/w)
		} else {
			dw = max(1, (w*maxPx+h/2)/h)
		}
		out = boxDownscale(img, b, dw, dh)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: quality}); err != nil || buf.Len() == 0 {
		return nil
	}
	return buf.Bytes()
}
