package main

import (
	"strconv"
	"sync"
	"time"
)

// ---------------- app state ------------------------------------

type App struct {
	cfg   *Config
	db    *DB
	start int64
	boot  int64
}

func initApp(cfg *Config, store *DB) App {
	var a App
	a.cfg = cfg
	a.db = store
	a.start = time.Now().Unix()
	a.boot = time.Now().Unix()
	return a
}

// ---------------- detect: global on/off toggle ----------------

// detectionMtx guards the global record-detections switch.
var detectionMtx sync.Mutex
var detectionOn = true

// detectionEnabled reports whether new detections should be recorded.
func detectionEnabled() bool {
	detectionMtx.Lock()
	v := detectionOn
	detectionMtx.Unlock()
	return v
}

// setDetectionEnabled flips the global detection switch.
func setDetectionEnabled(b bool) {
	detectionMtx.Lock()
	detectionOn = b
	detectionMtx.Unlock()
	mqttPublishState()
}

// ---------------- recordings: enable toggle --------------------

// recordingMtx guards the global clip-recording switch (separate from detection).
var recordingMtx sync.Mutex
var recordingOn = true

func recordingEnabled() bool {
	recordingMtx.Lock()
	v := recordingOn
	recordingMtx.Unlock()
	return v
}

func setRecordingEnabled(b bool) {
	recordingMtx.Lock()
	recordingOn = b
	recordingMtx.Unlock()
	mqttPublishState()
}

// ---------------- feature toggles: S3 / snapshots / pushover ----
// Runtime switches shadowing the YAML defaults (same pattern as
// detection/recording above). Initialized from config in main.go and
// flippable via API, header buttons, or MQTT set topics.

var s3Mtx sync.Mutex
var s3On = false

func s3UploadEnabled() bool {
	s3Mtx.Lock()
	v := s3On
	s3Mtx.Unlock()
	return v
}

func setS3UploadEnabled(b bool) {
	s3Mtx.Lock()
	s3On = b
	s3Mtx.Unlock()
	mqttPublishState()
}

var snapMtx sync.Mutex
var snapOn = true

func snapshotsEnabled() bool {
	snapMtx.Lock()
	v := snapOn
	snapMtx.Unlock()
	return v
}

func setSnapshotsEnabled(b bool) {
	snapMtx.Lock()
	snapOn = b
	snapMtx.Unlock()
	mqttPublishState()
}

var pushMtx sync.Mutex
var pushOn = false

func pushoverEnabled() bool {
	pushMtx.Lock()
	v := pushOn
	pushMtx.Unlock()
	return v
}

func setPushoverEnabled(b bool) {
	pushMtx.Lock()
	pushOn = b
	pushMtx.Unlock()
	mqttPublishState()
}

// ---------------- recordings: grouping (post_detection window) ---

type activeRec struct {
	id      string
	firstAt int64
	lastAt  int64
	endAt   int64
	// detType is the primary (most specific) tag; detTypes is the full merged
	// comma-separated list. They live on the reservation so tags that arrive
	// while collectDetection is still capturing snapshots are not lost before
	// the DB row exists.
	detType  string
	detTypes string
	// established is false while the reserving collectDetection is still
	// running (its DB row may not exist yet) and true once inserted.
	// Only established slots get ghost-checked against the DB.
	established bool
}

var activeRecMtx sync.Mutex
var activeRecMap = map[string]*activeRec{}

// groupMtx serializes the extend-or-reserve decision in handleRawOnvifEvent.
// The slow collectDetection runs OUTSIDE this lock; the reservation made
// inside it is what keeps concurrent events in the same group.
// Lock order wherever nested: groupMtx -> activeRecMtx (never the reverse).
var groupMtx sync.Mutex

// ghostCheckMtx throttles ghost-row verification to one DB lookup per second
// per camera (event bursts would otherwise query per event).
var ghostCheckMtx sync.Mutex
var ghostCheckLast = map[string]int64{}

// ghostCheckDue reports whether a ghost verification is due for camID.
// Callers hold groupMtx; the throttle map has its own lock.
func ghostCheckDue(camID string, now int64) bool {
	ghostCheckMtx.Lock()
	defer ghostCheckMtx.Unlock()
	if last, ok := ghostCheckLast[camID]; ok && now-last < 1 {
		return false
	}
	ghostCheckLast[camID] = now
	return true
}

func getActiveRec(camID string) *activeRec {
	activeRecMtx.Lock()
	defer activeRecMtx.Unlock()
	if r, ok := activeRecMap[camID]; ok {
		// copy to avoid race
		c := *r
		return &c
	}
	return nil
}

