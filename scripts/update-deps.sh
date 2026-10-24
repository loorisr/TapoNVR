#!/usr/bin/env bash
#
# scripts/update-deps.sh — update Go modules, Dockerfile base images and
# GitHub Actions pins to latest.
#
# Usage:
#   scripts/update-deps.sh [--go-only] [--docker-only] [--actions-only]
#                          [--check] [--skip-verify] [--help]
#
#   Default (no flags): update everything in place, then verify with
#   `go mod tidy`, `go build`, `go vet` and `go test`.
#
#   --check: dry-run, report what would change, exit 1 if updates available.
#   GO2RTC is pinned via ARG GO2RTC_VERSION — the script verifies its download
#   URL still resolves (pre-1.9 releases were deleted upstream and 404, so a
#   stale pin silently breaks `docker compose build`).
#
set -euo pipefail

# ---------- options ----------
DO_GO=1
DO_DOCKER=1
DO_ACTIONS=1
CHECK=0
SKIP_VERIFY=0

usage() {
  sed -n '2,15p' "$0"
}

for arg in "$@"; do
  case "$arg" in
    --go-only)      DO_DOCKER=0; DO_ACTIONS=0 ;;
    --docker-only)  DO_GO=0; DO_ACTIONS=0 ;;
    --actions-only) DO_GO=0; DO_DOCKER=0 ;;
    --check)        CHECK=1 ;;
    --skip-verify)  SKIP_VERIFY=1 ;;
    -h|--help)      usage; exit 0 ;;
    *) echo "Unknown flag: $arg" >&2; usage >&2; exit 2 ;;
  esac
done

# ---------- helpers ----------
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m ok\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

# Repo root (works from anywhere inside the checkout).
ROOT="$(git rev-parse --show-toplevel 2>/dev/null || dirname "$(dirname "$(readlink -f "$0")")")"
cd "$ROOT"

DOCKERFILE="$ROOT/Dockerfile"
GOMOD="$ROOT/go.mod"
WORKFLOW="$ROOT/.github/workflows/docker-publish.yml"

NEEDS_UPDATE=0  # set to 1 in --check mode when something is outdated
CHANGED=0       # set to 1 when a file was modified on disk

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

# Extract "X.Y" style tag names from a Docker Hub API payload without jq.
# $1 = hub repo (e.g. library/golang), $2 = grep -E pattern for tag names
dockerhub_latest() {
  local repo="$1" pattern="$2" page tags latest
  page="$(curl -fsSL --max-time 30 \
    "https://hub.docker.com/v2/repositories/${repo}/tags?page_size=100&ordering=last_updated" 2>/dev/null)" \
    || return 1
  tags="$(printf '%s' "$page" | grep -o '"name":"[^"]*"' | cut -d'"' -f4 | grep -E "$pattern" || true)"
  [ -n "$tags" ] || return 1
  latest="$(printf '%s\n' "$tags" | sort -V | tail -n1)"
  [ -n "$latest" ] || return 1
  printf '%s' "$latest"
}

# Latest release tag of a GitHub repo (e.g. actions/checkout -> v7).
# Falls back to the tags API when /releases/latest 404s (repos with no releases).
github_latest_tag() {
  local repo="$1" out tag
  out="$(curl -fsSL --max-time 30 "https://api.github.com/repos/${repo}/releases/latest" 2>/dev/null)" && {
    tag="$(printf '%s' "$out" | grep -m1 '"tag_name":' | cut -d'"' -f4)"
    [ -n "$tag" ] && { printf '%s' "$tag"; return 0; }
  }
  out="$(curl -fsSL --max-time 30 "https://api.github.com/repos/${repo}/tags?per_page=5" 2>/dev/null)" \
    || return 1
  tag="$(printf '%s' "$out" | grep -m1 '"name":' | cut -d'"' -f4)"
  [ -n "$tag" ] || return 1
  printf '%s' "$tag"
}

# ---------- preflight ----------
require_cmd git
require_cmd curl
if [ "$DO_GO" -eq 1 ]; then
  require_cmd go
fi

if [ -n "$(git status --porcelain -- go.mod go.sum Dockerfile .github/workflows/ 2>/dev/null)" ]; then
  warn "working tree has uncommitted changes in dependency files — continuing, review the diff afterwards."
fi

