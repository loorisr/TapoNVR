package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

// ---------------- vision: burn YOLO boxes into snapshots (serve-time) ----
// The stored JPG is never modified: boxes are drawn on the fly when
// GET /api/detections/{id}/snapshot[?boxes=1] is requested and the detection
// has a positive vision result for that exact file. Pure stdlib (no font
// dependency): rectangles + a tiny embedded 5x7 bitmap font for labels.

// maxAnnotatePixels rejects absurd source frames before allocating a full
// RGBA buffer (12 MP covers 4K with margin; 2 concurrent renders stay ~100 MB).
const maxAnnotatePixels = 12_000_000

// visionPalette holds distinct box colors; picked by label hash.
var visionPalette = []color.RGBA{
	{0x4C, 0xAF, 0x50, 0xFF}, // green
	{0x21, 0x96, 0xF3, 0xFF}, // blue
	{0xFF, 0x98, 0x00, 0xFF}, // orange
	{0xE9, 0x1E, 0x63, 0xFF}, // pink
	{0xFF, 0xEB, 0x3B, 0xFF}, // yellow
	{0x00, 0xBC, 0xD4, 0xFF}, // cyan
	{0x9C, 0x27, 0xB0, 0xFF}, // purple
	{0xFF, 0x57, 0x22, 0xFF}, // deep orange
}

func visionLabelColor(label string) color.RGBA {
	var h uint32
	for i := 0; i < len(label); i++ {
		h = h*31 + uint32(label[i])
	}
	return visionPalette[int(h%uint32(len(visionPalette)))]
}