func setActiveRec(camID string, rec *activeRec) {
	activeRecMtx.Lock()
	activeRecMap[camID] = rec
	activeRecMtx.Unlock()
}

// addActiveRecType merges dtype into the reservation's tag list and promotes
// its primary type when dtype is more specific. Returns the resulting primary
// tag and whether it changed to a more specific one. Callers hold groupMtx.
func addActiveRecType(camID string, id string, dtype string) (primary string, promoted bool) {
	activeRecMtx.Lock()
	defer activeRecMtx.Unlock()
	var r = activeRecMap[camID]
	if r == nil || r.id != id {
		return "", false
	}
	var before = detTypeRank(r.detType)
	r.detTypes = mergeDetTypeList(r.detTypes, dtype)
	r.detType = primaryDetType(parseDetTypes(r.detTypes), r.detType)
	return r.detType, detTypeRank(r.detType) > before
}

func extendActiveRec(camID string, now int64, postSec int64) (string, bool) {
	activeRecMtx.Lock()
	defer activeRecMtx.Unlock()
	var r = activeRecMap[camID]
	if r == nil {
		return "", false
	}
	if now >= r.endAt {
		// already expired
		return "", false
	}
	r.lastAt = now
	r.endAt = now + postSec
	return r.id, true
}

func clearActiveRec(camID string, id string) {
	activeRecMtx.Lock()
	defer activeRecMtx.Unlock()
	if r, ok := activeRecMap[camID]; ok && r.id == id {
		delete(activeRecMap, camID)
	}
}

// confirmActiveRec marks the reserved slot established once its DB row is
// inserted, keeping the latest end time: extensions may have landed while the
// (slow) collectDetection was running and must not be clobbered.
func confirmActiveRec(camID string, id string, endAt int64) {
	activeRecMtx.Lock()
	defer activeRecMtx.Unlock()
	if r, ok := activeRecMap[camID]; ok && r.id == id {
		if r.endAt > endAt {
			endAt = r.endAt
		}
		activeRecMap[camID] = &activeRec{id: r.id, firstAt: r.firstAt, lastAt: time.Now().Unix(), endAt: endAt, detType: r.detType, detTypes: r.detTypes, established: true}
		return
	}
	if _, ok := activeRecMap[camID]; ok {
		// A different recording already owns this camera's grouping slot (a
		// manual test detection running while a real one is active): hijacking
		// it would stop the real clip's end time from being extended.
		return
	}
	activeRecMap[camID] = &activeRec{id: id, firstAt: endAt, lastAt: endAt, endAt: endAt, established: true}
}

// clearAllActiveRecs drops every grouping slot (used by delete-all so later
// events can't extend groups whose rows are gone).
func clearAllActiveRecs() {
	activeRecMtx.Lock()
	defer activeRecMtx.Unlock()
	activeRecMap = map[string]*activeRec{}
}

// handleRawOnvifEvent records the raw ONVIF event and decides whether to
// start a new recording or extend the current one.
// Returns true if a new recording was started.
func handleRawOnvifEvent(app *App, camID string, camName string, dtype string, src string) bool {
	return handleRawDetectionEvent(app, camID, camName, dtype, src, "")
}

