package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------- config: YAML load ------------------------

type AuthCfg struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Disabled explicitly allows open UI when password is empty (for
	// reverse-proxy setups). Empty password without this flag still
	// opens auth for backward compat but logs a warning.
	Disabled bool `yaml:"disabled"`
}

type ServerCfg struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	DataDir       string `yaml:"data_dir"`
	RetentionDays int    `yaml:"retention_days"`
	WebUIDir      string `yaml:"webui_dir"`
	// OnvifNotifyURL is the externally-reachable URL the cameras POST ONVIF
	// notifications to (used for the WS-Base Subscribe detection path). If unset,
	// a private IPv4 on server.port is derived automatically. The webhook path
	// token (see OnvifNotifyToken) is appended automatically in both cases.
	OnvifNotifyURL string `yaml:"onvif_notify_url"`
	// OnvifNotifyToken is the shared secret gating the /onvif/notify webhook
	// (cameras cannot do Basic auth). When empty, a stable token derived from
	// server.auth.password is used; when no password is set either, the webhook
	// stays open (the UI itself is unauthenticated in that case). An explicit
	// server.onvif_notify_url gains the "/<token>" suffix automatically (a
	// manually appended suffix is not duplicated).
	OnvifNotifyToken string `yaml:"onvif_notify_token"`
	// LogLevel controls the server log verbosity: debug, info, warn, error.
	// debug shows per-snapshot and per-file-delete messages.
	LogLevel string  `yaml:"log_level"`
	Auth     AuthCfg `yaml:"auth"`
}

type StreamCfg struct {
	Go2rtcPath string `yaml:"go2rtc_path"`
	Port       int    `yaml:"port"`
	// RtspPort is the go2rtc RTSP re-broadcast port recording pulls from. go2rtc
	// exposes an RTSP server on this port; recording reads rtsp://127.0.0.1:port/camN
	// so it uses the relayed stream rather than a second direct camera connection.
	RtspPort int `yaml:"rtsp_port"`
	// Go2rtcLogLevel sets the go2rtc log verbosity in its generated config
	// (trace, debug, info, warn, error). Defaults to "warn".
	Go2rtcLogLevel string `yaml:"go2rtc_log_level"`
	// IceServers lists WebRTC ICE server URLs for go2rtc (e.g.
	// ["stun:stun.example.com:3478"]). Empty by default: LAN/host candidates
	// suffice locally and no public STUN is contacted (no IP leak, works on
	// isolated LANs). Remote/NAT setups may add STUN/TURN here.
	IceServers []string `yaml:"ice_servers"`
}

type SnapshotsCfg struct {
	Enabled   bool `yaml:"enabled"`
	Count     int  `yaml:"count"`
	IntervalS int  `yaml:"interval_s"`
}

type RecordCfg struct {
	Mode string `yaml:"mode"`
	// Enabled toggles video clip recording (the pre-event buffer + clip
	// assembly). When false, detections still run but clips are not captured.
	Enabled       bool `yaml:"enabled"`
	PreDetection  int  `yaml:"pre_detection"`
	PostDetection int  `yaml:"post_detection"`
	// BufferDir is where the rolling pre-event buffer segments live. It defaults
	// to a RAM-backed directory (/dev/shm on Linux) so the fast-changing buffer
	// is never written to durable flash; only the assembled clip (and snapshots)
	// are written to data_dir on disk. Set to your own path to override.
	BufferDir string       `yaml:"buffer_dir"`
	Snapshots SnapshotsCfg `yaml:"snapshots"`
	// RetentionByType overrides the global retention (server.retention_days)
	// per detection type, in days (e.g. {motion: 3, person: 30}). A type
	// absent from the map, or with a value <= 0, falls back to the global
	// retention. Locked recordings are never pruned by either rule.
	RetentionByType map[string]int `yaml:"retention_by_type"`
}

// recordKnownKeys lists every YAML key accepted under `record:`, and
// snapshotKnownKeys every key under `record.snapshots`. recordKeys/snapshotKeys
// are the same names sorted, for error messages.
var recordKnownKeys = map[string]bool{
	"mode": true, "enabled": true,
	"pre_detection": true, "post_detection": true,
	"pre_detection_time": true, "post_detection_time": true,
	"pre_detection_seconds": true, "post_detection_seconds": true,
	"buffer_seconds": true, "clip_seconds": true,
	"buffer_dir": true, "snapshots": true, "retention_by_type": true,
}
var snapshotKnownKeys = map[string]bool{
	"enabled": true, "count": true, "interval_s": true,
}

