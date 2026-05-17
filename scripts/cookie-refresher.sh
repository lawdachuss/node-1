#!/bin/sh
apk add --no-cache curl jq
echo '[COOKIE] Starting Cloudflare cookie refresher...'
until curl -fsS --max-time 5 http://byparr-lb/backend-health > /dev/null 2>&1; do
  echo '[COOKIE] Waiting for Byparr backend...'
  sleep 5
done

while true; do
  echo "[COOKIE] Getting cf_clearance from Byparr... ($(date -u +%H:%M:%S))"
  RESPONSE=$(curl -sS --fail --max-time 90 -X POST http://byparr-lb/v1 \
    -H 'Content-Type: application/json' \
    -d '{"cmd":"request.get","url":"https://chaturbate.com","maxTimeout":60000}')
  CF_COOKIE=$(echo "$RESPONSE" | jq -r '.solution.cookies[] | select(.name=="cf_clearance" or .name=="csrftoken") | .name + "=" + .value' 2>/dev/null | paste -sd ';' -)
  CF_USER_AGENT=$(echo "$RESPONSE" | jq -r '.solution.userAgent // empty' 2>/dev/null)
  if [ -n "$CF_COOKIE" ]; then
    echo "[COOKIE] Refreshed cookies (cf_clearance + csrftoken when present)"
    if [ -n "$CF_USER_AGENT" ]; then
      body=$(jq -n --arg cookies "$CF_COOKIE" --arg ua "$CF_USER_AGENT" '{cookies:$cookies, user_agent:$ua}')
    else
      body=$(jq -n --arg cookies "$CF_COOKIE" '{cookies:$cookies}')
    fi
    HTTP_CODE=$(curl -sS -o /tmp/cookie-push.json -w '%{http_code}' --max-time 15 -X POST http://chaturbate-dvr:8080/update_config \
      -H 'Content-Type: application/json' \
      -d "$body")
    if [ "$HTTP_CODE" = "200" ]; then
      echo '[COOKIE] Pushed to chaturbate-dvr (ok)'
    else
      echo "[COOKIE] Failed to push cookies (HTTP $HTTP_CODE)"
      cat /tmp/cookie-push.json 2>/dev/null || true
    fi
  else
    echo '[COOKIE] Failed to get cf_clearance, retrying...'
  fi
  sleep 1800
done
