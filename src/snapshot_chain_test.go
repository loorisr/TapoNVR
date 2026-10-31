package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testSnapApp() *App {
	var cfg Config
	configDefaults(&cfg)
	return &App{cfg: &cfg}
}

// swapSnapshotGrabbers replaces the snapshot source seams for one test and
// restores them afterwards. Tests in this package are not parallel.
func swapSnapshotGrabbers(t *testing.T,
	buf func(*App, string, string) (bool, time.Time),
	native func(*App, string, string, time.Time) bool,
	rtsp func(*App, string, string, time.Duration) bool,
	httpGrab func(*App, string, string, time.Time) bool,
) {
	t.Helper()
	var oldBuf, oldNative, oldRTSP, oldHTTP = snapshotGrabBuffer, snapshotGrabNative, snapshotGrabRTSP, snapshotGrabHTTP
	snapshotGrabBuffer, snapshotGrabNative, snapshotGrabRTSP, snapshotGrabHTTP = buf, native, rtsp, httpGrab
	t.Cleanup(func() {
		snapshotGrabBuffer, snapshotGrabNative, snapshotGrabRTSP, snapshotGrabHTTP = oldBuf, oldNative, oldRTSP, oldHTTP
	})
}

func TestSnapshotChainUsesBufferImmediately(t *testing.T) {
	var dir = t.TempDir()
	var dest = filepath.Join(dir, "snap.jpg")
	var liveCalls int
	swapSnapshotGrabbers(t,
		func(app *App, camID, dest string) (bool, time.Time) {
			_ = os.WriteFile(dest, []byte("buffer"), 0o644)
			// Older than snapshotMaxIDRAge but well within the ring's life:
			// the old code went through the whole live chain here.
			return true, time.Now().Add(-4 * time.Second)
		},
		func(app *App, camID, dest string, deadline time.Time) bool { liveCalls++; return false },
		func(app *App, camID, dest string, timeout time.Duration) bool { liveCalls++; return false },
		func(app *App, camID, dest string, deadline time.Time) bool { liveCalls++; return false },
	)

	var start = time.Now()
	var g = grabSnapshotChainMetaBudget(testSnapApp(), "cam0", dest, false, 200*time.Millisecond)
	if !g.ok || g.source != "buffer" {
		t.Fatalf("grab=%+v, want buffer", g)
	}
	if liveCalls != 0 {
		t.Fatalf("live grabbers called %d times for a usable buffer frame", liveCalls)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("buffer-first took %v, want an immediate return", elapsed)
	}
	if raw, err := os.ReadFile(dest); err != nil || string(raw) != "buffer" {
		t.Fatalf("dest=%q err=%v, want the buffer JPEG", raw, err)
	}
}

func TestSnapshotChainVeryStaleBufferSkipsLiveRetry(t *testing.T) {
	var dir = t.TempDir()
	var dest = filepath.Join(dir, "snap.jpg")
	var liveCalls int
	swapSnapshotGrabbers(t,
		func(app *App, camID, dest string) (bool, time.Time) {
			_ = os.WriteFile(dest, []byte("buffer"), 0o644)
			return true, time.Now().Add(-time.Minute)
		},
		func(app *App, camID, dest string, deadline time.Time) bool { liveCalls++; return false },
		func(app *App, camID, dest string, timeout time.Duration) bool { liveCalls++; return false },
		func(app *App, camID, dest string, deadline time.Time) bool { liveCalls++; return false },
	)

	var g = grabSnapshotChainMetaBudget(testSnapApp(), "cam0", dest, false, time.Second)
	if !g.ok || g.source != "buffer" {
		t.Fatalf("grab=%+v, want the stale buffer", g)
	}
	if liveCalls != 0 {
		t.Fatalf("live grabbers called %d times for a very stale buffer", liveCalls)
	}
}