// visionFont is a 5x7 bitmap font ('#' = on). Covers what YOLO labels need:
// A-Z 0-9 space . : - _ / + % ( ).
var visionFont = map[byte][7]string{
	'A': {".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'B': {"####.", "#...#", "#...#", "####.", "#...#", "#...#", "####."},
	'C': {".####", "#....", "#....", "#....", "#....", "#....", ".####"},
	'D': {"####.", "#...#", "#...#", "#...#", "#...#", "#...#", "####."},
	'E': {"#####", "#....", "#....", "####.", "#....", "#....", "#####"},
	'F': {"#####", "#....", "#....", "####.", "#....", "#....", "#...."},
	'G': {".####", "#....", "#....", "#.###", "#...#", "#...#", ".###."},
	'H': {"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'I': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "#####"},
	'J': {"..###", "...#.", "...#.", "...#.", "...#.", "#..#.", ".##.."},
	'K': {"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"},
	'L': {"#....", "#....", "#....", "#....", "#....", "#....", "#####"},
	'M': {"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"},
	'N': {"#...#", "##..#", "##..#", "#.#.#", "#..##", "#..##", "#...#"},
	'O': {".###.", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'P': {"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."},
	'Q': {".###.", "#...#", "#...#", "#...#", "#.#.#", "#..#.", ".##.#"},
	'R': {"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"},
	'S': {".####", "#....", "#....", ".###.", "....#", "....#", "####."},
	'T': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'U': {"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'V': {"#...#", "#...#", "#...#", "#...#", "#...#", ".#.#.", "..#.."},
	'W': {"#...#", "#...#", "#...#", "#.#.#", "#.#.#", "##.##", "#...#"},
	'X': {"#...#", "#...#", ".#.#.", "..#..", ".#.#.", "#...#", "#...#"},
	'Y': {"#...#", "#...#", ".#.#.", "..#..", "..#..", "..#..", "..#.."},
	'Z': {"#####", "....#", "...#.", "..#..", ".#...", "#....", "#####"},
	'0': {".###.", "#..##", "#.#.#", "#.#.#", "##..#", "#...#", ".###."},
	'1': {"..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."},
	'2': {".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"},
	'3': {"####.", "....#", "....#", ".###.", "....#", "....#", "####."},
	'4': {"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."},
	'5': {"#####", "#....", "####.", "....#", "....#", "#...#", ".###."},
	'6': {".###.", "#....", "#....", "####.", "#...#", "#...#", ".###."},
	'7': {"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
	'8': {".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."},
	'9': {".###.", "#...#", "#...#", ".####", "....#", "....#", ".###."},
	' ': {".....", ".....", ".....", ".....", ".....", ".....", "....."},
	'.': {".....", ".....", ".....", ".....", ".....", ".##..", ".##.."},
	':': {".....", ".##..", ".##..", ".....", ".##..", ".##..", "....."},
	'-': {".....", ".....", ".....", "#####", ".....", ".....", "....."},
	'_': {".....", ".....", ".....", ".....", ".....", ".....", "#####"},
	'/': {"....#", "....#", "...#.", "..#..", ".#...", "#....", "#...."},
	'+': {".....", "..#..", "..#..", "#####", "..#..", "..#..", "....."},
	'%': {"##..#", "##..#", "..#..", "..#..", ".#..#", ".#..#", "....."},
	'(': {"...#.", "..#..", ".#...", ".#...", ".#...", "..#..", "...#."},
	')': {".#...", "..#..", "...#.", "...#.", "...#.", "..#..", ".#..."},
}

func fillRect(img *image.RGBA, x1, y1, x2, y2 int, c color.RGBA) {
	var b = img.Bounds()
	if x1 < b.Min.X {
		x1 = b.Min.X
	}
	if y1 < b.Min.Y {
		y1 = b.Min.Y
	}
	if x2 > b.Max.X {
		x2 = b.Max.X
	}
	if y2 > b.Max.Y {
		y2 = b.Max.Y
	}
	for y := y1; y < y2; y++ {
		for x := x1; x < x2; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func strokeRect(img *image.RGBA, x1, y1, x2, y2, thick int, c color.RGBA) {
	fillRect(img, x1, y1, x2, y1+thick, c)
	fillRect(img, x1, y2-thick, x2, y2, c)
	fillRect(img, x1, y1, x1+thick, y2, c)
	fillRect(img, x2-thick, y1, x2, y2, c)
}

// drawVisionText renders upper-case text at (x,y) with the 5x7 font scaled
// by `scale` (1 or 2). Unknown glyphs render as blank.
func drawVisionText(img *image.RGBA, x, y, scale int, s string, c color.RGBA) int {
	var cx = x
	for i := 0; i < len(s); i++ {
		var ch = s[i]
		if ch >= 'a' && ch <= 'z' {
			ch = ch - 'a' + 'A'
		}
		if g, ok := visionFont[ch]; ok {
			for r := 0; r < 7; r++ {
				var row = g[r]
				for col := 0; col < 5 && col < len(row); col++ {
					if row[col] == '#' {
						fillRect(img, cx+col*scale, y+r*scale, cx+(col+1)*scale, y+(r+1)*scale, c)
					}
				}
			}
		}
		cx += 6 * scale
	}
	return cx - x
}

// visionDetRect resolves one detection to pixel coords. box_norm (0..1) wins
// when valid; otherwise box is treated as original-image pixels (clamped).
func visionDetRect(d visionDetection, w, h int) (int, int, int, int, bool) {
	var n = d.BoxNorm
	if n.XMax > n.XMin && n.YMax > n.YMin &&
		n.XMin >= 0 && n.YMin >= 0 && n.XMax <= 1.0 && n.YMax <= 1.0 {
		return int(n.XMin * float64(w)), int(n.YMin * float64(h)),
			int(n.XMax*float64(w) + 0.5), int(n.YMax*float64(h) + 0.5), true
	}
	var b = d.Box
	var x1, y1, x2, y2 = int(b.XMin), int(b.YMin), int(b.XMax + 0.5), int(b.YMax + 0.5)
	if x1 < 0 {
		x1 = 0
	}
	if y1 < 0 {
		y1 = 0
	}
	if x2 > w {
		x2 = w
	}
	if y2 > h {
		y2 = h
	}
	if x2 <= x1 || y2 <= y1 {
		return 0, 0, 0, 0, false
	}
	return x1, y1, x2, y2, true
}

// parseVisionDets decodes the stored vision_json array (tolerant: "" -> nil).
func parseVisionDets(raw string) []visionDetection {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []visionDetection
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// drawVisionBoxes returns a JPEG with YOLO boxes + labels burned in.
// Input may be JPEG or PNG; output is always JPEG (quality 88).
func drawVisionBoxes(imgBytes []byte, dets []visionDetection) ([]byte, error) {
	return drawVisionBoxesMax(imgBytes, dets, 0)
}

// drawVisionBoxesMax is drawVisionBoxes with an optional long-edge cap
// (0 = original size). The cap keeps grid thumbnails small enough to cache.
func drawVisionBoxesMax(imgBytes []byte, dets []visionDetection, maxPx int) ([]byte, error) {
	if len(dets) == 0 {
		return nil, fmt.Errorf("no detections")
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
	for _, d := range dets {
		x1, y1, x2, y2, ok := visionDetRect(d, w, h)
		if !ok {
			continue
		}
		x1 += ox
		y1 += oy
		x2 += ox
		y2 += oy
		var c = visionLabelColor(d.Label)
		strokeRect(rgba, x1, y1, x2, y2, thick, c)
		var label = strings.ToUpper(d.Label) + fmt.Sprintf(" %.2f", d.Confidence)
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
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeRGBAJPEG encodes an annotated frame, optionally downscaling its long
// edge to maxPx first. Grid thumbnails ask for a small copy so the cache and
// browser memory stay bounded even for 4K snapshots.
func encodeRGBAJPEG(img *image.RGBA, maxPx int) ([]byte, error) {
	if img == nil {
		return nil, fmt.Errorf("nil image")
	}
	var out image.Image = img
	if maxPx > 0 {
		var b = img.Bounds()
		var w, h = b.Dx(), b.Dy()
		if w > maxPx || h > maxPx {
			var nw, nh = w, h
			if w >= h {
				nw = maxPx
				nh = (h*maxPx + w/2) / w
			} else {
				nh = maxPx
				nw = (w*maxPx + h/2) / h
			}
			if nw < 1 {
				nw = 1
			}
			if nh < 1 {
				nh = 1
			}
			out = boxDownscale(img, b, nw, nh)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
