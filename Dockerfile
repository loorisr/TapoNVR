# syntax=docker/dockerfile:1.6
FROM golang:1.27-alpine AS builder
ENV GOTOOLCHAIN=auto
WORKDIR /src

# Cache dependencies (BuildKit cache survives rebuilds)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy only Go sources — webui/config/data changes must not bust the build cache
COPY src/ ./src/

# Build static binary (modernc/sqlite is pure Go, no CGO needed)
# Sources are in ./src (package main)
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/taponvr ./src \
    && chmod +x /out/taponvr \
    && mkdir -p /out/blank-data && chmod 770 /out/blank-data

# ---------- go2rtc fetch stage (isolated so curl doesn't bloat final image) ----------
FROM alpine:3.24 AS go2rtc-fetch
# Pinned by default for reproducible builds; override with
# --build-arg GO2RTC_VERSION=vX.Y.Z to bump. "latest" is still accepted but
# non-reproducible and skips verification unless GO2RTC_SHA256 is set.
# NOTE: every go2rtc release before v1.9 was deleted upstream, so its download
# URLs now 404 — the pin must stay on an existing release tag.
ARG GO2RTC_VERSION=v1.9.14
# Per-architecture integrity pins for GO2RTC_VERSION. A single GO2RTC_SHA256
# cannot work multi-arch: each CPU downloads a different artifact. Pass
# GO2RTC_SHA256 to override the default pin when bumping GO2RTC_VERSION
# (all other arches still use the table below, so build per-arch or bump all).
ARG GO2RTC_SHA256=
# Escape hatch for local/dev builds only: --build-arg ALLOW_UNPINNED=1 skips
# verification. Never set this in CI/release builds: an unpinned download is a
# supply-chain risk (a compromised upstream release would be baked into the
# image unverified).
ARG ALLOW_UNPINNED=
# Multi-arch: the old hardcoded go2rtc_linux_amd64 made arm64/armv7 images
# (Raspberry Pi) silently non-functional.
# NOTE: upstream has no armv7 asset — 32-bit hard-float ARM ships as
# go2rtc_linux_arm (only armv6 has a dedicated artifact).
ARG TARGETARCH
ARG TARGETVARIANT
RUN apk add --no-cache curl file \
    && arch="${TARGETARCH}${TARGETVARIANT}" \
    && case "$arch" in \
         amd64) art=go2rtc_linux_amd64; expect=32d616af226bd731678ffde328b94cfb94e30339bfefc469cfb76323144615a6 ;; \
         arm64) art=go2rtc_linux_arm64; expect=359fabade8a7a51e81a55fe6df6b0ef81764a5e1d63179577534eaaa71904b50 ;; \
         armv7) art=go2rtc_linux_arm;   expect=4d7e1639af5a2722a28e864468fd8099b3c1682565446c798bf9e3b38fde12e4 ;; \
         armv6) art=go2rtc_linux_armv6; expect=4dc20370556b29f3a90f4c7a09dcd95472c8f74cca56d4d1fb91f32bdd15174c ;; \
         *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
       esac \
    && [ -z "$GO2RTC_SHA256" ] || expect="$GO2RTC_SHA256" \
    && curl -fsSL "https://github.com/AlexxIT/go2rtc/releases/download/${GO2RTC_VERSION}/${art}" -o /out-go2rtc \
    && if [ "$ALLOW_UNPINNED" = "1" ]; then \
         echo "WARNING: building with unpinned go2rtc ${GO2RTC_VERSION} (ALLOW_UNPINNED=1, dev only)" >&2; \
       else \
         [ -n "$expect" ] || { echo "error: no sha256 pin for $arch at GO2RTC_VERSION=${GO2RTC_VERSION}: pass --build-arg GO2RTC_SHA256=<sha256> or ALLOW_UNPINNED=1 (dev only)" >&2; exit 1; }; \
         echo "${expect}  /out-go2rtc" | sha256sum -c -; \
       fi \
    && chmod +x /out-go2rtc \
    && file /out-go2rtc | grep -q "ELF"

# ---------- runtime ----------
# distroless/static:nonroot ships ca-certificates + tzdata + a nonroot user
# (65532:65532) with no shell or package manager. Both binaries are static,
# so nothing else is needed at runtime.
# no ffmpeg: snapshots via ONVIF HTTP (primary) + pure-Go gortsplib+hi264 (IDR-only fallback), recording is Go-native (gortsplib+mpegts/pmp4)
FROM gcr.io/distroless/static:nonroot

# go2rtc binary from fetch stage (matches stream.go2rtc_path default /usr/bin/go2rtc)
COPY --from=go2rtc-fetch /out-go2rtc /usr/bin/go2rtc

# App binary
COPY --from=builder /out/taponvr /usr/local/bin/taponvr

# WebUI prebuilt assets (served by the binary via server.webui_dir)
COPY webui/dist /app/webui/dist

# Example config as reference inside image; also installed as the default
# /app/config.yaml so a fresh `docker run` (no bind-mount) starts instead of
# crash-looping. A bind-mounted /app/config.yaml still overrides this.
COPY config.example.yaml /app/config.example.yaml
COPY config.example.yaml /app/config.yaml

WORKDIR /app

# Data volume (detections, db.sqlite) — mount host ./data or a named volume.
# Mode 0777 on purpose: compose runs the container as the invoking host user
# (`user: ${UID:-1000}`) while the image's default UID is 65532, and a named
# volume inherits this directory's ownership/mode at creation time. A tighter
# 0770 would lock out every UID other than the image default (SQLITE_CANTOPEN:
# "unable to open database file" on first start), and distroless has no shell
# to chown at runtime. It is a dedicated volume dir; files inside stay 0600
# (db) / 0750 (media dirs) as the app creates them. For a ./data bind-mount,
# chown the host dir to the UID you run as.
COPY --from=builder --chmod=0777 /out/blank-data /app/data

VOLUME ["/app/data"]

# Default runtime user for plain `docker run` (no shell/chown possible on
# distroless). docker compose overrides this with the invoking host user.
USER 65532:65532

# Only the externally-reachable listeners. The go2rtc API (stream.port) and RTSP
# relay (stream.rtsp_port) bind 127.0.0.1 in the generated go2rtc config, so they
# are deliberately not exposed.
EXPOSE 8971 8555/tcp 8555/udp

# Healthcheck: API requires BasicAuth, so 401 also means the server is up.
# Uses the binary itself (no shell/wget/curl on distroless).
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/taponvr", "--healthcheck", "--config", "/app/config.yaml"]

ENTRYPOINT ["/usr/local/bin/taponvr", "--config", "/app/config.yaml"]
