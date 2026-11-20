package main

import "testing"

func resetPushRearm() {
	rearmMtx.Lock()
	rearmMap = map[string]*rearmState{}
	rearmMtx.Unlock()
	detGateMtx.Lock()
	detGateMap = map[string]*detGateEntry{}
	detGateMtx.Unlock()
}

// Test shims: the pushover-only consumer as the original tests used it.
func pushRearmTouch(camID, eventType string, now int64, rearmS int) {
	var w rearmWindows
	w[rearmPush] = rearmS
	rearmTouch(camID, eventType, now, w)
}

func pushRearmTake(camID, eventType string, now int64, rearmS int) bool {
	return rearmTake(rearmPush, camID, eventType, now, rearmS)
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
	rearmRestore(rearmPush, rearmKey("cam1", "motion"))
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
		rearmMtx.Lock()
		st := rearmMap[rearmKey("cam1", "person")]
		rearmMtx.Unlock()
		if st != nil && st.disarmed[rearmPush] {
			t.Fatal("throttled alert disarmed the key")
		}
	})
}

// Consumers are independent: each has its own window and its own armed
// state, while presence (last event time) is shared.
func TestRearmConsumersIndependent(t *testing.T) {
	resetPushRearm()
	var w rearmWindows
	w[rearmPush], w[rearmVision], w[rearmMQTT] = 60, 300, 0
	rearmTouch("cam1", "vehicle", 1000, w)
	if !rearmTake(rearmPush, "cam1", "vehicle", 1000, 60) || !rearmTake(rearmVision, "cam1", "vehicle", 1000, 300) {
		t.Fatal("first event must act for every consumer")
	}
	// A vision decision must not disarm pushover and vice versa.
	rearmTouch("cam1", "vehicle", 1010, w)
	if rearmTake(rearmVision, "cam1", "vehicle", 1010, 300) {
		t.Fatal("vision must stay disarmed")
	}
	if !rearmTake(rearmMQTT, "cam1", "vehicle", 1010, 0) {
		t.Fatal("mqtt rearm_s=0 must never suppress")
	}
	// 100s of absence: pushover (60s) re-arms, vision (300s) does not.
	rearmTouch("cam1", "vehicle", 1110, w)
	if !rearmTake(rearmPush, "cam1", "vehicle", 1110, 60) {
		t.Fatal("pushover must re-arm after 60s of absence")
	}
	if rearmTake(rearmVision, "cam1", "vehicle", 1110, 300) {
		t.Fatal("vision must not re-arm before 300s of absence")
	}
	rearmTouch("cam1", "vehicle", 1500, w)
	if !rearmTake(rearmVision, "cam1", "vehicle", 1500, 300) {
		t.Fatal("vision must re-arm after 300s of absence")
	}
}

// A new group for a stationary object mutes vision and MQTT (including the
// group's later republishes) but only when those consumers are active.
func TestRearmGateNewGroup(t *testing.T) {
	withToggles(t, func() {
		resetPushRearm()
		app := &App{cfg: &Config{}}
		app.cfg.Mqtt.Enabled = true
		app.cfg.Mqtt.RearmS = 60
		app.cfg.Vision.RearmS = 60 // vision itself off: must not be muted
		w := rearmWindowsFromConfig(app.cfg)
		rearmTouch("cam1", "person", 1000, w)
		if !rearmGateNewGroup(app, "g1", "cam1", "person", 1000) {
			t.Fatal("first group must publish")
		}
		if detGateMuted("g1", rearmMQTT) {
			t.Fatal("first group muted")
		}
		rearmTouch("cam1", "person", 1040, w)
		if rearmGateNewGroup(app, "g2", "cam1", "person", 1040) {
			t.Fatal("second group of a stationary object must not publish")
		}
		if !detGateMuted("g2", rearmMQTT) {
			t.Fatal("second group must stay muted for republishes")
		}
		if detGateMuted("g2", rearmVision) {
			t.Fatal("inactive vision consumer must not be muted")
		}
		// Tag promotion to an armed type re-opens the group.
		detGateUnmute("g2", rearmMQTT)
		if detGateMuted("g2", rearmMQTT) {
			t.Fatal("unmute failed")
		}
		// Extension events within the presence are suppressed too.
		rearmTouch("cam1", "person", 1050, w)
		if mqttRearmTake(app, "cam1", "person", 1050) {
			t.Fatal("raw event of a stationary object must not publish")
		}
		// MQTT off: never suppressed, slot not consumed.
		app.cfg.Mqtt.Enabled = false
		if !mqttRearmTake(app, "cam1", "person", 1060) {
			t.Fatal("mqtt off must not suppress")
		}
	})
}
