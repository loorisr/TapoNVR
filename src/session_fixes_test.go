package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- go2rtc reconnect-storm supervisor ----

func resetStormForTest() {
	stormState.Lock()
	stormState.lines = nil
	stormState.pauses = 0
	stormState.until = time.Time{}
	stormState.Unlock()
}

func TestStormWriterSplitsLines(t *testing.T) {
	var sink bytes.Buffer
	var mu sync.Mutex
	var got []string
	var sw = &stormWriter{w: &sink, note: func(line string) {
		mu.Lock()
		got = append(got, line)
		mu.Unlock()
	}}
	_, _ = sw.Write([]byte("first line\nsecond"))
	_, _ = sw.Write([]byte(" line\nthird\n"))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "first line" || got[1] != "second line" || got[2] != "third" {
		t.Fatalf("lines=%q", got)
	}
	if !strings.Contains(sink.String(), "first line") || !strings.Contains(sink.String(), "third") {
		t.Fatalf("stderr copy lost: %q", sink.String())
	}
}

func TestStormNoteTriggersPause(t *testing.T) {
	resetStormForTest()
	defer resetStormForTest()
	for i := 0; i < stormThreshold; i++ {
		stormNoteLine(`11:00:00 DBG [streams] retry=0 to url=rtsp://cam`)
	}
	if d := stormPauseRemaining(); d <= 0 {
		t.Fatalf("storm not detected, pause=%v", d)
	}
	stormState.Lock()
	var pauses = stormState.pauses
	stormState.Unlock()
	if pauses != 1 {
		t.Fatalf("pauses=%d want 1", pauses)
	}
}

func TestStormNoteIgnoresNormalLines(t *testing.T) {
	resetStormForTest()
	defer resetStormForTest()
	for i := 0; i < stormThreshold*2; i++ {
		stormNoteLine(`11:00:00 INF [rtsp] new consumer stream=cam0`)
	}
	if d := stormPauseRemaining(); d > 0 {
		t.Fatalf("normal lines triggered a pause: %v", d)
	}
}

// ---- vision failure circuit breaker ----

func resetVisionBackoffForTest() {
	visionBackoffMtx.Lock()
	visionFailStreak = 0
	visionFailUntil = time.Time{}
	visionBackoffMtx.Unlock()
}

func TestVisionBackoffGrowsAndResets(t *testing.T) {
	resetVisionBackoffForTest()
	defer resetVisionBackoffForTest()

	if visionBackoffActive() {
		t.Fatal("backoff active before any failure")
	}
	visionNoteFailure()
	if !visionBackoffActive() {
		t.Fatal("backoff not active after a failure")
	}
	visionBackoffMtx.Lock()
	var first = visionFailUntil.Sub(time.Now())
	var streak1 = visionFailStreak
	visionBackoffMtx.Unlock()
	if streak1 != 1 || first < 10*time.Second {
		t.Fatalf("streak=%d wait=%v", streak1, first)
	}
	visionNoteFailure()
	visionBackoffMtx.Lock()
	var second = visionFailUntil.Sub(time.Now())
	var streak2 = visionFailStreak
	visionBackoffMtx.Unlock()
	if streak2 != 2 || second <= first {
		t.Fatalf("backoff did not grow: streak=%d first=%v second=%v", streak2, first, second)
	}
	visionNoteSuccess()
	visionBackoffMtx.Lock()
	var streak3 = visionFailStreak
	visionBackoffMtx.Unlock()
	if streak3 != 0 || visionBackoffActive() {
		t.Fatalf("success did not reset: streak=%d active=%v", streak3, visionBackoffActive())
	}
}