# =====================================================================
# 1. Go modules
# =====================================================================
if [ "$DO_GO" -eq 1 ]; then
  log "Go: $(go version) (module $(basename "$ROOT"))"
  if [ "$CHECK" -eq 1 ]; then
    log "Go --check: listing available module updates (no changes made)"
    # `go list -m -u all` prints "[vX.Y.Z]" next to modules with updates.
    if go list -m -u all 2>/dev/null | grep -E '\['; then
      NEEDS_UPDATE=1
    else
      ok "Go modules already at latest"
    fi
  else
    log "Go: bumping all modules to latest (go get -u ./...)"
    go get -u ./...
    go mod tidy
    CHANGED=1
    ok "go.mod/go.sum updated"
    if [ "$SKIP_VERIFY" -eq 0 ]; then
      log "Go: verifying (build + vet + test)"
      go build -o /dev/null ./src
      go vet ./...
      go test ./src -count=1
      ok "go build + vet + test green"
    else
      warn "skipping Go verification (--skip-verify)"
    fi
  fi
fi

# =====================================================================
# 2. Dockerfile base images (+ go2rtc URL check)
# =====================================================================
if [ "$DO_DOCKER" -eq 1 ]; then
  [ -f "$DOCKERFILE" ] || die "Dockerfile not found at $DOCKERFILE"

  bump_docker_from() {
    # $1 = awk pattern identifying the FROM line, $2 = current image,
    # $3 = new image (empty = lookup failed, skip)
    local pattern="$1" current="$2" new="$3"
    if [ -z "$new" ]; then
      warn "could not resolve latest for $current — leaving as-is"
      return 0
    fi
    if [ "$current" = "$new" ]; then
      ok "$current already latest"
      return 0
    fi
    if [ "$CHECK" -eq 1 ]; then
      log "[would update] $current -> $new"
      NEEDS_UPDATE=1
      return 0
    fi
    # Validate tag shape before touching the file.
    if ! printf '%s' "$new" | grep -Eq '^[a-z0-9./-]+:[A-Za-z0-9_.-]+$'; then
      warn "refusing to write suspicious image ref: $new"
      return 0
    fi
    sed -i -E "s|${pattern}|FROM ${new}|" "$DOCKERFILE"
    CHANGED=1
    log "Dockerfile: $current -> $new"
  }

  # --- golang:X-alpine (builder) ---
  CUR_GOLANG="$(grep -m1 -E '^FROM golang:' "$DOCKERFILE" | awk '{print $2}')"
  if [ -n "$CUR_GOLANG" ]; then
    if LATEST_MINOR="$(dockerhub_latest 'library/golang' '^1\.[0-9]+-alpine$')"; then
      bump_docker_from '^FROM golang:[^ ]+' "$CUR_GOLANG" "golang:${LATEST_MINOR}"
    else
      warn "could not resolve latest golang tag — leaving $CUR_GOLANG"
    fi
  fi

  # --- alpine:3.x (go2rtc-fetch) ---
  CUR_ALPINE="$(grep -m1 -E '^FROM alpine:' "$DOCKERFILE" | awk '{print $2}')"
  if [ -n "$CUR_ALPINE" ]; then
    if LATEST_ALPINE="$(dockerhub_latest 'library/alpine' '^3\.[0-9]+$')"; then
      bump_docker_from '^FROM alpine:[^ ]+' "$CUR_ALPINE" "alpine:${LATEST_ALPINE}"
    else
      warn "could not resolve latest alpine tag — leaving $CUR_ALPINE"
    fi
  fi

  # --- docker/dockerfile syntax ---
  CUR_SYNTAX="$(grep -m1 -E '^# syntax=docker/dockerfile:' "$DOCKERFILE" | sed -E 's|.*docker/dockerfile:||')"
  if [ -n "$CUR_SYNTAX" ]; then
    if LATEST_SYNTAX="$(dockerhub_latest 'docker/dockerfile' '^1\.[0-9]+$')"; then
      if [ "$CUR_SYNTAX" != "$LATEST_SYNTAX" ]; then
        if [ "$CHECK" -eq 1 ]; then
          log "[would update] dockerfile syntax $CUR_SYNTAX -> $LATEST_SYNTAX"
          NEEDS_UPDATE=1
        else
          sed -i -E "s|# syntax=docker/dockerfile:[0-9.]+|# syntax=docker/dockerfile:${LATEST_SYNTAX}|" "$DOCKERFILE"
          CHANGED=1
          log "Dockerfile syntax: $CUR_SYNTAX -> $LATEST_SYNTAX"
        fi
      else
        ok "dockerfile syntax $CUR_SYNTAX already latest"
      fi
    else
      warn "could not resolve latest dockerfile syntax — leaving $CUR_SYNTAX"
    fi
  fi

  # --- distroless/static:nonroot (floating tag, nothing to bump) ---
  if grep -Eq '^FROM gcr\.io/distroless/static:nonroot' "$DOCKERFILE"; then
    if command -v docker >/dev/null 2>&1; then
      if [ "$CHECK" -eq 1 ]; then
        log "distroless/static:nonroot is a floating tag — run without --check to pull and validate it"
      else
        log "validating distroless/static:nonroot resolves (docker pull)"
        if docker pull -q gcr.io/distroless/static:nonroot >/dev/null 2>&1; then
          ok "distroless/static:nonroot resolves"
        else
          warn "docker pull gcr.io/distroless/static:nonroot failed — Dockerfile left unchanged"
        fi
      fi
    else
      ok "distroless/static:nonroot (floating tag, no version to bump; docker not installed, skipped pull check)"
    fi
  fi

  # --- go2rtc: validate download URL (pinned tag or latest) ---
  if grep -Eq '^ARG GO2RTC_VERSION=' "$DOCKERFILE"; then
    GO2RTC_VER="$(grep -m1 '^ARG GO2RTC_VERSION=' "$DOCKERFILE" | cut -d= -f2- | tr -d '[:space:]')"
    log "go2rtc pinned at ${GO2RTC_VER} — checking download URL resolves"
    if curl -fsSL --max-time 30 -o /dev/null \
        "https://github.com/AlexxIT/go2rtc/releases/download/${GO2RTC_VER}/go2rtc_linux_amd64"; then
      ok "go2rtc ${GO2RTC_VER} download URL resolves"
    else
      warn "go2rtc ${GO2RTC_VER} download URL did not resolve — pin a valid release (pre-1.9 releases were deleted upstream)"
    fi
  fi

  # --- compose images: report only (already floating) ---
  for img in "eclipse-mosquitto:2" "minio/minio:latest"; do
    if grep -Eq "${img%%:*}" "$ROOT/docker-compose.yml" 2>/dev/null; then
      ok "compose image $img already tracks upstream (nothing to bump)"
    fi
  done
