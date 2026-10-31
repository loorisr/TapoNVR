<h1 align="center">TapoNVR</h1>

<p align="center">
  <strong>Ultra-light, self-hosted NVR for Tapo, RTSP and ONVIF cameras.</strong><br>
  One Go binary · one YAML file · no ffmpeg
</p>

<p align="center">
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.27+"></a>
  <img src="https://img.shields.io/badge/image-~40%20MB-2496ED?logo=docker&logoColor=white" alt="Docker image ~40 MB">
  <img src="https://img.shields.io/badge/ffmpeg-not%20required-success" alt="No ffmpeg required">
</p>

TapoNVR is a small, self-hosted NVR aimed at Tapo and other RTSP/ONVIF cameras.
It records clips and snapshots around detections, keeps a RAM-backed pre-roll
buffer, and serves a multi-camera live view — all from a single Go binary and one
YAML file.

> **Design goal:** keep the server as small as possible. Video is never
> re-encoded, and detection is delegated to the camera (ONVIF events or the AI
> boxes already embedded in the H.264 stream). A local YOLO sidecar is optional.

## Contents

- [Why TapoNVR?](#why-taponvr)
- [Features](#features)
- [Dependencies](#dependencies)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Detection via RTSP](#detection-via-rtsp)
- [Recording and buffers](#recording-and-buffers)
- [API](#api)
- [Reverse proxy](#reverse-proxy)
- [Storage layout](#storage-layout)
- [Backup and restore](#backup-and-restore)
- [Web UI](#web-ui)
- [Testing](#testing)
- [Updating dependencies](#updating-dependencies)
- [Notes and limitations](#notes-and-limitations)

## Why TapoNVR?

| Criterion | **TapoNVR** | **LightNVR** | **Frigate** |
|-----------|-------------|--------------|-------------|
| **Philosophy** | Ultra-light, Tapo/ONVIF-focused, no local ML | Lightweight general-purpose NVR with NVR-side detection | Full NVR with local AI and Home Assistant integration |
| **Docker image size** | **~40 MB** (Go binary + go2rtc, distroless, **no ffmpeg**, no shell) | **~875 MB** | **~6.97 GB** |
| **Idle RAM (1 camera)** | **~40–80 MB** + 512 MB tmpfs buffer (RAM, no disk wear) | ~200–400 MB | ~800 MB – 1.5 GB (no Coral), ~500 MB with Coral |
| **RAM while recording** | ~80–120 MB + buffer | ~300–500 MB | ~1–2.5 GB (decode + TensorFlow/OpenVINO) |
| **CPU** | Very low (Go/gortsplib, no transcoding) | Low–medium | High (decode + inference) |
| **Detection** | ONVIF webhook **and/or** SEI boxes + type read from the RTSP stream (person/vehicle/pet/motion) — no broken PullPoint | NVR-side motion detection | Local AI (person, car, etc.) via Coral/GPU |
| **Pre-roll** | Yes, ring buffer in tmpfs (`pre_detection` seconds before the event) | Yes (config-dependent) | Yes (event recording) |
| **Dependencies** | Go + go2rtc (no ffmpeg) | FFmpeg included | FFmpeg + Python + ML model + Coral/GPU drivers |
| **Setup complexity** | 1 binary, 1 YAML file, `docker compose up` | 1 container, configuration UI | Compose + extensive YAML, MQTT, Coral recommended |
| **Best for** | 1–8 Tapo/ONVIF cameras, light NAS, Pi 4/5, low power | Small generic NVR without heavy AI | Setups requiring local AI and deep Home Assistant integration |

> **In short:** TapoNVR targets the *minimum viable NVR*: about **22× smaller**
> than LightNVR and **175× smaller** than Frigate, with 5–10× less RAM and
> near-zero CPU because detection is delegated to the camera. If you need local
> AI independent of the camera, Frigate remains the reference — at the cost of a
> ~7 GB image and much higher RAM/CPU/GPU requirements.

## Features

**Live and recording**
- Multi-camera live view via `go2rtc` (WebRTC + HLS), with no re-encode.
- Event recording or a continuous rolling buffer with pre-roll.
- RAM-backed buffer (`/dev/shm`), so continuous writes never wear the flash.
- Configurable snapshots per detection, retention and disk guard.

**Detection**
- ONVIF webhook events (motion / person / vehicle / pet).
- Camera-side AI boxes and type read directly from H.264 SEI metadata.
- Live bounding-box overlay and a detection timeline.
- Optional YOLO sidecar analysis for snapshots.

**Integrations and operations**
- Local storage plus S3-compatible upload (MinIO, Cloudflare R2, …).
- MQTT events, Pushover notifications (optional best-frame attachment using the largest matching SEI box) and ONVIF PTZ control.
- Single binary, distroless Docker image, no ffmpeg and no shell.
- HTTP Basic Auth and reverse-proxy-friendly deployment.

## Dependencies

- **Go 1.27+**.
- **`go2rtc`** — low-latency WebRTC + HLS live streaming (snapshots no longer go
  through `api/frame.jpeg`).
- **RTSP recording** via Go (`gortsplib/v5` + `mediacommon/v2` MPEG-TS/MPEG-4) —
  no `ffmpeg` required.
- **Snapshots** via Go (`gortsplib/v5` + `Eyevinn/hi264` pure-Go IDR decoder) +
  ONVIF `GetSnapshotUri` + HTTP Digest — no `ffmpeg` required.
- **RTSP SEI detection** via Go (`gortsplib/v5` H.264 depacketization) — reads
  the camera's AI boxes/type from the H.264 stream, no `ffmpeg` required.
- **MinIO** (optional) — local S3-compatible object store for `storage.enabled`.

> **Source layout:** the sources live in `src/` as one `main` package, split by
> responsibility: `config` · `onvif` · `stream` · `detection_rtsp` · `records` ·
> `store` · `storage` · `prune` · `alerts` · `vision` · `mqtt` · `csv` · `api`.

## Quick start

**Requirements:** Docker + Docker Compose, or Go 1.27+ and `go2rtc` for a
bare-metal build. You also need a camera reachable over RTSP/ONVIF.

### Docker

```sh
cp config.example.yaml config.yaml
$EDITOR config.yaml

mkdir -p data && chown -R "$(id -u):$(id -g)" data
UID="$(id -u)" GID="$(id -g)" docker compose up -d --build
```

The container runs as your host UID/GID (via `user: "${UID:-1000}:${GID:-1000}"`
in `docker-compose.yml`, defaulting to `1000:1000`), so files created under
`./data` (detections, `db.sqlite`) are owned by you — no `chown 65532` needed.
The image default remains the distroless `nonroot` user (65532) for plain
`docker run`. Hardening (`read_only` rootfs, `cap_drop: ALL`,
`no-new-privileges`) is unaffected by the UID choice.

### From source

```sh
cp config.example.yaml config.yaml
$EDITOR config.yaml

go build -o taponvr ./src
./taponvr --config config.yaml
```

Open `http://<host>:8971` and sign in with the HTTP Basic credentials from
`server.auth`. The web UI shows the live grid, the detection timeline, and a
detail view with snapshot + clip.

> Change the example `admin` / `changeme` credentials before exposing the UI.

## Configuration

See `config.example.yaml` for the full file. Key settings:

| Key | Description |
|-----|-------------|
| `server.port` | HTTP port for the API and web UI (default `8971`) |
| `server.data_dir` | Storage root for detections and `db.sqlite` |
| `server.retention_days` | Delete media and DB rows older than this (`0` = keep forever) |
| `server.log_level` | Server log verbosity: `debug`, `info`, `warn`, `error` |
| `server.auth` | HTTP Basic credentials for the API and SPA |
| `server.auth.disabled` | Explicitly allow an open UI (only behind a trusted proxy) |
| `server.onvif_notify_url` | **Webhook URL** cameras POST motion events to; required for ONVIF detection, not for RTSP-only |
| `stream.go2rtc_path` / `stream.port` | go2rtc binary path and API port |
| `record.mode` | `event` (record around detections) or `buffer` (rolling window, frozen on detection) |
| `record.pre_detection` / `record.post_detection` | Seconds recorded before and after a detection |
| `record.enabled` | Master switch for clip recording (default `true`) |
| `record.snapshots.enabled` | Master switch for snapshots (default `true`) |
| `record.snapshots.count` / `record.snapshots.interval_s` | Snapshots per detection and minimum delay between captures (`count: -1` = shoot while motion is active, capped at 60 shots / 180 s) |
| `storage.enabled` / `storage.s3.*` | S3-compatible upload (MinIO, R2, …) |
| `mqtt.*` | MQTT broker connection and event publishing |
| `vision.*` | Optional YOLO snapshot analysis via an external `POST /v1/detect` API (async, degraded-silent) |
| `detection.source` | `onvif` (webhook, type only — default), `rtsp` (camera AI boxes + type from H.264 SEI), or `both` |
| `detection.rtsp.*` | SEI reader tuning: `transport`, `debounce_s`, `hold_s`, `types`, `use_go2rtc` |
| `alerts.pushover.*` | Pushover notifications (`events` limits which detection types notify — empty means all; `send_snapshot` attaches a compressed copy; `snapshot_pick: best` waits for the burst and attaches the best frame — YOLO class/confidence when `vision.enabled`, else the largest matching SEI box with sharpness fallback; oversized copies fall back to text-only). Messages include the detection source (ONVIF/RTSP/manual/test), the camera identification and the YOLO label + confidence when available. |

### Pushover: best attached frame

`alerts.pushover.snapshot_pick: best` delays the alert until the timed snapshot
burst ends (bounded by `best_max_wait_s`, default 5 s) and attaches the frame
with the largest SEI box matching the detection tags. Near-ties prefer a
specific type (`person`/`vehicle`/`pet`) over a generic `motion` box, then the
sharpest frame, then the most recent one.

When `vision.enabled` is true, `best` mode also sends the burst candidates to
the YOLO API (at most 12 candidates, four concurrent, short time budget) and
picks the frame with the most relevant class and highest confidence
(`person`/`vehicle`/`pet` mapping, with `motion`/`other` as wildcards). If no
candidate has a matching YOLO box, selection falls back to the SEI
box/sharpness heuristic. This works even when `vision.snapshots: primary`:
the extra frames are analyzed for this attachment decision and their results
are stored for the UI.

Every Pushover message names the detection source (`ONVIF`, `RTSP`, `ONVIF +
RTSP`, `Manual` or `Test`) and the identification sources, for example:

```text
person detected on camera-1
Detection source: RTSP
Identification: camera=person,motion | YOLO=person 86% (3 boxes)
```

All snapshot modes are **buffer-first**: when `record.mode: buffer` has a
decodable frame, it is used immediately. The ring frame is the event image
(within one GOP, logged as `image_age_ms`) and never waits for the live chain;
a buffer frame is static and cannot be several seconds late like a fallback
capture. This keeps the primary snapshot, the detection row and the alert at
the event time instead of 30–40 s later.

`best` mode may spend one short attempt (≤1.5 s) on the camera's **native
snapshot** when the buffer IDR is older than 1.5 s, so the attached frame can
match the live SEI boxes; if that fails, the buffer frame is kept. Extras are
always buffer-first so the whole burst stays within `best_max_wait_s`.

SEI boxes for buffer frames come from a short per-camera box history at the
image timestamp (`seiBoxesAt`), never from the current live frame. A buffer
frame can therefore be paired with the boxes that were actually on screen when
that image was taken. Boxes are dropped when the closest sample is more than
~1.5 s away; live sources still use the latest boxes.

When the ring has no decodable frame (cold start, recorder down, legacy
`record.mode: event`), the live fallback is bounded by a global 4 s budget:
cached ONVIF snapshot URI first, then RTSP, then HTTP probes. The total never
exceeds the budget, and the detection is recorded even when every source fails.
The ONVIF snapshot URI is resolved in the background at startup, so discovery
is never on the detection hot path.

Without usable boxes (ONVIF-only source, no history, or no box in any
candidate) the sharpest non-placeholder frame wins, then the primary. `primary`
(the code default for existing configs) keeps the immediate trigger-frame
behavior.

Buffer frames are timestamped from the segment's mtime minus the IDR age inside
the segment, so `image_age_ms` in the logs is real wall-clock age. The
`snapshot buffer used` / `snapshot boxes paired` (capture) and `pushover message
sent` (`picked_source`, `picked_age_ms`, `picked_boxes`) logs make any remaining
offset easy to check. Only the attached copy is affected by `best` selection:
stored snapshots, S3 uploads and the primary shown in the UI are unchanged.

### Camera

```yaml
cameras:
  - name: camera-1
    enabled: true
    onvif:
      url: http://<ip>/onvif
      username: admin
      password: change-me
    rtsp_url: rtsp://admin:change-me@<ip>/h264Preview_01_main
    ptz: auto     # or 'off'
    # Optional per-camera detection source (onvif|rtsp|both|off); empty inherits
    # detection.source. E.g. read this camera's boxes from RTSP while others
    # stay on the ONVIF webhook:
    # detection_source: rtsp
```

- `ptz: auto` queries ONVIF and enables the PTZ pad only if the device supports
  it (C530WS is a fixed dome → controls stay hidden).
- Motion detection uses the camera's **ONVIF webhook**: the NVR registers its
  `server.onvif_notify_url` with each camera (WS-Base Subscribe over ONVIF port
  2020), and the camera POSTs a notification to that URL when motion/person/
  vehicle/pet is detected. This is the only working path on Tapo firmwares where
  ONVIF PullPoint eventing is broken (`ter:InvalidArgVal`). The URL must be plain
  HTTP and reachable from every camera — set it to this NVR's LAN IP.
  - Tapo App → Me → Third-Party Compatibility must be ON.
  - No local ML classification in v1; the type comes from the camera's event topic
    (motion / cellmotion / person / vehicle / pet).

## Detection via RTSP

As an alternative (or in addition) to the ONVIF webhook, TapoNVR can read the
camera's own AI detections straight from the RTSP stream. The type **and the
bounding box** travel as per-frame H.264 **SEI** metadata (`TPLINKMARKERBOX.`,
SEI type 5; `IPC-DETECTED-BOX`, SEI type 764), next to the pixels — the same data
the Tapo app draws on the live view. Nothing is re-encoded: the reader reuses the
pure-Go `gortsplib` decode path (no `ffmpeg`).

```yaml
detection:
  source: rtsp          # onvif (default) | rtsp | both
  rtsp:
    transport: tcp
    debounce_s: 10      # min gap between two detections of the same type
    hold_s: 2           # keep "active" this long after the last box (clip end)
    types: [motion, person, vehicle, pet]
    use_go2rtc: true    # pull the local go2rtc relay, not a 2nd camera connection
cameras:
  - name: camera-1
    detection_source: rtsp   # optional per-camera override (onvif|rtsp|both|off)
```

- **Data and coordinates.** Boxes are normalized `[0,1]`; the box is stored on
  the detection (`bbox`, JSON array of `{type,x1,y1,x2,y2,score?}`), exposed by
  `/api/detections` and `/api/onvif_events`, included in MQTT payloads, and can
  be burned into the snapshot with
  `GET /api/detections/{id}/snapshot?boxes=1` (serve-time only; the stored file
  is untouched). A YOLO result, when present for that exact file, takes
  precedence.
- **Live overlay.** While the SEI reader runs, the boxes are also pushed to the
  WebUI over the existing SSE channel (`GET /api/events`, event `detection`,
  payload `{"kind":"boxes","camera_id":"camN","bbox":[...]}`, throttled to
  5 updates/s per camera) and drawn on a transparent canvas over the live
  `<video>`. Boxes fade out ~0.9 s after the camera stops reporting them, so a
  stale box never sticks on screen. The `Boxes` checkbox in the Live view header
  toggles the overlay (persisted in `localStorage`); no extra endpoint, no
  re-encode and no CPU cost on the server beyond the parse it already does.
  Updates are held `liveBoxesDelay` ms (default 200) to reduce the lead over the
  WebRTC/MSE picture; set it in the browser console with
  `localStorage.setItem("liveBoxesDelay","0")` (0–3000) and reload. Types are
  colour-coded (motion/person/vehicle/pet/other) and every box the camera reports
  is drawn, including types filtered out of `detection.rtsp.types` (the overlay
  is informational; filtering only affects detections/recordings).
- **SEI extraction.** Tapo cameras send the boxes as SEI-only access units that
  share the frame's RTP timestamp. `gortsplib` ≥ v5.6.7 deliberately drops that
  pattern (to keep recorders clean), so the reader depacketizes SEI NALs
  directly from the RTP packets (single-NAL, STAP-A/B, MTAP16/24, FU-A/B)
  instead of using the format's decoder. Reading the boxes from the H.264 stream
  still requires the camera firmware to emit `TPLINKMARKERBOX.` /
  `IPC-DETECTED-BOX` markers.
- **Debouncing and hold.** SEI markers repeat on nearly every frame and can be
  present on an idle, static scene (e.g. a parked car), so a type is emitted at
  most once per `debounce_s` window. While boxes keep arriving the camera is kept
  "active"; it is released once no box has been seen for `hold_s`, which ends the
  clip's post-detection window (like an ONVIF inactive event). A static scene
  therefore yields a bounded number of detections/groups, not a per-frame flood.
- **Type mapping.** `detection.rtsp.types` filters which types create detections;
  `type` is mapped to the same labels the rest of the app uses (`motion`,
  `person`, `vehicle`, `pet`, `other`). `IPC-DETECTED-BOX` carries no type enum,
  so it is reported as `motion`.
- **Merged tags.** A single event window often mixes types (motion first, then
  person/vehicle/pet). Every tag is kept in the detection's `det_types` list and
  exposed as `types` in the API/UI, while the most specific one becomes the
  primary `type` (`person`/`vehicle`/`pet` > `other` > `motion`; ties keep the
  first specific tag). Timelines and raw events still show the per-event tag.
  `GET /api/detections?type=person` matches any merged tag, and
  `record.retention_by_type` applies to the primary (promoted) type. A more
  specific tag arriving after the clip row was inserted republishes the MQTT
  `detection` payload and sends an extra Pushover notification.
- **Merging sources.** Set `detection.source: both` to merge webhook and SEI
  detections. Both feed the same grouping path, so a single event window is
  created (the two sources cannot prematurely end each other's active state).
- **Relay vs. direct.** `use_go2rtc: true` (default) pulls the local go2rtc relay
  so no second direct camera connection is opened. If your go2rtc build does not
  pass the SEI NALs through its re-broadcast, set `use_go2rtc: false` to read the
  camera directly (transport `tcp` recommended). The reader reconnects
  automatically with backoff.
- **Firmware support.** Requires the camera to expose the box metadata (Tapo
  C220/C530WS-era firmware).

| Key (`detection.rtsp.*`) | Default | Meaning |
|---|---|---|
| `transport` | `tcp` | RTSP transport for the SEI reader (`tcp` or `udp`) |
| `debounce_s` | `10` | Minimum delay between two emitted detections of the same type (0–3600) |
| `hold_s` | `2` | Keep a type "active" this long after the last box (1–60) |
| `types` | `[motion, person, vehicle, pet]` | Allow-list of detection types |
| `use_go2rtc` | `true` | Use the local go2rtc relay instead of a second camera connection |

## Recording and buffers

In `record.mode: buffer` (default), each camera keeps a continuous **rolling
ring buffer** of the last `record.pre_detection` seconds of video. It lives in a
**RAM-backed tmpfs** (`/dev/shm/taponvr/buffer/<cam>/` on Linux) by default, so
the continuously-recycled segments are never written to durable flash and cause
no disk wear; only the finished clips and snapshots are saved to `data_dir`.
Override the location with `record.buffer_dir`.

When a detection occurs:

- the primary snapshot is captured immediately,
- a clip is assembled **in parallel**, covering the **pre-event buffer** plus the
  `record.post_detection` seconds after the event,
- `record.snapshots.count` snapshots are taken with `record.snapshots.interval_s`
  seconds between them (the primary one first, then the extras).

The clip therefore starts *before* the detection, not at the moment it fires.
Go timestamps are derived from RTSP PTS/DTS via `h264.DTSExtractor`, so H.264
B-frame streams stay monotonic and segments concatenate cleanly. The recorder
logs to the main log — no `ffmpeg` diagnostics.

Recording pulls the stream relayed through **go2rtc**
(`rtsp://127.0.0.1:<stream.port>/camN`) rather than opening a second direct RTSP
connection to each camera. A single re-broadcast serves both live view and
recording, so camera load stays low.

```yaml
record:
  mode: buffer
  pre_detection: 10    # pre-event pre-roll length
  post_detection: 10   # length of the "after" portion appended to the clip
  buffer_dir: ""       # RAM-backed tmpfs by default; set an explicit path to
                       # keep the rolling buffer on disk instead
  snapshots:
    count: 3           # how many snapshots per detection
    interval_s: 2      # seconds between each snapshot
```

The legacy aliases `buffer_seconds` / `clip_seconds` are still accepted.

`record.mode: event` keeps the legacy behaviour (no pre-roll; one clip recorded
from the event time).

## API

### Cameras and live view

| Endpoint | Description |
|----------|-------------|
| `GET /api/cameras` | Configured cameras + `ptz_available` |
| `GET /api/events` | SSE live channel: `detection` events (`kind:"boxes"` carries the current RTSP SEI boxes; other events carry the detection banner / vision refresh) |
| `POST /api/test/detection` | Simulate an event end-to-end (dev/debug) |
| `POST /api/cameras/{id}/snapshot` | Manual snapshot (uploaded to S3 when enabled, `manual/<cam>/` key) |
| `POST /api/cameras/{id}/ptz` | PTZ move (only if available) |
| `GET/POST /api/cameras/{id}/detection` | Per-camera tuning (`debounce_s` 0..3600, `hold_s` 1..60); GET prefills, POST saves; 404 when unsupported |
| `GET /api/manual/jobs/{id}` | Async legacy manual-record job status (`running`/`done`/`error` + `clip`); the UI polls it after a 202 `status_url` |

### Detections and media

| Endpoint | Description |
|----------|-------------|
| `GET /api/detections?camera=&type=` | Detection timeline (`&label=person&has_vision=1` filters YOLO results); `type` matches any merged tag, and each row carries `type` (primary), `types` (all merged tags), `source` (onvif/rtsp/manual/test, merged when both paths contributed) and `bbox` (RTSP SEI boxes, `null` when none) |
| `GET /api/detections/{id}/snapshot` | Snapshot JPEG |
| `GET /api/detections/{id}/snapshot?boxes=1` | Snapshot JPEG with boxes burned in (its own YOLO result, else the camera's RTSP SEI boxes; serve-time only, stored file untouched; plain JPEG when neither applies) |
| `GET /api/detections/{id}/clip` | Clip MP4 |
| `GET /api/detections/{id}/timeline` | Timeline data for the detail modal (start/end, raw events, YOLO results, clip/S3 status) |
| `POST /api/detections/{id}/lock` | Lock/unlock a recording (`{"locked":true}`); locked recordings are excluded from automatic pruning |
| `DELETE /api/detections/{id}` | Delete a detection (snapshot + clip + extras) |
| `DELETE /api/detections` | Delete all detections, uncapped (snapshot + clip + extras, local + S3, empty dirs pruned) |
| `GET /api/onvif_events` | Raw ONVIF/RTSP event list (each row may carry `bbox`) |
| `DELETE /api/onvif_events/{id}` | Delete one raw event (recording untouched) |
| `DELETE /api/onvif_events?detection_id=` | Delete all raw events of one group |
| `DELETE /api/onvif_events` | Delete all raw events |

### Vision

| Endpoint | Description |
|----------|-------------|
| `GET /api/vision/status` | YOLO API readiness (`enabled,ready,model`) without leaking the key |
| `GET/POST /api/vision/state?enabled=` | Vision analysis switch |
| `GET /api/detections/{id}/vision` | YOLO results: best-result summary + per-snapshot `results[]` (index, snapshot URL, boxes) |
| `POST /api/detections/{id}/vision` | Re-analyze the primary snapshot with YOLO now |
| `POST /api/detections/{id}/vision/exclude` | Vision feedback: exclude a wrong YOLO label (`{"snap_index":0,"label":"person"}`); 404 when unsupported |

### System, storage and toggles

| Endpoint | Description |
|----------|-------------|
| `GET /api/status` | Uptime / health |
| `GET /api/system/status` | Status tab payload: streams (+RTSP probe), RAM, S3/push/MQTT/detection/recording/snapshot flags, `detection_source`, event/disk/screenshot/video stats, per-camera rows (source, RTSP box counters) |
| `GET/POST /api/detection/state?enabled=` | Detection master switch |
| `GET/POST /api/recording/state?enabled=` | Clip recording switch |
| `GET/POST /api/snapshots/state?enabled=` | Snapshot switch |
| `GET/POST /api/s3/state?enabled=` | S3 upload switch (uploads only; deletes always run) |
| `POST /api/s3/retry` | Re-upload recordings pending S3 (`s3_expected=1 AND uploaded=0`) in background |
| `GET/POST /api/pushover/state?enabled=` | Push notification switch (throttled by `alerts.pushover.min_interval_s`) |
| `GET /api/mqtt/status` | MQTT connection + feature states |
| `GET /api/system/backup` | Backup descriptor JSON (config snapshot + DB/media inventory; see [Backup and restore](#backup-and-restore)) |

Basic Auth is required on all API and web routes, unless
`server.auth.disabled: true` explicitly allows an open UI — see
`config.example.yaml`.

## Reverse proxy

No TLS in the app by design — terminate TLS at the proxy and forward plain HTTP
to `127.0.0.1:8971`. Preserve client info and pass Basic auth through untouched.
The app relies on the browser's native Basic dialog; the session cookie is
`SameSite=Lax`, so keep the proxy on the same site or expect a re-login per
top-level navigation.

**Caddy**

```caddy
nvr.example.com {
	reverse_proxy 127.0.0.1:8971
}
```

**Nginx**

```nginx
server {
	listen 443 ssl;
	server_name nvr.example.com;
	# ssl_certificate / ssl_certificate_key ... (terminate TLS here)
	location / {
		proxy_pass http://127.0.0.1:8971;
		proxy_set_header Host $host;
		proxy_set_header X-Real-IP $remote_addr;
		proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
		proxy_set_header X-Forwarded-Proto $scheme;
		# Do NOT strip or inject Authorization: Basic auth passes through.
		proxy_http_version 1.1;
		proxy_set_header Upgrade $http_upgrade;
		proxy_set_header Connection "upgrade";  # for go2rtc WebRTC/ws
	}
}
```

## Storage layout

```text
data/
  detections/
    {YYYY-MM-DD}/{camera}/{HHMMSS}-{detid}-snap.jpg
    {YYYY-MM-DD}/{camera}/{HHMMSS}-{detid}-{type}.mp4
  db.sqlite
```

S3 keys mirror the local layout under `storage.s3.prefix`; every upload uses
`storage.s3.storage_class` (default `ONEZONE_IA`). All snapshots (primary +
extras) and the clip for a detection are uploaded.

## Backup and restore

- **Descriptor:** `GET /api/system/backup` returns JSON describing what to back
  up (config snapshot + DB/media inventory).
- **Database:** `<server.data_dir>/db.sqlite` (default `./data/db.sqlite`) — stop
  the binary (or checkpoint) before copying for a consistent snapshot.
- **Media:** `<server.data_dir>/detections/` alongside the database.
- **Restore:** stop the app, restore `db.sqlite` + `detections/` into `data_dir`,
  then restart. S3 copies (when `storage.enabled`) are a second copy, not a
  restore source for the local database.

## Web UI

- `webui/dist/` — static SPA served by the binary (hand-maintained vanilla JS:
  `app.js`, `video-rtc.js`, `style.css`, `index.html`; no build step).
- Detections with camera-side boxes (or YOLO results) show a badge and, in the
  detail view, the box burned into the snapshot (server-side `?boxes=1`).
- The detection detail modal has three tabs — **Snapshots**, **Video** and
  **Timeline**. The timeline is built from `GET /api/detections/{id}/timeline`
  and shows, in chronological order with offsets from the detection start: the
  detection start/end, every raw ONVIF/RTSP event (with a ▣ marker when it
  carries SEI boxes), every YOLO snapshot result (count/top label/ms, with a
  snapshot button), and the clip / S3 status.
- **Per-camera tuning:** each Live tile exposes `debounce_s` (0..3600) and
  `hold_s` (1..60) with a Save button (`POST /api/cameras/{id}/detection`).
  Values are prefilled via `GET` on the same URL, otherwise the global
  `detection.rtsp` defaults are used. A 404 (older server) shows a toast and
  keeps the defaults.
- **Vision feedback:** the detection detail panel lists each YOLO box with a
  "wrong label" button (`POST /api/detections/{id}/vision/exclude` with
  `{snap_index, label}`). A 404 (older server) toasts gracefully; otherwise the
  label is excluded from future filtering/ranking.

## Testing

`go build` validates the whole binary. A simulated, end-to-end detection can be
triggered through the UI or with:

```sh
curl -u admin:change-me -X POST http://localhost:8971/api/test/detection
```

This writes a snapshot + clip, records a detection row, uploads to S3 (if
enabled) and sends a Pushover alert (if enabled and `motion` is in
`alerts.pushover.events`).

For RTSP detection, watch the startup log for `rtsp detection reader started` /
`rtsp detection connected`, and the Status tab for the per-camera source and RTSP
box counters. `go test ./src -count=1` runs the SEI parser / state-machine unit
tests (synthetic metadata, no camera needed).

The YOLO-assisted best-frame selection has an optional live test (skipped by
default) that can be run against a vision sidecar with two sample images:

```sh
TAPONVR_VISION_TEST_URL=http://192.168.10.120:8080 \
TAPONVR_VISION_BUS=/tmp/bus.jpg TAPONVR_VISION_EMPTY=/tmp/empty.jpg \
go test ./src -run TestLiveVisionBestPick -v -count=1
```

## Updating dependencies

| Command | What it does |
|---------|--------------|
| `./scripts/update-deps.sh` | Update everything, then run `go mod tidy`, `go build`, `go vet` and `go test` |
| `./scripts/update-deps.sh --go-only` | Go modules only (`go get -u ./...` + `go mod tidy`, with verification) |
| `./scripts/update-deps.sh --docker-only` | Dockerfile base image tags only |
| `./scripts/update-deps.sh --actions-only` | GitHub Actions pins only |
| `./scripts/update-deps.sh --check` | Dry-run; report what would change and exit 1 if updates are available |
| `./scripts/update-deps.sh --skip-verify` | Skip the `go build` / `go vet` / `go test` gate |

What it covers:

- **Go modules** (`go.mod` / `go.sum`), verified with `go build -o /dev/null ./src`,
  `go vet ./...` and `go test ./src -count=1`.
- **Dockerfile base images:** `golang:X-alpine`, `alpine:3.x` and the
  `# syntax=docker/dockerfile:1.x` directive (resolved via Docker Hub; highest
  stable tag wins). `gcr.io/distroless/static:nonroot` is a floating tag, so it
  is pull-validated rather than bumped.
- **GitHub Actions pins** in `.github/workflows/docker-publish.yml`
  (major-tracked `vN` tags, resolved via the GitHub API).
- **go2rtc** is pinned via `ARG GO2RTC_VERSION` in the Dockerfile — the script
  checks that the pinned release's download URL still resolves. It does **not**
  auto-bump the pin: re-pinning to a new release also means re-checking the
  `GO2RTC_SHA256` integrity pin if one is set.
- **docker-compose.yml images** (`eclipse-mosquitto:2`, `minio/minio:latest`)
  already track upstream; the script just reports them. The web UI
  (`webui/dist/`) is hand-maintained vanilla JS with no package-manager
  dependencies.

After running, review with `git diff --stat` and rebuild via
`docker compose build`.

## Notes and limitations

- **ONVIF PullPoint is unreliable on Tapo.** Motion is delivered via the ONVIF
  webhook (`server.onvif_notify_url`). If no detection arrives, verify that the
  camera can reach that URL (plain HTTP, same/VLAN-routable subnet) and that
  Third-Party Compatibility is enabled in the Tapo app.
- **RTSP detection is the fallback** when the webhook is unreliable or when you
  need coordinates. Set `detection.source: rtsp` to read the camera's own
  boxes/type from the H.264 SEI metadata. This depends on the camera firmware
  emitting the markers and on the SEI NALs surviving go2rtc
  (`use_go2rtc: false` falls back to a direct RTSP read).
- **TP-Link cameras allow very few RTSP clients.** Their session table can wedge
  after a network drop: the main stream (`stream1`) then accepts DESCRIBE/SETUP
  but goes silent at PLAY, while `stream2` keeps working. go2rtc retries with no
  delay when START fails, which hammers the camera.
  - **Reboot the camera** (Tapo app → Device settings → Reboot) to clear it.
  - TapoNVR appends `#backchannel=0` to every generated go2rtc source (two-way
    audio is unused), as recommended for glitchy TP-Link cameras. Leave your own
    `backchannel=...` fragment to opt out.
  - A built-in supervisor detects the reconnect storm (>40 `EOF`/`retry` lines
    in 10 s), pauses the go2rtc relay with exponential backoff (1 → 8 min) and
    logs a warning, so a wedged camera is not hammered until it is rebooted.
- **Direct RTSP costs connections.** With `detection.rtsp.use_go2rtc: false`,
  detection and recording each open a **direct** camera session; TP-Link cameras
  only allow a couple. Startup logs a warning in that case — keep the relay
  enabled when possible.
- **Vision uses a circuit breaker.** After a failed `/v1/detect` call, further
  automatic attempts back off from 15 s to 5 min and only the first failure logs
  a WARN (`workers.vision_backoff*` in `/api/system/status`). An explicit
  re-analyze from the UI always retries.
- **Detection types are camera-defined.** ONVIF/RTSP are used for detection and
  (ONVIF) PTZ control; detection types are whatever the camera emits (motion /
  person / vehicle / pet / other).
- **Battery cameras** drop off the network intermittently; reconnect/backoff is
  expected behaviour.
- **S3** signing uses SigV4 (see `s3WriteObject`); MinIO with auth disabled also
  works.
- **Out of scope for v1:** multi-user, TLS, local object classification, and
  snapshot overlay/PNG (boxes are burned into the served JPEG at request time,
  not stored).
