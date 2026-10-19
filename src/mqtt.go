package main

import (
	"crypto/tls"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------- mqtt: publish events + control ----------------

// mqttApp holds the running App for MQTT callbacks.
var mqttApp *App
var mqttClient mqtt.Client
var mqttMtx sync.Mutex

// topicSafeName turns a user-chosen camera name into a safe MQTT topic
// segment: "/", "+", "#" and whitespace are topic separators or wildcards.
func topicSafeName(name string) string {
	var out = name
	for _, r := range []struct{ from, to string }{
		{"/", "_"}, {" ", "_"}, {"+", "_"}, {"#", "_"}, {"\t", "_"},
	} {
		out = strings.ReplaceAll(out, r.from, r.to)
	}
	return out
}

func mqttTopic(prefix, suffix string) string {
	p := strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	s := strings.TrimPrefix(strings.TrimSpace(suffix), "/")
	if p == "" {
		p = "taponvr"
	}
	if s == "" {
		return p
	}
	return p + "/" + s
}

func mqttBrokerURL(cfg *MqttCfg) string {
	if cfg.Broker == "" {
		return ""
	}
	b := strings.TrimSpace(cfg.Broker)
	if strings.Contains(b, "://") {
		return b
	}
	// broker may be "host" or "host:port"
	if strings.Contains(b, ":") {
		return "tcp://" + b
	}
	port := cfg.Port
	if port == 0 {
		port = 1883
	}
	return fmt.Sprintf("tcp://%s:%d", b, port)
}

func parseMqttBool(payload string) (bool, bool) {
	s := strings.ToLower(strings.TrimSpace(payload))
	switch s {
	case "on", "true", "1", "enable", "enabled", "yes":
		return true, true
	case "off", "false", "0", "disable", "disabled", "no":
		return false, true
	default:
		return false, false
	}
}

// mqttPubJob is one queued publish-ack wait for the bounded dispatcher.
type mqttPubJob struct {
	token mqtt.Token
	topic string
}

// mqttPubQueue bounds publish-ack waits: under detection bursts the queue
// absorbs spikes and extra publishes are dropped (counted) instead of
// spawning an unbounded goroutine per publish.
var mqttPubQueue = make(chan mqttPubJob, 1024)
var mqttPubOnce sync.Once
var mqttPubDropped int64

// mqttDone is closed by stopMqtt to end the background loops (connect retry,
// publish dispatcher) instead of leaking them for the life of the process.
var mqttDone = make(chan struct{})
var mqttStopOnce sync.Once

// mqttIsStopped reports whether shutdown has begun (publishes become no-ops).
var mqttStopped bool

func mqttPubDispatch() {
	for {
		select {
		case job := <-mqttPubQueue:
			if !job.token.WaitTimeout(2 * time.Second) {
				logIt(LWARN, "mqtt publish timeout", map[string]string{"topic": job.topic})
			} else if job.token.Error() != nil {
				logIt(LWARN, "mqtt publish failed", map[string]string{"topic": job.topic, "error": job.token.Error().Error()})
			}
		case <-mqttDone:
			return
		}
	}
}

func mqttPublish(topic string, payload string, retained bool) {
	mqttMtx.Lock()
	c := mqttClient
	stopped := mqttStopped
	mqttMtx.Unlock()
	if c == nil || stopped || !c.IsConnected() {
		return
	}
	mqttPubOnce.Do(func() { go mqttPubDispatch() })
	token := c.Publish(topic, 0, retained, payload)
	// don't block server; bounded queue, drop on overflow
	select {
	case mqttPubQueue <- mqttPubJob{token: token, topic: topic}:
	default:
		mqttMtx.Lock()
		mqttPubDropped++
		var n = mqttPubDropped
		mqttMtx.Unlock()
		logIt(LWARN, "mqtt publish queue full, dropped", map[string]string{"topic": topic, "dropped_total": strconv.FormatInt(n, 10)})
	}
}

// mqttPublishSync publishes and waits for the ack (or a short timeout). Used on
// shutdown, where the queued "offline" would otherwise race the disconnect and
// never reach the wire.
func mqttPublishSync(c mqtt.Client, topic string, payload string, retained bool) {
	if c == nil {
		return
	}
	token := c.Publish(topic, 1, retained, payload)
	if !token.WaitTimeout(2 * time.Second) {
		logIt(LWARN, "mqtt publish timeout", map[string]string{"topic": topic})
	} else if token.Error() != nil {
		logIt(LWARN, "mqtt publish failed", map[string]string{"topic": topic, "error": token.Error().Error()})
	}
}

// mqttPublishState publishes retained detection/recording states.
func mqttPublishState() {
	mqttMtx.Lock()
	c := mqttClient
	app := mqttApp
	mqttMtx.Unlock()
	if c == nil || !c.IsConnected() || app == nil {
		return
	}
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	var detState = "OFF"
	if detectionEnabled() {
		detState = "ON"
	}
	var recState = "OFF"
	if recordingEnabled() {
		recState = "ON"
	}
	var s3State = "OFF"
	if s3UploadEnabled() {
		s3State = "ON"
	}
	var snapState = "OFF"
	if snapshotsEnabled() {
		snapState = "ON"
	}
	var pushState = "OFF"
	if pushoverEnabled() {
		pushState = "ON"
	}
	var visionState = "OFF"
	if visionEnabled() {
		visionState = "ON"
	}
	mqttPublish(mqttTopic(prefix, "detection/state"), detState, true)
	mqttPublish(mqttTopic(prefix, "recording/state"), recState, true)
	mqttPublish(mqttTopic(prefix, "s3/state"), s3State, true)
	mqttPublish(mqttTopic(prefix, "snapshots/state"), snapState, true)
	mqttPublish(mqttTopic(prefix, "pushover/state"), pushState, true)
	mqttPublish(mqttTopic(prefix, "vision/state"), visionState, true)
	mqttPublish(mqttTopic(prefix, "status"), "online", true)
}

// mqttBaseURL resolves the public origin for absolute media links.
// Priority: mqtt.base_url, else origin of server.onvif_notify_url, else "".
func mqttBaseURL() string {
	mqttMtx.Lock()
	app := mqttApp
	mqttMtx.Unlock()
	if app == nil {
		return ""
	}
	if b := strings.TrimSpace(app.cfg.Mqtt.BaseURL); b != "" {
		return strings.TrimSuffix(b, "/")
	}
	if n := strings.TrimSpace(app.cfg.Server.OnvifNotifyURL); n != "" {
		// strip path, keep scheme://host[:port]
		if i := strings.Index(n, "://"); i != -1 {
			if j := strings.Index(n[i+3:], "/"); j != -1 {
				return n[:i+3+j]
			}
			return n
		}
	}
	return ""
}

func mqttAbs(base, path string) string {
	if base == "" || path == "" {
		return ""
	}
	return base + path
}

// mqttPublishEvent publishes a detection event (new recording or extension).
// payload is JSON; published to both generic and per-camera topics.
// It includes snapshot/clip links so automations (e.g. Home Assistant) can
// fetch media directly: relative snapshot_url/clip_url plus absolute
// snapshot_absolute_url/clip_absolute_url when a base URL is configured.
func mqttPublishEvent(camID string, camName string, dtype string, detectionID string, rawID string, isNew bool) {
	mqttMtx.Lock()
	c := mqttClient
	app := mqttApp
	mqttMtx.Unlock()
	if c == nil || !c.IsConnected() || app == nil {
		return
	}
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	var now = time.Now().Unix()
	var snapRel, clipRel = "", ""
	var bboxJSON = ""
	if detectionID != "" {
		// Only advertise snapshot/clip links when the files really exist
		// (buffer-mode clips assemble asynchronously; snapshots may be off).
		if det, derr := getDetection(app.db, detectionID); derr == nil && det != nil {
			if det.snapshotPath != "" {
				snapRel = "/api/detections/" + detectionID + "/snapshot"
			}
			clipRel = detectionClipURL(det)
			bboxJSON = det.bboxJSON
		}
	}
	var base = mqttBaseURL()
	var payload = "{" + kv("camera_id", camID) + "," + kv("camera_name", camName) + "," + kv("type", dtype) + "," + kv("detection_id", detectionID) + "," + kv("event_id", rawID) + "," + "\"created_at\":" + fmt.Sprintf("%d", now) + "," + "\"is_new\":" + boolJSON(isNew) +
		"," + kv("snapshot_url", snapRel) + "," + kv("clip_url", clipRel) + "," + kv("snapshot_absolute_url", mqttAbs(base, snapRel)) + "," + kv("clip_absolute_url", mqttAbs(base, clipRel))
	if clean, ok := sanitizeBboxJSON(bboxJSON); ok {
		payload += "," + kvRaw("bbox", clean)
	}
	payload += "}"
	mqttPublish(mqttTopic(prefix, "event"), payload, false)
	if camName != "" {
		mqttPublish(mqttTopic(prefix, topicSafeName(camName)+"/event"), payload, false)
	}
	mqttPublish(mqttTopic(prefix, camID+"/event"), payload, false)
	logIt(LDEBUG, "mqtt event published", map[string]string{"camera": camName, "type": dtype, "detection_id": detectionID})
}

// mqttDetectionPayload wraps detJSON (relative snapshot/clip/snapshots URLs)
// with absolute variants so subscribers get directly usable links.
func mqttDetectionPayload(d *Detection) string {
	var base = mqttBaseURL()
	var rel = detJSON(d)
	var visionExtra = ""
	if d != nil {
		visionExtra = `,"vision_count":` + strconv.Itoa(d.visionCount) +
			`,"vision_top":` + jsonString(d.visionTop) +
			`,"vision_error":` + jsonString(d.visionError)
		if len(rel) > 0 && rel[len(rel)-1] == '}' {
			rel = rel[:len(rel)-1] + visionExtra + "}"
		}
	}
	if base == "" || d == nil {
		return rel
	}
	var snapRel string
	if d.snapshotPath != "" {
		snapRel = "/api/detections/" + d.id + "/snapshot"
	}
	var clipRel = detectionClipURL(d)
	var extra = "," + kv("snapshot_absolute_url", mqttAbs(base, snapRel)) + "," + kv("clip_absolute_url", mqttAbs(base, clipRel))
	// absolute list for all snapshots (primary + extras)
	var snaps []string
	if snapRel != "" {
		snaps = append(snaps, snapRel)
	}
	if d.extraSnap != "" {
		var extras = strings.Split(d.extraSnap, ",")
		for k := range extras {
			if extras[k] == "" {
				continue
			}
			snaps = append(snaps, "/api/detections/"+d.id+"/snapshot/"+strconv.Itoa(k+1))
		}
	}
	var quoted []string
	for _, u := range snaps {
		quoted = append(quoted, jsonString(mqttAbs(base, u)))
	}
	extra += `,"snapshots_absolute":[` + strings.Join(quoted, ",") + `]`
	if len(rel) > 0 && rel[len(rel)-1] == '}' {
		return rel[:len(rel)-1] + extra + "}"
	}
	return rel
}

// mqttPublishDetection publishes a full detection record (after DB insert).
func mqttPublishDetection(d *Detection) {
	if d == nil {
		return
	}
	mqttMtx.Lock()
	c := mqttClient
	app := mqttApp
	mqttMtx.Unlock()
	if c == nil || !c.IsConnected() || app == nil {
		return
	}
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	var payload = mqttDetectionPayload(d)
	mqttPublish(mqttTopic(prefix, "detection"), payload, false)
	mqttPublish(mqttTopic(prefix, d.cameraID+"/detection"), payload, false)
	mqttPublish(mqttTopic(prefix, topicSafeName(d.camName)+"/detection"), payload, false)
	logIt(LDEBUG, "mqtt detection published", map[string]string{"camera": d.camName, "id": d.id})
}

func mqttOnConnect(c mqtt.Client) {
	mqttMtx.Lock()
	app := mqttApp
	mqttMtx.Unlock()
	if app == nil {
		return
	}
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	logIt(LINFO, "mqtt connected", map[string]string{"broker": mqttBrokerURL(&app.cfg.Mqtt), "client_id": app.cfg.Mqtt.ClientID})
	// subscribe to control topics
	topics := map[string]byte{
		mqttTopic(prefix, "detection/set"):       1,
		mqttTopic(prefix, "recording/set"):       1,
		mqttTopic(prefix, "detection/state/set"): 1,
		mqttTopic(prefix, "recording/state/set"): 1,
		mqttTopic(prefix, "s3/set"):              1,
		mqttTopic(prefix, "s3/state/set"):        1,
		mqttTopic(prefix, "snapshots/set"):       1,
		mqttTopic(prefix, "snapshots/state/set"): 1,
		mqttTopic(prefix, "pushover/set"):        1,
		mqttTopic(prefix, "pushover/state/set"):  1,
		mqttTopic(prefix, "vision/set"):          1,
		mqttTopic(prefix, "vision/state/set"):    1,
	}
	for t, qos := range topics {
		token := c.Subscribe(t, qos, mqttMessageHandler)
		if !token.WaitTimeout(5 * time.Second) {
			logIt(LWARN, "mqtt subscribe timeout", map[string]string{"topic": t})
		} else if token.Error() != nil {
			logIt(LWARN, "mqtt subscribe failed", map[string]string{"topic": t, "error": token.Error().Error()})
		} else {
			logIt(LINFO, "mqtt subscribed", map[string]string{"topic": t})
		}
	}
	mqttPublishState()
	// Home Assistant discovery (optional, retained)
	mqttPublishHassDiscovery()
}

func mqttOnConnectionLost(c mqtt.Client, err error) {
	logIt(LWARN, "mqtt connection lost", map[string]string{"error": err.Error()})
}

func mqttMessageHandler(c mqtt.Client, msg mqtt.Message) {
	mqttMtx.Lock()
	app := mqttApp
	mqttMtx.Unlock()
	if app == nil {
		return
	}
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	topic := msg.Topic()
	payload := string(msg.Payload())
	logIt(LDEBUG, "mqtt message received", map[string]string{"topic": topic, "payload": payload})
	val, ok := parseMqttBool(payload)
	if !ok {
		logIt(LWARN, "mqtt bad payload", map[string]string{"topic": topic, "payload": payload})
		return
	}
	// Match on the sub-topic after the prefix: matching the full topic would
	// misroute when a custom TopicPrefix itself contains a keyword (e.g. "s3").
	suffix := strings.TrimPrefix(topic, mqttTopic(prefix, ""))
	suffix = strings.TrimPrefix(suffix, "/")
	switch suffix {
	case "pushover/set", "pushover/state/set":
		setPushoverEnabled(val)
		logIt(LINFO, "mqtt pushover set", map[string]string{"enabled": boolJSON(val), "topic": topic})
	case "vision/set", "vision/state/set":
		setVisionEnabled(val)
		logIt(LINFO, "mqtt vision set", map[string]string{"enabled": boolJSON(val), "topic": topic})
	case "snapshots/set", "snapshots/state/set":
		setSnapshotsEnabled(val)
		logIt(LINFO, "mqtt snapshots set", map[string]string{"enabled": boolJSON(val), "topic": topic})
	case "detection/set", "detection/state/set":
		setDetectionEnabled(val)
		logIt(LINFO, "mqtt detection set", map[string]string{"enabled": boolJSON(val), "topic": topic})
	case "recording/set", "recording/state/set":
		setRecordingEnabled(val)
		logIt(LINFO, "mqtt recording set", map[string]string{"enabled": boolJSON(val), "topic": topic})
	case "s3/set", "s3/state/set":
		setS3UploadEnabled(val)
		logIt(LINFO, "mqtt s3 set", map[string]string{"enabled": boolJSON(val), "topic": topic})
	default:
		logIt(LWARN, "mqtt unknown control topic", map[string]string{"topic": topic})
	}
}

func mqttPublishHassDiscovery() {
	// Publish simple MQTT discovery for Home Assistant if prefix is set.
	// This is best-effort and non-fatal.
	mqttMtx.Lock()
	c := mqttClient
	app := mqttApp
	mqttMtx.Unlock()
	if c == nil || !c.IsConnected() || app == nil {
		return
	}
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	// Switch entities for detection/recording/s3/snapshots/pushover
	for _, ent := range []struct{ id, name string }{
		{"detection", "Detection"},
		{"recording", "Recording"},
		{"s3", "S3 Upload"},
		{"snapshots", "Snapshots"},
		{"pushover", "Push Notification"},
		{"vision", "Vision Analysis"},
	} {
		discoveryTopic := fmt.Sprintf("homeassistant/switch/%s_%s/config", prefix, ent.id)
		uniqueID := fmt.Sprintf("%s_%s", prefix, ent.id)
		stateTopic := mqttTopic(prefix, ent.id+"/state")
		cmdTopic := mqttTopic(prefix, ent.id+"/set")
		// Built with jsonString: a TopicPrefix containing a quote or backslash
		// must not produce malformed discovery JSON.
		payload := `{"name":` + jsonString(prefix+" "+ent.name) +
			`,"unique_id":` + jsonString(uniqueID) +
			`,"state_topic":` + jsonString(stateTopic) +
			`,"command_topic":` + jsonString(cmdTopic) +
			`,"payload_on":"ON","payload_off":"OFF"` +
			`,"device":{"identifiers":[` + jsonString(prefix) + `],"name":"TapoNVR","manufacturer":"TapoNVR"}}`
		mqttPublish(discoveryTopic, payload, true)
	}
}

func initMqtt(app *App) {
	if !app.cfg.Mqtt.Enabled {
		return
	}
	// Recreate shutdown state so a second init (tests/restart) works.
	mqttMtx.Lock()
	mqttDone = make(chan struct{})
	mqttStopOnce = sync.Once{}
	mqttStopped = false
	mqttMtx.Unlock()
	brokerURL := mqttBrokerURL(&app.cfg.Mqtt)
	if brokerURL == "" {
		logIt(LWARN, "mqtt enabled but no broker configured", map[string]string{})
		return
	}
	prefix := app.cfg.Mqtt.TopicPrefix
	if prefix == "" {
		prefix = "taponvr"
	}
	clientID := app.cfg.Mqtt.ClientID
	if clientID == "" {
		clientID = "taponvr"
	}
	mqttMtx.Lock()
	mqttApp = app
	mqttMtx.Unlock()
	opts := mqtt.NewClientOptions()
	opts.AddBroker(brokerURL)
	opts.SetClientID(clientID)
	if strings.HasPrefix(strings.ToLower(brokerURL), "tls://") || strings.HasPrefix(strings.ToLower(brokerURL), "ssl://") {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	if app.cfg.Mqtt.Username != "" {
		opts.SetUsername(app.cfg.Mqtt.Username)
		opts.SetPassword(app.cfg.Mqtt.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
	opts.SetCleanSession(true)
	opts.SetOnConnectHandler(mqttOnConnect)
	opts.SetConnectionLostHandler(mqttOnConnectionLost)
	// Last will: status offline
	opts.SetWill(mqttTopic(prefix, "status"), "offline", 1, true)
	client := mqtt.NewClient(opts)
	mqttMtx.Lock()
	mqttClient = client
	mqttMtx.Unlock()
	// Connect async with retry loop (ends on stopMqtt so it cannot leak).
	go func() {
		for {
			if token := client.Connect(); token.WaitTimeout(10 * time.Second) {
				if token.Error() == nil {
					logIt(LINFO, "mqtt connecting", map[string]string{"broker": brokerURL})
					return
				}
				logIt(LWARN, "mqtt connect failed, retrying", map[string]string{"broker": brokerURL, "error": token.Error().Error()})
			} else {
				logIt(LWARN, "mqtt connect timeout, retrying", map[string]string{"broker": brokerURL})
			}
			select {
			case <-mqttDone:
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

// stopMqtt publishes the retained "offline" status and disconnects. Safe to
// call more than once (shutdown runs it from both the signal handler and run()).
func stopMqtt() {
	mqttMtx.Lock()
	c := mqttClient
	app := mqttApp
	mqttStopped = true
	mqttMtx.Unlock()
	// End the background loops (connect retry, publish dispatcher).
	mqttStopOnce.Do(func() { close(mqttDone) })
	if c == nil {
		return
	}
	prefix := "taponvr"
	if app != nil && app.cfg.Mqtt.TopicPrefix != "" {
		prefix = app.cfg.Mqtt.TopicPrefix
	}
	// Publish synchronously: going through the bounded queue would race the
	// disconnect below and the retained status could stay "online" forever.
	if c.IsConnected() {
		mqttPublishSync(c, mqttTopic(prefix, "status"), "offline", true)
	}
	c.Disconnect(500)
}
