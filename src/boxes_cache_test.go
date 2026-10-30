package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEncodeRGBAJPEGDownscale(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 80, 255})
		}
	}
	raw, err := encodeRGBAJPEG(img, 160)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := decoded.Bounds(); b.Dx() != 160 || b.Dy() != 90 {
		t.Fatalf("downscaled size = %dx%d want 160x90", b.Dx(), b.Dy())
	}
	raw, err = encodeRGBAJPEG(img, 0)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := decoded.Bounds(); b.Dx() != 640 || b.Dy() != 360 {
		t.Fatalf("original size = %dx%d want 640x360", b.Dx(), b.Dy())
	}
}

func TestBoxRenderMax(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"&max=":       0,
		"&max=abc":    0,
		"&max=10":     64,
		"&max=480":    480,
		"&max=999999": 3840,
		"&max=-5":     0,
	}
	for query, want := range cases {
		req := httptest.NewRequest("GET", "/api/detections/x/snapshot?boxes=1"+query, nil)
		if got := boxRenderMax(req); got != want {
			t.Errorf("boxRenderMax(%q)=%d want %d", query, got, want)
		}
	}
}

func TestServeNotModified(t *testing.T) {
	mt := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	etag := smallETag("f", "123")
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("If-Modified-Since", mt.Add(time.Hour).UTC().Format(http.TimeFormat))
	rec := httptest.NewRecorder()
	if !serveNotModified(rec, req, mt, etag) {
		t.Fatal("expected not modified")
	}
	if rec.Code != 304 {
		t.Fatalf("status=%d want 304", rec.Code)
	}
	// An older validator must still render the image.
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("If-Modified-Since", mt.Add(-time.Hour).UTC().Format(http.TimeFormat))
	rec = httptest.NewRecorder()
	if serveNotModified(rec, req, mt, etag) {
		t.Fatal("older If-Modified-Since should not be 304")
	}
	// ETag validation works without a modtime (bbox changes) and changes when
	// the boxes change.
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	if !serveNotModified(rec, req, time.Time{}, etag) || rec.Code != 304 {
		t.Fatalf("etag 304 failed: code=%d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	if serveNotModified(rec, req, time.Time{}, smallETag("f", "456")) {
		t.Fatal("different ETag should not be 304")
	}
}

func TestBoxesCacheMaxEntry(t *testing.T) {
	globalBoxesCache.mu.Lock()
	globalBoxesCache.entries = map[string]boxesCacheEntry{}
	globalBoxesCache.bytes = 0
	globalBoxesCache.mu.Unlock()
	defer func() {
		globalBoxesCache.mu.Lock()
		globalBoxesCache.entries = map[string]boxesCacheEntry{}
		globalBoxesCache.bytes = 0
		globalBoxesCache.mu.Unlock()
	}()
	// A single oversized render is served but not pinned in RAM.
	boxesCacheSet("big", make([]byte, boxesCacheMaxEntry+1))
	if _, ok := boxesCacheGet("big"); ok {
		t.Fatal("oversized entry was cached")
	}
	boxesCacheSet("small", []byte("jpeg"))
	got, ok := boxesCacheGet("small")
	if !ok || string(got) != "jpeg" {
		t.Fatalf("small cache get = %q ok=%v", got, ok)
	}
}