func TestSnapshotChainBestModeTriesNativeBriefly(t *testing.T) {
	for _, nativeOK := range []bool{false, true} {
		var dir = t.TempDir()
		var dest = filepath.Join(dir, "snap.jpg")
		var nativeRemaining time.Duration
		var liveCalls int
		swapSnapshotGrabbers(t,
			func(app *App, camID, dest string) (bool, time.Time) {
				_ = os.WriteFile(dest, []byte("buffer"), 0o644)
				return true, time.Now().Add(-3 * time.Second)
			},
			func(app *App, camID, dest string, deadline time.Time) bool {
				nativeRemaining = time.Until(deadline)
				if nativeOK {
					return os.WriteFile(dest, []byte("native"), 0o644) == nil
				}
				return false
			},
			func(app *App, camID, dest string, timeout time.Duration) bool { liveCalls++; return false },
			func(app *App, camID, dest string, deadline time.Time) bool { liveCalls++; return false },
		)

		var g = grabSnapshotChainMetaBudget(testSnapApp(), "cam0", dest, true, snapshotChainBudget)
		if nativeRemaining <= 0 || nativeRemaining > snapshotBestNativeBudget+100*time.Millisecond {
			t.Fatalf("native deadline=%v, want <= %v", nativeRemaining, snapshotBestNativeBudget)
		}
		if liveCalls != 0 {
			t.Fatalf("best mode must not fall through to RTSP/HTTP (%d calls)", liveCalls)
		}
		if nativeOK {
			if g.source != "native" {
				t.Fatalf("grab=%+v, want native", g)
			}
			if raw, _ := os.ReadFile(dest); string(raw) != "native" {
				t.Fatalf("dest=%q, want the native frame", raw)
			}
		} else {
			if g.source != "buffer" {
				t.Fatalf("grab=%+v, want buffer fallback", g)
			}
			if raw, _ := os.ReadFile(dest); string(raw) != "buffer" {
				t.Fatalf("dest=%q, want the untouched buffer frame", raw)
			}
		}
	}
}

func TestSnapshotChainNoBufferRespectsBudget(t *testing.T) {
	var dir = t.TempDir()
	var dest = filepath.Join(dir, "snap.jpg")
	var calls []string
	swapSnapshotGrabbers(t,
		func(app *App, camID, dest string) (bool, time.Time) { return false, time.Time{} },
		func(app *App, camID, dest string, deadline time.Time) bool {
			calls = append(calls, "native")
			time.Sleep(minDurationTest(20*time.Millisecond, time.Until(deadline)))
			return false
		},
		func(app *App, camID, dest string, timeout time.Duration) bool {
			calls = append(calls, "rtsp")
			time.Sleep(minDurationTest(20*time.Millisecond, timeout))
			return false
		},
		func(app *App, camID, dest string, deadline time.Time) bool {
			calls = append(calls, "http")
			time.Sleep(minDurationTest(20*time.Millisecond, time.Until(deadline)))
			return false
		},
	)

	var start = time.Now()
	var g = grabSnapshotChainMetaBudget(testSnapApp(), "cam0", dest, false, 150*time.Millisecond)
	var elapsed = time.Since(start)
	if g.ok {
		t.Fatalf("grab=%+v, want failure", g)
	}
	if elapsed > 120*time.Millisecond {
		t.Fatalf("live chain took %v, budget was 150ms", elapsed)
	}
	if len(calls) != 3 {
		t.Fatalf("sources called=%v, want native+rtsp+http", calls)
	}
}

// minDurationTest is the test-local min for durations (the production code
// uses snapshotFetchTimeout).
func minDurationTest(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}

func TestSeiBoxesAt(t *testing.T) {
	var cfg Config
	configDefaults(&cfg)
	cfg.Cameras = []CameraCfg{{Name: "cam"}}
	var app = &App{cfg: &cfg}
	var d = newRTSPDetector(app, &cfg.Cameras[0], "cam0")
	registerRTSPDetector(d)
	defer unregisterRTSPDetector("cam0")

	var base = time.Now().Add(-2 * time.Second)
	d.mu.Lock()
	d.pushBoxHistoryLocked([]detBox{box("person", 0.1, 0.1, 0.4, 0.4)}, base)
	d.pushBoxHistoryLocked([]detBox{box("person", 0.2, 0.2, 0.8, 0.8)}, base.Add(time.Second))
	d.mu.Unlock()

	// Closest sample wins, not simply the newest one.
	boxes, at, ok := seiBoxesAt("cam0", base.Add(900*time.Millisecond), snapshotMaxBoxPairAge)
	if !ok || len(boxes) != 1 || boxes[0].X2 != 0.8 {
		t.Fatalf("seiBoxesAt=%v ok=%v, want the sample at +1s", boxes, ok)
	}
	if !at.Equal(base.Add(time.Second)) {
		t.Fatalf("seiBoxesAt at=%v, want %v", at, base.Add(time.Second))
	}
	if _, _, ok := seiBoxesAt("cam0", base.Add(10*time.Second), snapshotMaxBoxPairAge); ok {
		t.Fatal("a sample outside the tolerance must not match")
	}
	if _, _, ok := seiBoxesAt("nope", base, snapshotMaxBoxPairAge); ok {
		t.Fatal("unknown camera must yield no boxes")
	}
}
