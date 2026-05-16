#!/bin/sh
apk add --no-cache curl jq
echo '[COOKIE] Starting Cloudflare cookie refresher...'
while true; do
  echo '[COOKIE] Getting cf_clearance from Byparr...'
  RESPONSE=$(curl -s --max-time 120 -X POST http://byparr:8191/v1 \
    -H 'Content-Type: application/json' \
    -d '{"cmd":"request.get","url":"https://chaturbate.com","maxTimeout":60000}')
  CF_COOKIE=$(echo "$RESPONSE" | jq -r '.solution.cookies[] | select(.name=="cf_clearance") | .name + "=" + .value' 2>/dev/null)
  if [ -n "$CF_COOKIE" ]; then
    echo "[COOKIE] Refreshed cf_clearance"
    curl -s --max-time 10 -X POST http://chaturbate-dvr:8080/update_config \
      -H 'Content-Type: application/json' \
      -d "{\"cookies\":\"$CF_COOKIE\"}" > /dev/null 2>&1
    echo '[COOKIE] Pushed to chaturbate-dvr'
  else
    echo '[COOKIE] Failed to get cf_clearance, retrying...'
  fi
  sleep 1800
done