fi

# =====================================================================
# 3. GitHub Actions pins
# =====================================================================
if [ "$DO_ACTIONS" -eq 1 ]; then
  if [ ! -f "$WORKFLOW" ]; then
    warn "workflow not found at $WORKFLOW — skipping"
  else
    log "GitHub Actions: checking pins in $(basename "$WORKFLOW")"
    # Unique owner/repo list parsed from `uses:` lines.
    ACTIONS_REPOS="$(grep -oE 'uses:[[:space:]]+[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@v[0-9]+' "$WORKFLOW" \
      | sed -E 's|uses:[[:space:]]+||; s|@v[0-9]+||' | sort -u)"
    for repo in $ACTIONS_REPOS; do
      cur_ver="$(grep -m1 -E "uses:[[:space:]]+${repo}@v[0-9]+" "$WORKFLOW" | grep -oE '@v[0-9]+' | grep -oE '[0-9]+')"
      latest_tag="$(github_latest_tag "$repo" || true)"
      if [ -z "$latest_tag" ]; then
        warn "$repo: could not resolve latest release (rate limit/offline?) — leaving as-is"
        continue
      fi
      # Normalise "v7" / "7" / "v7.1.0" to major number for comparison.
      latest_major="$(printf '%s' "$latest_tag" | grep -oE '[0-9]+' | head -n1)"
      if [ -z "$latest_major" ]; then
        warn "$repo: unparseable tag $latest_tag — leaving as-is"
        continue
      fi
      if [ "$cur_ver" = "$latest_major" ]; then
        ok "$repo@v$cur_ver already latest"
      elif [ "$latest_major" -gt "$cur_ver" ] 2>/dev/null; then
        if [ "$CHECK" -eq 1 ]; then
          log "[would update] $repo@v$cur_ver -> @v$latest_major (latest tag $latest_tag)"
          NEEDS_UPDATE=1
        else
          # Only bump the major pin (vN), preserving action major-tracking convention.
          sed -i -E "s|(uses:[[:space:]]+${repo}@)v[0-9]+|\1v${latest_major}|g" "$WORKFLOW"
          CHANGED=1
          log "$repo@v$cur_ver -> @v$latest_major (latest tag $latest_tag)"
        fi
      else
        ok "$repo@v$cur_ver newer than remote v$latest_major? — leaving as-is"
      fi
    done
  fi
fi

# =====================================================================
# Summary
# =====================================================================
echo
if [ "$CHECK" -eq 1 ]; then
  if [ "$NEEDS_UPDATE" -eq 1 ]; then
    log "--check: updates are available (see [would update] lines above)"
    exit 1
  fi
  ok "--check: everything already at latest"
  exit 0
fi

if [ "$CHANGED" -eq 1 ]; then
  log "changes:"
  git diff --stat -- go.mod go.sum Dockerfile .github/workflows/ || true
  echo
  log "next steps: docker compose build && curl -u <user>:<pass> -X POST http://localhost:8971/api/test/detection"
else
  ok "nothing changed — everything already at latest"
fi
