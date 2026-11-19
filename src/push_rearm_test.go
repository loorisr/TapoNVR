package main

import "testing"

func resetPushRearm() {
	pushRearmMtx.Lock()
	pushRearmMap = map[string]*pushRearmState{}
	pushRearmMtx.Unlock()
}

// A stationary object keeps emitting events with gaps shorter than rearm_s:
// only the first detection notifies, even across new detection groups.
func TestPushRearmSuppressesStationaryObject(t *testing.T) {
	resetPushRearm()
	const rearm = 60
	pushRearmTouch("cam1", "vehicle", 1000, rearm)
	if !pushRearmTake("cam1", "vehicle", 1000, rearm) {
		t.Fatal("first detection must notify")
	}
	for ts := int64(1030); ts <= 3000; ts += 45 { // gaps 45s < 60s
		pushRearmTouch("cam1", "vehicle", ts, rearm)
		if pushRearmTake("cam1", "vehicle", ts, rearm) {
			t.Fatalf("object still present at %d: must not notify", ts)
		}
	}
}

// After rearm_s without any event the key re-arms and the next arrival notifies.
func TestPushRearmAfterAbsence(t *testing.T) {
	resetPushRearm()
	const rearm = 60
	pushRearmTouch("cam1", "person", 1000, rearm)
	pushRearmTake("cam1", "person", 1000, rearm)
	pushRearmTouch("cam1", "person", 1050, rearm)
	if pushRearmTake("cam1", "person", 1050, rearm) {
		t.Fatal("must stay disarmed within rearm_s")
	}
	pushRearmTouch("cam1", "person", 1110, rearm) // 60s gap
	if !pushRearmTake("cam1", "person", 1110, rearm) {
		t.Fatal("must re-arm after rearm_s without events")
	}
}

// Keys are per camera and per type: a parked car must not mute a person
// on the same camera nor a car on another camera.
func TestPushRearmIndependentKeys(t *testing.T) {
	resetPushRearm()
	const rearm = 60
	pushRearmTouch("cam1", "vehicle", 1000, rearm)
	pushRearmTake("cam1", "vehicle", 1000, rearm)
	pushRearmTouch("cam1", "person", 1010, rearm)
	if !pushRearmTake("cam1", "person", 1010, rearm) {
		t.Fatal("other type on same camera must notify")
	}
	pushRearmTouch("cam2", "vehicle", 1010, rearm)
	if !pushRearmTake("cam2", "vehicle", 1010, rearm) {
		t.Fatal("same type on other camera must notify")
	}
}

// rearm_s = 0 keeps the legacy behavior; a failed send gives the key back.
func TestPushRearmDisabledAndRestore(t *testing.T) {
	resetPushRearm()
	for i := 0; i < 3; i++ {
		pushRearmTouch("cam1", "motion", int64(1000+i), 0)
		if !pushRearmTake("cam1", "motion", int64(1000+i), 0) {
			t.Fatal("rearm_s=0 must never suppress")
		}
	}
	pushRearmTouch("cam1", "motion", 2000, 60)
	pushRearmTake("cam1", "motion", 2000, 60)
	pushRearmRestore(pushRearmKey("cam1", "motion"))
	pushRearmTouch("cam1", "motion", 2010, 60)
	if !pushRearmTake("cam1", "motion", 2010, 60) {
		t.Fatal("restored key must notify")
	}
}

// A throttled alert (min_interval_s) must not consume the re-arm slot.
func TestPushRearmNotConsumedByThrottle(t *testing.T) {
	withToggles(t, func() {
		resetPushRearm()
		setPushoverEnabled(true)
		app := &App{cfg: &Config{}}
		app.cfg.Alerts.Pushover.Enabled = true
		app.cfg.Alerts.Pushover.AppToken = "a"
		app.cfg.Alerts.Pushover.UserKey = "u"
		app.cfg.Alerts.Pushover.MinIntervalS = 3600
		app.cfg.Alerts.Pushover.RearmS = 60
		pushRateMtx.Lock()
		lastPushUnix = 1 << 40 // far future: always throttled
		pushRateMtx.Unlock()
		defer func() { pushRateMtx.Lock(); lastPushUnix = 0; pushRateMtx.Unlock() }()
		if tk := pushPrepare(app, "cam1", "person", "t"); tk.ok {
			t.Fatal("expected throttled")
		}
		pushRearmMtx.Lock()
		st := pushRearmMap[pushRearmKey("cam1", "person")]
		pushRearmMtx.Unlock()
		if st != nil && !st.armed {
			t.Fatal("throttled alert disarmed the key")
		}
	})
}