// rejectUnknownKeys fails on any key outside the allow-list. dec.KnownFields(true)
// cannot reach a *yaml.Node decoder (it builds a fresh, lenient one), so a typo
// under `record:` would otherwise be silently ignored and the wrong pre/post
// windows used with no error at all.
func rejectUnknownKeys(node *yaml.Node, known map[string]bool, section string) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		var k = node.Content[i].Value
		if !known[k] {
			return fmt.Errorf("unknown key %q in %s (expected one of: %s)", k, section, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
		}
	}
	return nil
}

// UnmarshalYAML handles rename from buffer_seconds/clip_seconds to pre_detection/post_detection
// while keeping backward compatibility for old configs.
func (r *RecordCfg) UnmarshalYAML(node *yaml.Node) error {
	if err := rejectUnknownKeys(node, recordKnownKeys, "record"); err != nil {
		return err
	}
	// auxiliary with all possible keys
	var aux struct {
		Mode          *string        `yaml:"mode"`
		Enabled       *bool          `yaml:"enabled"`
		PreDetection  *int           `yaml:"pre_detection"`
		PostDetection *int           `yaml:"post_detection"`
		PreTime       *int           `yaml:"pre_detection_time"`
		PostTime      *int           `yaml:"post_detection_time"`
		PreSeconds    *int           `yaml:"pre_detection_seconds"`
		PostSeconds   *int           `yaml:"post_detection_seconds"`
		BufferSeconds *int           `yaml:"buffer_seconds"`
		ClipSeconds   *int           `yaml:"clip_seconds"`
		BufferDir     *string        `yaml:"buffer_dir"`
		RetByType     map[string]int `yaml:"retention_by_type"`
	}
	if err := node.Decode(&aux); err != nil {
		return err
	}
	// start from current values (defaults already set by caller)
	if aux.Mode != nil {
		r.Mode = *aux.Mode
	}
	if aux.Enabled != nil {
		r.Enabled = *aux.Enabled
	}
	if aux.BufferDir != nil {
		r.BufferDir = *aux.BufferDir
	}
	if aux.RetByType != nil {
		r.RetentionByType = aux.RetByType
	}
	// Decode the snapshots sub-node INTO the existing value: a wholesale copy of
	// a freshly-decoded struct would reset the count/interval_s defaults to 0
	// whenever the block sets only some of the keys. The sub-node is located
	// explicitly so its own keys can be validated as well.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "snapshots" {
			continue
		}
		var sub = node.Content[i+1]
		if err := rejectUnknownKeys(sub, snapshotKnownKeys, "record.snapshots"); err != nil {
			return err
		}
		if err := sub.Decode(&r.Snapshots); err != nil {
			return err
		}
	}
	// pre_detection priority: pre_detection > pre_detection_time > pre_detection_seconds > buffer_seconds
	if aux.PreDetection != nil {
		r.PreDetection = *aux.PreDetection
	} else if aux.PreTime != nil {
		r.PreDetection = *aux.PreTime
	} else if aux.PreSeconds != nil {
		r.PreDetection = *aux.PreSeconds
	} else if aux.BufferSeconds != nil {
		r.PreDetection = *aux.BufferSeconds
	}
	if aux.PostDetection != nil {
		r.PostDetection = *aux.PostDetection
	} else if aux.PostTime != nil {
		r.PostDetection = *aux.PostTime
	} else if aux.PostSeconds != nil {
		r.PostDetection = *aux.PostSeconds
	} else if aux.ClipSeconds != nil {
		r.PostDetection = *aux.ClipSeconds
	}
	return nil
}

type S3Cfg struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Prefix    string `yaml:"prefix"`
	// StorageClass is the S3 storage class applied to every uploaded object
	// (default ONEZONE_IA).
	StorageClass string `yaml:"storage_class"`
}

