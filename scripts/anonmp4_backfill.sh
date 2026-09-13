#!/usr/bin/env bash
# anonmp4_backfill.sh — AnonMP4 thumbnail backfill sweep (GitHub Actions + local).
#
# 1. Queries Supabase for recordings still missing thumbnail_url that have an
#    AnonMP4 embed mirror (the only recoverable host for those rows).
# 2. Captures each embed's /video-api payload through headless Chrome (CDP),
#    since the payload is signed at runtime and rejects plain curl.
# 3. For videos whose render has finished ("hls" present), builds a backfill
#    manifest and runs cmd/backfillvoe, which re-hosts thumb/preview/sprite to
#    Pixhost and PATCHes recordings + preview_images.
#
# Skips fast (single Supabase query) when there is nothing to backfill, so a
# 15-minute cron job is effectively free outside active retries.
#
# Env: SUPABASE_URL, SUPABASE_API_KEY, SUPABASE_SERVICE_ROLE_KEY (required).
#      CHROME_BIN (optional; auto-detects google-chrome/chromium).
set -euo pipefail
cd "$(dirname "$0")/.."

: "${SUPABASE_URL:?SUPABASE_URL required}"
: "${SUPABASE_SERVICE_ROLE_KEY:?SUPABASE_SERVICE_ROLE_KEY required}"

SB="${SUPABASE_URL%/}"
RECS="${RUNNER_TEMP:-$(mktemp -d)}/anonmp4"
mkdir -p "$RECS"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

# ---- 1. Find missing-thumbnail recordings with an AnonMP4 mirror ----
ROWS="$(curl -fsS --max-time 120 \
  -H "apikey: $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY" \
  "$SB/rest/v1/recordings?thumbnail_url=is.null&select=filename,upload_links(url,host)" 2>&1)" || { log "DB query failed: $ROWS"; exit 1; }

ENTRIES="$(printf '%s' "$ROWS" | jq -c '
  [ .[] | . as $r
    | [ $r.upload_links[]? | select((.host|ascii_downcase|contains("anonmp4")) and (.url|contains("embed/"))) ] as $an
    | select($an|length>0)
    | { filename: $r.filename,
        url: ("https://anonmp4.help/embed/" + ($an[0].url | capture("embed/(?<c>[A-Za-z0-9]+)").c)) }
  ]')"

N="$(printf '%s' "$ENTRIES" | jq 'length')"
log "missing + AnonMP4 mirror: $N"
[[ "$N" == "0" ]] && exit 0

printf '%s' "$ENTRIES" > "$RECS/anon_entries.json"
log "entries -> $RECS/anon_entries.json"

# ---- 2. Headless Chrome CDP capture ----
CHROME="${CHROME_BIN:-$(command -v google-chrome chromium chromium-browser 2>/dev/null | head -n1 || true)}"
if [[ -z "$CHROME" ]]; then
  log "ERROR: no Chrome/Chromium found; set CHROME_BIN"; exit 1
fi

rm -rf "$RECS/profile"; mkdir -p "$RECS/profile"
CHPID=""
cleanup() { [[ -n "$CHPID" ]] && kill "$CHPID" 2>/dev/null || true; }
trap cleanup EXIT

"$CHROME" --headless=new --disable-gpu --no-sandbox --disable-dev-shm-usage \
  --user-data-dir="$RECS/profile" --remote-debugging-port=9222 about:blank >/dev/null 2>&1 &
CHPID=$!
sleep 4

node scripts/anonmp4_capture.js "$RECS/anon_entries.json" "$RECS/anon_caps.json"
cleanup; trap - EXIT

# ---- 3. Build manifest from ready captures and backfill ----
READY="$(jq -c '
  [ .[] | . as $e
    | [ $e.bodies[]? | select((.url|contains("video-api")) and (.body|contains("\"hls\""))) | .body ] as $ok
    | select($ok|length>0)
    | ( $ok[0] | fromjson ) as $api
    | { filename: $e.filename,
        source: $api.hls,
        duration: ($api.duration | tonumber),
        thumb: $api.thumbnail,
        preview: $api.thumbnail,
        sprite: ($api.preview_thums[0].url // "") }
  ]' "$RECS/anon_caps.json")"

NR="$(printf '%s' "$READY" | jq 'length')"
NSTILL="$(jq -c '[.[] | .filename] | length' "$RECS/anon_entries.json")"
log "ready to backfill: $NR (still queued: $((NSTILL - NR)))"
[[ "$NR" == "0" ]] && exit 0

printf '%s' "$READY" > "$RECS/anon_manifest.json"
log "backfilling via cmd/backfillvoe..."
go run ./cmd/backfillvoe "$RECS/anon_manifest.json"