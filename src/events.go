package main

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ---------------- live events: SSE broadcast ------------------

// eventHub fans out detection events to connected WebUI clients over SSE.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan string]bool
}

var hub = &eventHub{subs: map[chan string]bool{}}

// broadcast sends one JSON event (as an SSE "detection" data line) to every
// subscriber. Non-blocking: drops a slow client rather than stalling. The
// subscriber set is copied under the lock and each send happens outside it,
// so a slow/blocked client can never stall subscribe/unsubscribe or starve
// other broadcasts.
func (h *eventHub) broadcast(payload string) {
	h.mu.Lock()
	var subs = make([]chan string, 0, len(h.subs))
	for c := range h.subs {
		subs = append(subs, c)
	}
	h.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- payload:
		default:
			// drop for slow clients
		}
	}
}

func (h *eventHub) subscribe() chan string {
	var c = make(chan string, 16)
	h.mu.Lock()
	h.subs[c] = true
	h.mu.Unlock()
	return c
}

func (h *eventHub) unsubscribe(c chan string) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

// hasSubscribers reports whether at least one SSE client is connected. Used to
// skip per-frame work (e.g. the live-boxes broadcast) when nobody is watching.
func (h *eventHub) hasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// broadcastDetection emits a live "motion detected" event for the UI.
// camera_id is the stable key (names may collide); camera name is kept for
// backwards compat with older UIs. bbox is the RTSP SEI boxes JSON ("" when
// none) so a live overlay can draw them.
func broadcastDetection(camID, camName string, dtype string, bbox string) {
	var body = "{" + kv("camera_id", camID) + "," + kv("camera", camName) + "," + kv("type", dtype)
	if clean, ok := sanitizeBboxJSON(bbox); ok {
		body += "," + kvRaw("bbox", clean)
	}
	body += "}"
	hub.broadcast(body)
}

// broadcastLiveBoxes emits the camera's current SEI boxes for the live overlay.
// bbox is the JSON array produced by boxesJSON (validated again here so one bad
// frame can never break the SSE stream).
func broadcastLiveBoxes(camID, camName string, bbox string) {
	var clean, ok = sanitizeBboxJSON(bbox)
	if !ok {
		return
	}
	var body = "{" + kv("camera_id", camID) + "," + kv("camera", camName) + "," +
		kv("kind", "boxes") + "," + kvRaw("bbox", clean) + "}"
	hub.broadcast(body)
}

// broadcastVision emits a live "vision analyzed" event so the UI can refresh
// badges/overlays without a full reload.
func broadcastVision(camID, camName string, dtype string, detID string, count int, top string) {
	var body = "{" + kv("camera_id", camID) + "," + kv("camera", camName) + "," + kv("type", dtype) + "," + kv("detection_id", detID) + "," +
		"\"vision_count\":" + strconv.Itoa(count) + "," + kv("vision_top", top) + "," + kv("kind", "vision") + "}"
	hub.broadcast(body)
}

// handleEvents streams SSE events to the WebUI. Auth is enforced by the
// route registration in serve(); the handler itself assumes authOK passed.
func handleEvents(app *App, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Don't let proxies buffer the stream, and tell EventSource how fast to
	// retry after a drop (instead of hammering reconnects).
	w.Header().Set("X-Accel-Buffering", "no")
	var fl, _ = w.(http.Flusher)
	// Send an initial comment to establish the stream.
	_, _ = w.Write([]byte("retry: 3000\n: connected\n\n"))
	if fl != nil {
		fl.Flush()
	}
	var ch = hub.subscribe()
	defer hub.unsubscribe(ch)
	var ctx = r.Context()
	// Keepalive comment every 15s so proxies don't close the idle connection.
	var ticker = time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			if fl != nil {
				fl.Flush()
			}
		case msg := <-ch:
			_, _ = w.Write([]byte("event: detection\ndata: " + msg + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
	}
}