type StorageCfg struct {
	Enabled bool  `yaml:"enabled"`
	S3      S3Cfg `yaml:"s3"`
	// MaxDiskPct caps data_dir usage: when exceeded, oldest unlocked
	// detections are pruned until back under the threshold (0 = off).
	MaxDiskPct int `yaml:"max_disk_pct"`
	// MinFreeGB keeps at least this many free gigabytes on data_dir's
	// filesystem (0 = off). Same emergency pruning as MaxDiskPct.
	MinFreeGB float64 `yaml:"min_free_gb"`
}

type PushoverCfg struct {
	Enabled      bool   `yaml:"enabled"`
	AppToken     string `yaml:"app_token"`
	UserKey      string `yaml:"user_key"`
	MinIntervalS int    `yaml:"min_interval_s"`
	// SendSnapshot attaches the detection snapshot to the notification.
	// False sends text-only alerts. Defaults to true so existing configs
	// keep today's behavior.
	SendSnapshot bool `yaml:"send_snapshot"`
	// SnapshotMaxPx caps the long edge of the attached copy (aspect kept).
	// 0 means original size. The stored snapshot is never modified.
	SnapshotMaxPx int `yaml:"snapshot_max_px"`
	// SnapshotQuality is the JPEG quality (1-100) of the attached copy.
	SnapshotQuality int `yaml:"snapshot_quality"`
}

type AlertsCfg struct {
	Pushover PushoverCfg `yaml:"pushover"`
}