// handleRawDetectionEvent is the shared ingestion path for ONVIF webhook events
// (bbox = "") and RTSP SEI boxes (bbox = JSON array from the camera's stream).
func handleRawDetectionEvent(app *App, camID string, camName string, dtype string, src string, bbox string) bool {
	var now = time.Now().Unix()
	if !detectionEnabled() {
		var ev0 = OnvifEvent{id: newID(), cameraID: camID, camName: camName, detType: dtype, createdAt: now, detectionID: "", bbox: bbox}
		if err := dbInsertOnvifEvent(app.db, &ev0); err != nil {
			logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
		}
		logIt(LDEBUG, "onvif raw event (detection disabled)", map[string]string{"camera": camName, "type": dtype, "id": ev0.id})
		mqttPublishEvent(camID, camName, dtype, "", ev0.id, false)
		return false
	}
	var postSec = int64(app.cfg.Record.PostDetection)
	// Extend-or-reserve, atomically. The reservation happens BEFORE the slow
	// collectDetection (snapshot capture can take seconds): events arriving
	// during capture extend this slot instead of forking a second group.
	groupMtx.Lock()
	var extID string
	var extended bool
	if id, ok := extendActiveRec(camID, now, postSec); ok {
		// A slot whose row is gone is a ghost (delete during an active
		// recording): drop it and start fresh. Reserved-but-uninserted
		// slots are trusted — their row is still being written.
		// Throttled to one DB check per second per camera: bursts of 10-20
		// events/s must not each hit the DB for the same slot.
		var ghost bool
		if cur := getActiveRec(camID); cur != nil && cur.id == id && cur.established {
			if ghostCheckDue(camID, now) {
				if row, derr := getDetection(app.db, id); derr != nil || row == nil {
					ghost = true
				}
			}
		}
		if ghost {
			clearActiveRec(camID, id)
		} else {
			extID, extended = id, true
		}
	}
	var detID string
	if !extended {
		detID = newID()
		setActiveRec(camID, &activeRec{id: detID, firstAt: now, lastAt: now, endAt: now + postSec, detType: normalizeDetType(dtype), detTypes: normalizeDetType(dtype)})
	} else {
		// Merge the tag into the reservation even while the DB row is still
		// being written by the reserving collectDetection.
		addActiveRecType(camID, extID, dtype)
	}
	groupMtx.Unlock()
	if extended {
		var ev = OnvifEvent{id: newID(), cameraID: camID, camName: camName, detType: dtype, createdAt: now, detectionID: extID, bbox: bbox}
		if err := dbInsertOnvifEvent(app.db, &ev); err != nil {
			logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
		}
		_, err := app.db.db.Exec("UPDATE detections SET ended_at=? WHERE id=?", now+postSec, extID)
		if err != nil {
			logIt(LWARN, "db extend failed", map[string]string{"error": err.Error()})
		}
		// Merge the new boxes into the group's stored boxes for the overlay.
		if bbox != "" {
			if err := dbMergeBBox(app.db, extID, bbox); err != nil {
				logIt(LWARN, "db bbox merge failed", map[string]string{"error": err.Error()})
			}
		}
		// Merge the tag into the DB row (no-op while the row does not exist
		// yet: the reserving collectDetection writes the in-memory slot tags).
		// A more specific tag arriving after the clip was inserted triggers an
		// extra notification/republish so subscribers see the tag upgrade.
		if found, promoted, terr := dbMergeDetectionType(app.db, extID, dtype); terr != nil {
			logIt(LWARN, "db tag merge failed", map[string]string{"error": terr.Error()})
		} else if found && promoted {
			if det, derr := getDetection(app.db, extID); derr == nil && det != nil {
				mqttPublishDetection(det)
				broadcastDetection(camID, camName, dtype, bbox)
				go pushNotifyDetection(app, dtype, "Detection: "+camName, dtype+" detected on "+camName, det, detSnapSetByID(extID))
				logIt(LINFO, "detection tag promoted", map[string]string{"camera": camName, "id": extID, "type": dtype})
			}
		}
		logIt(LDEBUG, "onvif raw event (grouped)", map[string]string{"camera": camName, "type": dtype, "id": ev.id, "group": extID})
		logIt(LINFO, "recording extended", map[string]string{"camera": camName, "id": extID, "new_end": strconv.FormatInt(now+postSec, 10)})
		mqttPublishEvent(camID, camName, dtype, extID, ev.id, false)
		return false
	}
	// 3) fresh recording with the reserved id (collectDetection merges any
	// extensions that landed during capture into the final end time)
	var det, err = collectDetectionWithID(app, camID, camName, src, dtype, detID, bbox)
	if err != nil || det == nil {
		// Don't leave a phantom slot that would swallow future events.
		clearActiveRec(camID, detID)
		var ev = OnvifEvent{id: newID(), cameraID: camID, camName: camName, detType: dtype, createdAt: now, detectionID: "", bbox: bbox}
		if err := dbInsertOnvifEvent(app.db, &ev); err != nil {
			logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
		}
		mqttPublishEvent(camID, camName, dtype, "", ev.id, false)
		return false
	}
	var ev2 = OnvifEvent{id: newID(), cameraID: camID, camName: camName, detType: dtype, createdAt: now, detectionID: det.id, bbox: bbox}
	if err := dbInsertOnvifEvent(app.db, &ev2); err != nil {
		logIt(LWARN, "db event insert failed", map[string]string{"error": err.Error()})
	}
	logIt(LDEBUG, "onvif raw event (new group)", map[string]string{"camera": camName, "type": dtype, "id": ev2.id, "group": det.id})
	mqttPublishEvent(camID, camName, dtype, det.id, ev2.id, true)
	// Slot already reserved above (and re-registered by collectDetection with
	// the merged end time); nothing left to do.
	return true
}
