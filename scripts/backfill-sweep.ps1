# backfill-sweep.ps1 — thumbnail backfill running in the background of the RDP
# session (replaces the deleted .github/workflows/backfill.yml 15-min cron).
#
# Both sweeps run on a loop every BACKFILL_INTERVAL_MIN minutes (default 15)
# until the session deadline (START_TIME + 355min, same as the DVR's
# RUN_DEADLINE) so the sweep never competes with the final upload drain:
#
#   1. AnonMP4 sweep — recordings missing a thumbnail_url that have an AnonMP4
#      embed mirror. A headless Chrome on 127.0.0.1:9222 (CDP) passively
#      captures each embed's /video-api payload (signed at runtime, rejects
#      plain curl) via scripts/anonmp4_capture.js, then cmd/backfillvoe
#      re-hosts the thumb/preview/sprite to Pixhost and PATCHes the rows.
#   2. Remote-thumb sweep — cmd/backfillremotethumbs recovers Vidara og:image
#      (and Streamtape where still live) thumbnails, made runner-safe by
#      CATBOX_PROXY_URL.
#   3. Sync-thumbs sweep — cmd/syncthumbs copies the thumbnail/sprite/preview
#      URLs from preview_images onto recordings rows whose thumbnail_url is NULL
#      (pipelines that wedged at thumbnail_upload never reached save_metadata).
#      Pure DB sync: idempotent, no image-host uploads.
#
# This is the Windows-runner equivalent of scripts/anonmp4_backfill.sh (bash +
# jq), reimplemented with curl.exe / PowerShell JSON / node / go run — all
# present on the GitHub windows-latest image. The fast path (nothing to
# backfill) is a single Supabase query, so the loop is ~free when idle.
#
# Env: SUPABASE_URL, SUPABASE_SERVICE_ROLE_KEY (required for AnonMP4),
#      SUPABASE_API_KEY (required for backfillremotethumbs), REPO_DIR,
#      START_TIME, RUN_DEADLINE, CATBOX_PROXY_URL, CHROME_BIN (optional).
#      BACKFILL_RUNNER=false disables the sweep entirely.
$ErrorActionPreference = 'Continue'

$repoDir = $env:REPO_DIR
if (-not $repoDir) { $repoDir = (Get-Location).Path }
Set-Location $repoDir

function Bf-Log {
  param([string]$m)
  Write-Host ("[{0}] {1}" -f (Get-Date -Format 'HH:mm:ss'), $m)
  $null = [System.Console]::Out.Flush()
}

# Keep the single-repo guard from the old cron: default ON, opt out per repo
# by setting BACKFILL_RUNNER=false so parallel RDP sessions don't all hammer
# AnonMP4/Pixhost at once.
if ($env:BACKFILL_RUNNER -eq 'false') {
  Bf-Log "disabled (BACKFILL_RUNNER=$env:BACKFILL_RUNNER) - exiting"
  exit 0
}

$start = $null
if ($env:START_TIME) { $start = [int64]$env:START_TIME }
if (-not $start -or $start -le 0) { $start = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() }
$deadline = $null
if ($env:RUN_DEADLINE) { $deadline = [int64]$env:RUN_DEADLINE }
if (-not $deadline -or $deadline -le $start) { $deadline = $start + (355 * 60) }

$intervalMin = 15
if ($env:BACKFILL_INTERVAL_MIN) {
  $try = 0
  if ([int]::TryParse($env:BACKFILL_INTERVAL_MIN, [ref]$try) -and $try -ge 5 -and $try -le 120) { $intervalMin = $try }
}

$work = if ($env:TEMP) { Join-Path $env:TEMP 'anonmp4' } else { Join-Path $repoDir 'tmp\anonmp4' }
New-Item -ItemType Directory -Force -Path $work | Out-Null

