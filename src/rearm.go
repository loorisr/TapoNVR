package main

import (
	"strings"
	"sync"
)

// ---------------- re-arm on absence ----------------------------------------
// A parked car or a seated person keeps emitting events (intermittently,
// forking new detection groups). Each consumer below acts once per presence:
// after it fires for a camera+type, it stays disarmed until that camera has
// gone <consumer>.rearm_s seconds without any event of that type. Presence
// (the last event time) is shared; the armed state is per consumer, each
// with its own window. A window of 0 disables the limit for that consumer.

type rearmConsumer int

const (
	rearmPush   rearmConsumer = iota // alerts.pushover.rearm_s
	rearmVision                      // vision.rearm_s (YOLO on new groups)
	rearmMQTT                        // mqtt.rearm_s (event + detection messages)
	rearmLLM                         // llm.rearm_s (automatic analysis)
	rearmRecord                      // record.rearm_s (new detection groups)
	rearmCount
)

var rearmNames = [rearmCount]string{"pushover", "vision", "mqtt", "llm", "record"}

type rearmWindows [rearmCount]int

func rearmWindowsFromConfig(c *Config) rearmWindows {
	var w rearmWindows
	if c == nil {
		return w
	}
	w[rearmPush] = c.Alerts.Pushover.RearmS
	w[rearmVision] = c.Vision.RearmS
	w[rearmMQTT] = c.Mqtt.RearmS
	w[rearmLLM] = c.LLM.RearmS
	w[rearmRecord] = c.Record.RearmS
	return w
}

type rearmState struct {
	lastEventAt int64
	disarmed    [rearmCount]bool
}

var rearmMtx sync.Mutex
var rearmMap = map[string]*rearmState{}

func rearmKey(camID string, eventType string) string {
	return camID + "|" + normalizeDetType(eventType)
}

// rearmTouch records a raw detection event. It runs before any consumer is
// asked, so the gap since the previous event decides which consumers re-arm
// (a consumer asking later would otherwise see the fresh timestamp and lose
// the gap).
func rearmTouch(camID string, eventType string, now int64, w rearmWindows) {
	if camID == "" {
		return
	}
	var any bool
	for _, s := range w {
		if s > 0 {
			any = true
		}
	}
	if !any {
		return
	}
	var key = rearmKey(camID, eventType)
	rearmMtx.Lock()
	defer rearmMtx.Unlock()
	var st = rearmMap[key]
	if st == nil {
		rearmMap[key] = &rearmState{lastEventAt: now}
		return
	}
	var gap = now - st.lastEventAt
	for c, s := range w {
		if s > 0 && gap >= int64(s) {
			st.disarmed[c] = false
		}
	}
	st.lastEventAt = now
}

// rearmTake reports whether consumer c may act for camID+eventType, and
// disarms it when it does. Unknown keys (no event recorded) are armed.
func rearmTake(c rearmConsumer, camID string, eventType string, now int64, rearmS int) bool {
	if rearmS <= 0 || camID == "" {
		return true
	}
	var key = rearmKey(camID, eventType)
	rearmMtx.Lock()
	defer rearmMtx.Unlock()
	var st = rearmMap[key]
	if st == nil {
		st = &rearmState{lastEventAt: now}
		st.disarmed[c] = true
		rearmMap[key] = st
		return true
	}
	if st.disarmed[c] && now-st.lastEventAt >= int64(rearmS) {
		st.disarmed[c] = false
	}
	if st.disarmed[c] {
		return false
	}
	st.disarmed[c] = true
	return true
}

// rearmRestore re-arms consumer c for a key whose action did not happen
// (failed or throttled send).
func rearmRestore(c rearmConsumer, key string) {
	if key == "" {
		return
	}
	rearmMtx.Lock()
	if st := rearmMap[key]; st != nil {
		st.disarmed[c] = false
	}
	rearmMtx.Unlock()
}

// ---------------- per-detection gates --------------------------------------
// The decision is taken once, when a new detection group starts, and applies
// to everything that group triggers later (burst extras, clip-ready and
// YOLO/LLM republishes), so a muted group stays fully silent.

type detGateEntry struct {
	at    int64
	muted [rearmCount]bool
}

// detGateTTL bounds how long a group decision is kept: longer than any
// clip assembly / queued LLM analysis that could still republish it.
const detGateTTL = 24 * 3600

var detGateMtx sync.Mutex
var detGateMap = map[string]*detGateEntry{}

// detGateMute records that consumer c must stay silent for detection id.
func detGateMute(id string, c rearmConsumer, now int64) {
	if id == "" {
		return
	}
	detGateMtx.Lock()
	defer detGateMtx.Unlock()
	var e = detGateMap[id]
	if e == nil {
		// Prune on insert: at most one entry per detection group per day.
		for k, v := range detGateMap {
			if now-v.at > detGateTTL {
				delete(detGateMap, k)
			}
		}
		e = &detGateEntry{at: now}
		detGateMap[id] = e
	}
	e.muted[c] = true
}

// detGateUnmute lets consumer c act again for detection id (tag promotion to
// a type that is armed).
func detGateUnmute(id string, c rearmConsumer) {
	detGateMtx.Lock()
	if e := detGateMap[id]; e != nil {
		e.muted[c] = false
	}
	detGateMtx.Unlock()
}

// detGateMuted reports whether consumer c is muted for detection id.
func detGateMuted(id string, c rearmConsumer) bool {
	if id == "" {
		return false
	}
	detGateMtx.Lock()
	defer detGateMtx.Unlock()
	var e = detGateMap[id]
	return e != nil && e.muted[c]
}

// mqttRearmTake applies mqtt.rearm_s to one event message. Always true when
// MQTT is off, so a disabled consumer never consumes the slot.
func mqttRearmTake(app *App, camID string, dtype string, now int64) bool {
	if app == nil || app.cfg == nil || !app.cfg.Mqtt.Enabled {
		return true
	}
	if rearmTake(rearmMQTT, camID, dtype, now, app.cfg.Mqtt.RearmS) {
		return true
	}
	logIt(LDEBUG, "mqtt event suppressed (object still present, not re-armed)", map[string]string{"camera_id": camID, "type": normalizeDetType(dtype)})
	return false
}

// rearmGateNewGroup takes the vision/llm/mqtt decision for a new detection
// group and mutes the consumers that are not re-armed. Only active consumers
// are asked (an inactive one must not consume its slot). Returns whether the
// group's MQTT messages may be published.
func rearmGateNewGroup(app *App, detID string, camID string, dtype string, now int64) bool {
	var muted []string
	if visionShouldAnalyze(app) && !rearmTake(rearmVision, camID, dtype, now, app.cfg.Vision.RearmS) {
		detGateMute(detID, rearmVision, now)
		muted = append(muted, rearmNames[rearmVision])
	}
	if llmAutoActive(app) && !rearmTake(rearmLLM, camID, dtype, now, app.cfg.LLM.RearmS) {
		detGateMute(detID, rearmLLM, now)
		muted = append(muted, rearmNames[rearmLLM])
	}
	var mqttOK = mqttRearmTake(app, camID, dtype, now)
	if !mqttOK {
		detGateMute(detID, rearmMQTT, now)
		muted = append(muted, rearmNames[rearmMQTT])
	}
	if len(muted) > 0 {
		logIt(LDEBUG, "detection group muted (object still present, not re-armed)", map[string]string{"id": detID, "camera_id": camID, "type": normalizeDetType(dtype), "muted": strings.Join(muted, ",")})
	}
	return mqttOK
}