type MqttCfg struct {
	Enabled     bool   `yaml:"enabled"`
	Broker      string `yaml:"broker"`
	Port        int    `yaml:"port"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	ClientID    string `yaml:"client_id"`
	TopicPrefix string `yaml:"topic_prefix"`
	// BaseURL is the public origin of the NVR web UI, e.g. http://192.168.1.10:8971.
	// When set, MQTT event/detection payloads include absolute snapshot/clip URLs
	// (snapshot_absolute_url / clip_absolute_url) in addition to the relative paths.
	// When unset, the origin of server.onvif_notify_url is used as fallback.
	BaseURL string `yaml:"base_url"`
}

// VisionCfg configures optional snapshot analysis via an external YOLO
// HTTP API (NVR Vision: POST /v1/detect with JPEG bytes -> JSON detections).
// Disabled by default: zero impact when off (no extra goroutine, no dep).
type VisionCfg struct {
	// Enabled master switch (also flippable at runtime via /api/vision/state).
	Enabled bool `yaml:"enabled"`
	// URL is the base URL of the vision API, e.g. http://127.0.0.1:8080.
	URL string `yaml:"url"`
	// APIKey is sent as X-API-Key when non-empty (open LAN mode when empty).
	APIKey string `yaml:"api_key"`
	// TimeoutS bounds one /v1/detect call (clamped 2..30).
	TimeoutS int `yaml:"timeout_s"`
	// MinConf default ?min_conf= (clamped 0..1).
	MinConf float64 `yaml:"min_conf"`
	// Classes CSV COCO filter, empty = all (e.g. "person,car,bicycle").
	Classes string `yaml:"classes"`
	// Snapshots selects which stills are analyzed: "primary" or "all"
	// (primary + timed extras burst).
	Snapshots string `yaml:"snapshots"`
	// MaxWorkers bounds concurrent /v1/detect calls (clamped 1..8).
	MaxWorkers int `yaml:"max_workers"`
}

type OnvifCfg struct {
	Url      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// DetectionRTSPCfg tunes the RTSP SEI detection source (boxes + type embedded
// in the camera's H.264 stream, see rtsp-detection-boxes.md).
type DetectionRTSPCfg struct {
	// Transport is the RTSP transport used by the SEI reader ("tcp" or "udp").
	Transport string `yaml:"transport"`
	// DebounceS is the minimum delay between two emitted detections of the same
	// type on a camera. SEI markers repeat on nearly every frame (and on idle
	// scenes), so this prevents a raw-event flood.
	DebounceS int `yaml:"debounce_s"`
	// HoldS keeps a detection type "active" for this long after the last box;
	// it ends the clip's post-detection window (like an ONVIF inactive event).
	HoldS int `yaml:"hold_s"`
	// Types is the allow-list of detection types to accept (motion, person,
	// vehicle, pet, other). Empty falls back to motion/person/vehicle/pet.
	Types []string `yaml:"types"`
	// UseGo2rtc pulls the local go2rtc relay instead of opening a second direct
	// camera RTSP connection.
	UseGo2rtc bool `yaml:"use_go2rtc"`
}

// DetectionCfg selects where detections come from: the ONVIF webhook (type
// only) and/or the RTSP SEI stream (type + box).
type DetectionCfg struct {
	// Source is onvif (default), rtsp, or both. Per-camera detection_source
	// overrides it.
	Source string           `yaml:"source"`
	RTSP   DetectionRTSPCfg `yaml:"rtsp"`
}

type CameraCfg struct {
	Name    string   `yaml:"name"`
	Enabled bool     `yaml:"enabled"`
	Onvif   OnvifCfg `yaml:"onvif"`
	RtspURL string   `yaml:"rtsp_url"`
	Ptz     string   `yaml:"ptz"`
	// DetectionSource overrides detection.source for this camera
	// (onvif|rtsp|both|off). Empty inherits the global source.
	DetectionSource string `yaml:"detection_source"`
	// DetectionDebounceS overrides detection.rtsp.debounce_s for this camera.
	// Nil inherits the global value.
	DetectionDebounceS *int `yaml:"detection_debounce_s"`
	// DetectionHoldS overrides detection.rtsp.hold_s for this camera.
	// Nil inherits the global value.
	DetectionHoldS *int `yaml:"detection_hold_s"`
}

// cameraKnownKeys / onvifKnownKeys are the YAML keys accepted under a
// `cameras[]` entry and its `onvif` block. They are validated explicitly
// because CameraCfg.UnmarshalYAML builds its own decoder, bypassing the
// top-level dec.KnownFields(true): a typo like `enabeld: false` would silently
// leave the camera enabled (default true) instead of failing startup.
var cameraKnownKeys = map[string]bool{
	"name": true, "enabled": true, "onvif": true,
	"rtsp_url": true, "ptz": true, "detection_source": true,
	"detection_debounce_s": true, "detection_hold_s": true,
}
var onvifKnownKeys = map[string]bool{"url": true, "username": true, "password": true}

// UnmarshalYAML defaults Enabled to true when the key is absent: a bool
// defaults to false, which silently disabled cameras whose config omitted
// `enabled:`. An explicit `enabled: false` is still honoured.
func (c *CameraCfg) UnmarshalYAML(node *yaml.Node) error {
	if err := rejectUnknownKeys(node, cameraKnownKeys, "cameras[]"); err != nil {
		return err
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "onvif" {
			if err := rejectUnknownKeys(node.Content[i+1], onvifKnownKeys, "cameras[].onvif"); err != nil {
				return err
			}
		}
	}
	type plain CameraCfg
	c.Enabled = true
	return node.Decode((*plain)(c))
}

type Config struct {
	Server    ServerCfg    `yaml:"server"`
	Stream    StreamCfg    `yaml:"stream"`
	Record    RecordCfg    `yaml:"record"`
	Storage   StorageCfg   `yaml:"storage"`
	Alerts    AlertsCfg    `yaml:"alerts"`
	Mqtt      MqttCfg      `yaml:"mqtt"`
	Vision    VisionCfg    `yaml:"vision"`
	Detection DetectionCfg `yaml:"detection"`
	Cameras   []CameraCfg  `yaml:"cameras"`
}

// detectionLabels is the set of detection types the pipeline knows about.
var detectionLabels = map[string]bool{
	"motion": true, "person": true, "vehicle": true, "pet": true, "other": true,
}

// sourceHas reports whether an effective source string enables one channel.
func sourceHas(source, want string) bool {
	return source == want || source == "both"
}

// camDetectionSource resolves the effective detection source for a camera:
// "onvif", "rtsp", "both" or "off".
func camDetectionSource(cfg *Config, cam *CameraCfg) string {
	var s = strings.ToLower(strings.TrimSpace(cfg.Detection.Source))
	if cam != nil {
		if o := strings.ToLower(strings.TrimSpace(cam.DetectionSource)); o != "" {
			s = o
		}
	}
	switch s {
	case "onvif", "rtsp", "both":
		return s
	case "off", "none", "false", "disabled":
		return "off"
	default:
		return "onvif"
	}
}

// camDebounceS resolves the effective RTSP debounce window (seconds) for a
// camera: per-camera override when non-nil, else the global default.
func camDebounceS(cfg *Config, cam *CameraCfg) int {
	if cam != nil && cam.DetectionDebounceS != nil {
		return *cam.DetectionDebounceS
	}
	if cfg != nil {
		return cfg.Detection.RTSP.DebounceS
	}
	return 10
}

// camHoldS resolves the effective RTSP hold window (seconds) for a camera:
// per-camera override when non-nil, else the global default.
func camHoldS(cfg *Config, cam *CameraCfg) int {
	if cam != nil && cam.DetectionHoldS != nil {
		return *cam.DetectionHoldS
	}
	if cfg != nil {
		return cfg.Detection.RTSP.HoldS
	}
	return 2
}

// clampPerCamDetection clamps per-camera detection tuning fields when present
// (debounce 0..3600, hold 1..60). Nil means inherit global (untouched).
func clampPerCamDetection(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Cameras {
		if v := c.Cameras[i].DetectionDebounceS; v != nil {
			if *v < 0 {
				configWarn("config clamped: cameras[].detection_debounce_s < 0, using 0", nil)
				*v = 0
			}
			if *v > 3600 {
				configWarn("config clamped: cameras[].detection_debounce_s > 3600, using 3600", nil)
				*v = 3600
			}
		}
		if v := c.Cameras[i].DetectionHoldS; v != nil {
			if *v < 1 {
				configWarn("config clamped: cameras[].detection_hold_s not in 1..60, using 1", nil)
				*v = 1
			}
			if *v > 60 {
				configWarn("config clamped: cameras[].detection_hold_s not in 1..60, using 60", nil)
				*v = 60
			}
		}
	}
}

func configDefaults(c *Config) {
	c.Server.Host = "[IP_ADDRESS]"
	c.Server.Port = 8971
	c.Server.DataDir = "./data"
	c.Server.RetentionDays = 7
	c.Server.WebUIDir = "webui/dist"
	c.Server.LogLevel = "info"
	c.Server.Auth.Username = "admin"
	c.Server.Auth.Password = "changeme"
	c.Stream.Port = 18884
	c.Stream.RtspPort = 8554
	c.Stream.Go2rtcLogLevel = "warn"
	c.Stream.Go2rtcPath = "/usr/bin/go2rtc"
	c.Record.Mode = "buffer"
	c.Record.Enabled = true
	c.Record.PreDetection = 10
	c.Record.PostDetection = 10
	c.Record.BufferDir = ""
	c.Record.Snapshots.Enabled = true
	c.Record.Snapshots.Count = 1
	c.Record.Snapshots.IntervalS = 2
	c.Storage.Enabled = false
	c.Storage.S3.Endpoint = ""
	c.Storage.S3.Region = "us-east-1"
	c.Storage.S3.Bucket = ""
	c.Storage.S3.AccessKey = ""
	c.Storage.S3.SecretKey = ""
	c.Storage.S3.Prefix = "recordings"
	c.Storage.S3.StorageClass = "ONEZONE_IA"
	c.Storage.MaxDiskPct = 0
	c.Storage.MinFreeGB = 0
	c.Alerts.Pushover.Enabled = false
	c.Alerts.Pushover.AppToken = ""
	c.Alerts.Pushover.UserKey = ""
	c.Alerts.Pushover.MinIntervalS = 60
	c.Alerts.Pushover.SendSnapshot = true
	c.Alerts.Pushover.SnapshotMaxPx = 1280
	c.Alerts.Pushover.SnapshotQuality = 70
	c.Mqtt.Enabled = false
	c.Mqtt.Broker = ""
	c.Mqtt.Port = 1883
	c.Mqtt.Username = ""
	c.Mqtt.Password = ""
	c.Mqtt.ClientID = "taponvr"
	c.Mqtt.TopicPrefix = "taponvr"
	c.Vision.Enabled = false
	c.Vision.URL = "http://127.0.0.1:8080"
	c.Vision.APIKey = ""
	c.Vision.TimeoutS = 10
	c.Vision.MinConf = 0.25
	c.Vision.Classes = ""
	c.Vision.Snapshots = "primary"
	c.Vision.MaxWorkers = 2
	c.Detection.Source = "onvif"
	c.Detection.RTSP.Transport = "tcp"
	c.Detection.RTSP.DebounceS = 10
	c.Detection.RTSP.HoldS = 2
	c.Detection.RTSP.Types = []string{"motion", "person", "vehicle", "pet"}
	c.Detection.RTSP.UseGo2rtc = true
}

// clampGo2rtcLogLevel normalizes a go2rtc log level to its known set
// (trace, debug, info, warn, error), falling back to "warn". The generated
// go2rtc config would otherwise carry a bogus level through verbatim.
func clampGo2rtcLogLevel(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug", "info", "warn", "error":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "warn"
	}
}

// redactNotifyURL masks the webhook secret in a server.onvif_notify_url value
// for logs: the ?token= query value and the trailing path segment (the token
// suffix onvifNotifyURL appends) are replaced with ***.
func redactNotifyURL(u string) string {
	if idx := strings.Index(u, "?token="); idx >= 0 {
		var rest = u[idx+7:]
		if amp := strings.Index(rest, "&"); amp >= 0 {
			return u[:idx+7] + "***" + rest[amp:]
		}
		return u[:idx+7] + "***"
	}
	if idx := strings.LastIndex(u, "/"); idx >= 0 && idx+1 < len(u) {
		return u[:idx+1] + "***"
	}
	return u
}

// validateDataDirs rejects dangerous data/buffer directories. "/" would
// expose (and prune) the whole filesystem, "/dev" is not a writable data
// store, "/tmp" is world-readable and ephemeral, and an empty data_dir leaves
// the DB and detections with nowhere safe to live. An empty buffer_dir is the
// default (a RAM-backed directory is derived at runtime) and stays allowed.
func validateDataDirs(c *Config) {
	var check = func(label, d string, allowEmpty bool) {
		var t = strings.TrimSpace(d)
		if t == "" {
			if !allowEmpty {
				configWarn("config: "+label+" is empty (detections/db have nowhere safe to live)", nil)
			}
			return
		}
		switch filepath.Clean(t) {
		case "/", "/tmp", "/dev":
			configWarn("config: "+label+" must not point at "+filepath.Clean(t)+" (unsafe)", map[string]string{"value": d})
		}
	}
	check("server.data_dir", c.Server.DataDir, false)
	check("record.buffer_dir", c.Record.BufferDir, true)
}

// validateConfig warns about incoherent configs at startup: duplicate camera
// names (they break name-keyed lookups), empty credentials for ONVIF cameras,
// stream/rtsp port collisions, and unusable stream ports / log levels.
func validateConfig(c *Config) {
	clampPerCamDetection(c)
	seen := map[string]int{}
	for i := range c.Cameras {
		n := strings.TrimSpace(c.Cameras[i].Name)
		if n == "" {
			configWarn("config: camera has an empty name", map[string]string{"id": fmt.Sprintf("cam%d", i)})
			continue
		}
		if prev, ok := seen[strings.ToLower(n)]; ok {
			configWarn("config: duplicate camera name (webhook routing uses the stable camID, but names stay ambiguous)", map[string]string{"name": n, "ids": fmt.Sprintf("cam%d,cam%d", prev, i)})
		} else {
			seen[strings.ToLower(n)] = i
		}
		if c.Cameras[i].Enabled && c.Cameras[i].RtspURL == "" {
			configWarn("config: enabled camera has no rtsp_url (no live/record/snapshot)", map[string]string{"camera": n})
		}
		if c.Cameras[i].Enabled && sourceHas(camDetectionSource(c, &c.Cameras[i]), "onvif") && c.Cameras[i].Onvif.Url != "" && strings.TrimSpace(c.Cameras[i].Onvif.Password) == "" {
			configWarn("config: ONVIF camera has an empty password (auth/PTZ/events may fail)", map[string]string{"camera": n})
		}
	}
	if c.Stream.Port != 0 && c.Stream.Port == c.Stream.RtspPort {
		configWarn("config: stream.port == stream.rtsp_port (go2rtc API and RTSP relay collide)", map[string]string{"port": strconv.Itoa(c.Stream.Port)})
	}
	if c.Stream.Port < 0 || c.Stream.Port > 65535 || c.Stream.RtspPort < 0 || c.Stream.RtspPort > 65535 {
		configWarn("config: stream port out of range 0..65535", nil)
	}
	if c.Storage.MaxDiskPct < 0 || (c.Storage.MaxDiskPct > 0 && c.Storage.MaxDiskPct < 10) {
		configWarn("config: storage.max_disk_pct must be 0 (off) or 10..99, disabled", nil)
		c.Storage.MaxDiskPct = 0
	}
	if c.Storage.MaxDiskPct > 99 {
		configWarn("config: storage.max_disk_pct > 99, using 99", nil)
		c.Storage.MaxDiskPct = 99
	}
	if c.Storage.MinFreeGB < 0 {
		configWarn("config: storage.min_free_gb < 0, disabled", nil)
		c.Storage.MinFreeGB = 0
	}
	switch strings.ToLower(strings.TrimSpace(c.Stream.Go2rtcLogLevel)) {
	case "", "trace", "debug", "info", "warn", "error":
		if strings.TrimSpace(c.Stream.Go2rtcLogLevel) == "" {
			c.Stream.Go2rtcLogLevel = "warn"
		}
	default:
		configWarn("config clamped: stream.go2rtc_log_level unknown, using warn", map[string]string{"value": c.Stream.Go2rtcLogLevel})
		c.Stream.Go2rtcLogLevel = "warn"
	}
	if isUnsafeGo2rtcPath(c.Stream.Go2rtcPath) {
		configWarn("config: stream.go2rtc_path looks unsafe (must be a plain path or binary name, no shell metacharacters)", map[string]string{"value": c.Stream.Go2rtcPath})
	}
	if u := strings.TrimSpace(c.Server.OnvifNotifyURL); u != "" && !(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
		configWarn("config: server.onvif_notify_url should be an http(s) URL reachable from the cameras", map[string]string{"value": redactNotifyURL(u)})
	}
	validateDataDirs(c)
}

// quietConfigLogs suppresses config WARN emission (healthcheck probes call
// loadConfig every 30s; clamped values must not spam docker logs).
var quietConfigLogs = false

func configWarn(msg string, kvs map[string]string) {
	if quietConfigLogs {
		return
	}
	logIt(LWARN, msg, kvs)
}

func loadConfig(path string) (*Config, error) {
	var blob []byte
	var err error
	blob, err = os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	configDefaults(&c)
	var dec = yaml.NewDecoder(bytes.NewReader(blob))
	dec.KnownFields(true)
	if derr := dec.Decode(&c); derr != nil {
		return nil, derr
	}
	// Clamp record windows: zero/negative values would yield degenerate ring
	// sizes and clip windows (maxSeg<=0, negative durations).
	if c.Record.PreDetection < 0 {
		configWarn("config clamped: record.pre_detection < 0, using 0", nil)
		c.Record.PreDetection = 0
	}
	if c.Record.PostDetection < 0 {
		configWarn("config clamped: record.post_detection < 0, using 0", nil)
		c.Record.PostDetection = 0
	}
	if c.Server.RetentionDays < 0 {
		configWarn("config clamped: server.retention_days < 0, using 0 (never prune)", nil)
		c.Server.RetentionDays = 0
	}
	// Per-type retention: normalize keys, drop unknown labels and negatives
	// (both fall back to the global retention with a warning).
	if len(c.Record.RetentionByType) > 0 {
		var clean = map[string]int{}
		for k, v := range c.Record.RetentionByType {
			var t = strings.ToLower(strings.TrimSpace(k))
			if !detectionLabels[t] {
				configWarn("config: ignoring unknown record.retention_by_type entry", map[string]string{"type": k})
				continue
			}
			if v < 0 {
				configWarn("config: ignoring negative record.retention_by_type entry (uses global retention)", map[string]string{"type": t})
				continue
			}
			if v > 0 {
				clean[t] = v
			}
		}
		c.Record.RetentionByType = clean
	}
	if c.Alerts.Pushover.SnapshotQuality < 1 || c.Alerts.Pushover.SnapshotQuality > 100 {
		configWarn("config clamped: alerts.pushover.snapshot_quality not in 1..100, using 70", nil)
		c.Alerts.Pushover.SnapshotQuality = 70
	}
	if c.Alerts.Pushover.SnapshotMaxPx < 0 {
		configWarn("config clamped: alerts.pushover.snapshot_max_px < 0, using 0", nil)
		c.Alerts.Pushover.SnapshotMaxPx = 0
	}
	if c.Vision.TimeoutS < 2 || c.Vision.TimeoutS > 30 {
		configWarn("config clamped: vision.timeout_s not in 2..30, using 10", nil)
		c.Vision.TimeoutS = 10
	}
	if c.Vision.MinConf < 0 || c.Vision.MinConf > 1 {
		configWarn("config clamped: vision.min_conf not in 0..1, using 0.25", nil)
		c.Vision.MinConf = 0.25
	}
	if c.Vision.MaxWorkers < 1 || c.Vision.MaxWorkers > 8 {
		configWarn("config clamped: vision.max_workers not in 1..8, using 2", nil)
		c.Vision.MaxWorkers = 2
	}
	switch strings.ToLower(strings.TrimSpace(c.Vision.Snapshots)) {
	case "", "primary":
		c.Vision.Snapshots = "primary"
	case "all":
		c.Vision.Snapshots = "all"
	default:
		configWarn("config clamped: vision.snapshots must be primary|all, using primary", nil)
		c.Vision.Snapshots = "primary"
	}
	c.Vision.URL = strings.TrimSuffix(strings.TrimSpace(c.Vision.URL), "/")
	// Clamp the go2rtc log level now (not just warn): the generated go2rtc
	// config carries it verbatim. validateConfig re-clamps for configs that
	// bypass loadConfig.
	if orig := strings.TrimSpace(c.Stream.Go2rtcLogLevel); orig != "" && clampGo2rtcLogLevel(orig) != strings.ToLower(orig) {
		configWarn("config clamped: stream.go2rtc_log_level unknown, using warn", map[string]string{"value": c.Stream.Go2rtcLogLevel})
	}
	c.Stream.Go2rtcLogLevel = clampGo2rtcLogLevel(c.Stream.Go2rtcLogLevel)
	// detection: validate the source, transport, windows and type allow-list.
	switch strings.ToLower(strings.TrimSpace(c.Detection.Source)) {
	case "", "onvif":
		c.Detection.Source = "onvif"
	case "rtsp", "both":
		c.Detection.Source = strings.ToLower(strings.TrimSpace(c.Detection.Source))
	default:
		configWarn("config clamped: detection.source must be onvif|rtsp|both, using onvif", nil)
		c.Detection.Source = "onvif"
	}
	switch strings.ToLower(strings.TrimSpace(c.Detection.RTSP.Transport)) {
	case "udp":
		c.Detection.RTSP.Transport = "udp"
	default:
		c.Detection.RTSP.Transport = "tcp"
	}
	if c.Detection.RTSP.DebounceS < 0 {
		configWarn("config clamped: detection.rtsp.debounce_s < 0, using 0", nil)
		c.Detection.RTSP.DebounceS = 0
	}
	if c.Detection.RTSP.DebounceS > 3600 {
		configWarn("config clamped: detection.rtsp.debounce_s > 3600, using 3600", nil)
		c.Detection.RTSP.DebounceS = 3600
	}
	if c.Detection.RTSP.HoldS < 1 {
		configWarn("config clamped: detection.rtsp.hold_s not in 1..60, using 2", nil)
		c.Detection.RTSP.HoldS = 2
	}
	if c.Detection.RTSP.HoldS > 60 {
		configWarn("config clamped: detection.rtsp.hold_s not in 1..60, using 60", nil)
		c.Detection.RTSP.HoldS = 60
	}
	var types []string
	for _, t := range c.Detection.RTSP.Types {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if !detectionLabels[t] {
			configWarn("config: ignoring unknown detection.rtsp.types entry", map[string]string{"type": t})
			continue
		}
		types = append(types, t)
	}
	if len(types) == 0 {
		types = []string{"motion", "person", "vehicle", "pet"}
	}
	c.Detection.RTSP.Types = types
	clampPerCamDetection(&c)
	switch strings.ToLower(strings.TrimSpace(c.Server.LogLevel)) {
	case "debug", "trace":
		logMin = LDEBUG
	case "warn", "warning":
		logMin = LWARN
	case "error":
		logMin = LERR
	default: // info (and empty)
		logMin = LINFO
	}
	validateConfig(&c)
	return &c, nil
}