# Write-JsonArray serializes $Data as a JSON ARRAY even when it holds a single
# item.  Piping to ConvertTo-Json unwraps a one-element array into a bare
# object, and anonmp4_capture.js then crashes on "entries is not iterable" —
# the AnonMP4 sweep silently no-oped for the exact case that matters (a node
# with a one-file backlog).  -InputObject keeps array-ness; the .NET writer is
# BOM-free on both PowerShell 5.1 and pwsh 7 (Set-Content -Encoding utf8 mints
# a UTF-8 BOM under 5.1 that JSON.parse rejects).
function Write-JsonArray {
  param([string]$Path, [object]$Data, [int]$Depth = 5)
  $json = ConvertTo-Json -InputObject @($Data) -Depth $Depth
  [System.IO.File]::WriteAllText($Path, $json, (New-Object System.Text.UTF8Encoding($false)))
}

function Resolve-Chrome {
  if ($env:CHROME_BIN -and (Test-Path $env:CHROME_BIN)) { return $env:CHROME_BIN }
  $cands = @(
    'C:\chrome146\chrome-win64\chrome.exe',
    "$env:ProgramFiles\Google\Chrome\Application\chrome.exe",
    "$env:ProgramFiles(x86)\Microsoft\Edge\Application\msedge.exe"
  )
  foreach ($c in $cands) { if ($c -and (Test-Path $c)) { return $c } }
  $gc = Get-Command chrome, msedge -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($gc) { return $gc.Source }
  return $null
}

function Invoke-AnonMp4 {
  param([string]$dir)
  $sb = $env:SUPABASE_URL
  $key = $env:SUPABASE_SERVICE_ROLE_KEY
  if (-not $sb -or [string]::IsNullOrWhiteSpace($key)) {
    Bf-Log 'anonmp4: SUPABASE_URL / SERVICE_ROLE_KEY missing - skipping'
    return
  }
  $sb = $sb.TrimEnd('/')

  $rows = & curl.exe -fsS --max-time 120 -H "apikey: $key" -H "Authorization: Bearer $key" "$sb/rest/v1/recordings?thumbnail_url=is.null&select=filename,upload_links(url,host)" 2>&1
  if ($LASTEXITCODE -ne 0 -or -not $rows) {
    Bf-Log "anonmp4: DB query failed (exit $LASTEXITCODE) - skipping"
    return
  }
  if ($rows -is [array]) { $rows = $rows -join '' }
  try { $recs = $rows | ConvertFrom-Json } catch { Bf-Log "anonmp4: parse rows failed: $_"; return }

  $entries = @()
  foreach ($r in $recs) {
    if (-not $r.upload_links) { continue }
    $an = @($r.upload_links | Where-Object {
      $_ -and $_.host -and $_.url -and
      $_.host.ToLower().Contains('anonmp4') -and
      $_.url.Contains('embed/')
    })
    if ($an.Count -eq 0) { continue }
    if ($an[0].url -notmatch 'embed/([A-Za-z0-9]+)') { continue }
    $entries += [pscustomobject]@{
      filename = $r.filename
      url      = "https://anonmp4.help/embed/$($matches[1])"
    }
  }
  Bf-Log "anonmp4: missing + AnonMP4 mirror: $($entries.Count)"
  if ($entries.Count -eq 0) { return }

  $entriesJson = Join-Path $dir 'anon_entries.json'
  Write-JsonArray $entriesJson $entries 5

  $chrome = Resolve-Chrome
  if (-not $chrome) { Bf-Log 'anonmp4: no Chrome/Chromium found (set CHROME_BIN) - skipping'; return }
  $profile = Join-Path $dir 'profile'
  New-Item -ItemType Directory -Force -Path $profile | Out-Null

  $chromeP = $null
  try {
    $chromeP = Start-Process -FilePath $chrome -ArgumentList @(
      '--headless=new', '--disable-gpu', '--no-sandbox', '--disable-dev-shm-usage',
      "--user-data-dir=$profile", '--remote-debugging-port=9222', 'about:blank'
    ) -WindowStyle Hidden -PassThru `
      -RedirectStandardOutput (Join-Path $dir 'chrome.out.log') `
      -RedirectStandardError (Join-Path $dir 'chrome.err.log')
  } catch { Bf-Log "anonmp4: chrome launch failed: $_" }
  Start-Sleep -Seconds 6

  $capsJson = Join-Path $dir 'anon_caps.json'
  try {
    & node scripts/anonmp4_capture.js $entriesJson $capsJson
  } catch {
    Bf-Log "anonmp4: capture failed: $_"
  }
  if ($chromeP) { try { $chromeP.Kill() } catch {} }

  if (-not (Test-Path $capsJson)) { Bf-Log 'anonmp4: no captures produced - skipping backfill'; return }
  try { $caps = (Get-Content $capsJson -Raw) | ConvertFrom-Json } catch { Bf-Log "anonmp4: parse caps failed: $_"; return }

  $ready = @()
  foreach ($e in $caps) {
    if (-not $e.bodies) { continue }
    $ok = @($e.bodies | Where-Object {
      $_ -and $_.url -and $_.body -and
      $_.url.Contains('video-api') -and
      $_.body.Contains('"hls"')
    })
    if ($ok.Count -eq 0) { continue }
    try {
      $api = $ok[0].body | ConvertFrom-Json
      $sprite = ''
      if ($api.preview_thums -and $api.preview_thums.Count -gt 0 -and $api.preview_thums[0].url) { $sprite = [string]$api.preview_thums[0].url }
      $ready += [pscustomobject]@{
        filename = $e.filename
        source   = [string]$api.hls
        duration = [double]$api.duration
        thumb    = [string]$api.thumbnail
        preview  = [string]$api.thumbnail
        sprite   = $sprite
      }
    } catch { }
  }
  Bf-Log "anonmp4: ready to backfill: $($ready.Count) (still queued: $(($entries.Count - $ready.Count)))"
  if ($ready.Count -eq 0) { return }

  $manifest = Join-Path $dir 'anon_manifest.json'
  Write-JsonArray $manifest $ready 10
  Bf-Log 'anonmp4: backfilling via cmd/backfillvoe...'
  & go run ./cmd/backfillvoe $manifest
  Bf-Log "anonmp4: backfillvoe exit $LASTEXITCODE"
}

function Invoke-RemoteThumb {
  Bf-Log 'remote-thumb: running cmd/backfillremotethumbs...'
  & go run ./cmd/backfillremotethumbs
  Bf-Log "remote-thumb: exit $LASTEXITCODE"
}

# 3. Sync-thumbs sweep — cmd/syncthumbs copies the thumbnail/sprite/preview URLs
#    from preview_images onto the recordings row where recordings.thumbnail_url is
#    still NULL (the pipeline that owned the file wedged at thumbnail_upload and
#    never reached save_metadata). Pure DB sync, no regeneration or image-host
#    uploads, so it is idempotent and cheap when nothing is missing. Needs
#    SUPABASE_URL + SUPABASE_API_KEY (already in the step env) and reads .env
#    from the repo dir as fallback. go run is fine: the source ships with the
#    repo and the other sweeps compile the same way.
function Invoke-SyncThumbs {
  Bf-Log 'syncthumbs: running cmd/syncthumbs...'
  & go run ./cmd/syncthumbs
  Bf-Log "syncthumbs: exit $LASTEXITCODE"
}

$cycle = 0
$next = $start
while ($true) {
  $now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
  if ($now -ge $deadline) { Bf-Log 'reached session deadline - stopping backfill loop'; break }
  $cycle++
  Bf-Log "backfill cycle $cycle starting (elapsed $([Math]::Round(($now - $start) / 60, 1))m)"
  try { Invoke-AnonMp4 $work } catch { Bf-Log "anonmp4 sweep failed: $_" }
  try { Invoke-RemoteThumb } catch { Bf-Log "remote-thumb sweep failed: $_" }
  try { Invoke-SyncThumbs } catch { Bf-Log "syncthumbs sweep failed: $_" }

  $now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
  $remaining = $deadline - $now
  $sleepSec = [Math]::Min($intervalMin * 60, $remaining)
  if ($sleepSec -le 0) { break }
  Bf-Log "cycle $cycle complete - sleeping ${intervalMin}m"
  Start-Sleep -Seconds $sleepSec
}