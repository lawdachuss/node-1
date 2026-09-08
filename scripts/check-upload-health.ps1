# Check upload pipeline health - disk usage, upload status, stuck files
$ErrorActionPreference = "Stop"

if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        if ($_ -match '^\s*([^#][^=]+)=(.*)$') {
            [Environment]::SetEnvironmentVariable($matches[1].Trim(), $matches[2].Trim().Trim('"').Trim("'"), "Process")
        }
    }
}

$h = @{ "apikey" = $env:SUPABASE_API_KEY; "Authorization" = "Bearer $env:SUPABASE_API_KEY"; "Content-Type" = "application/json" }
$base = $env:SUPABASE_URL

# 1. Disk usage per node
Write-Host "=== DISK USAGE PER NODE ===" -ForegroundColor Cyan
try {
    $disk = Invoke-RestMethod -Uri "$base/rest/v1/disk_usage?select=*&order=node_id.asc" -Method Get -Headers $h
    foreach ($d in $disk) {
        $usedGB = [math]::Round($d.used_bytes / 1GB, 2)
        $totalGB = [math]::Round($d.total_bytes / 1GB, 2)
        $pct = if ($d.total_bytes -gt 0) { [math]::Round(($d.used_bytes / $d.total_bytes) * 100, 1) } else { 0 }
        $color = if ($pct -gt 90) { "Red" } elseif ($pct -gt 75) { "Yellow" } else { "Green" }
        Write-Host "  $($d.node_id): ${usedGB}GB / ${totalGB}GB (${pct}%)" -ForegroundColor $color
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 2. Upload journal summary
Write-Host ""
Write-Host "=== UPLOAD JOURNAL ===" -ForegroundColor Cyan
try {
    $uj = Invoke-RestMethod -Uri "$base/rest/v1/upload_journal?select=status,count=*&order=created_at.desc&limit=500" -Method Get -Headers $h
    $groups = $uj | Group-Object -Property status
    foreach ($g in $groups) {
        Write-Host "  $($g.Name): $($g.Count)" -ForegroundColor $(switch ($g.Name) { "success" { "Green" } "failed" { "Red" } default { "Yellow" } })
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 3. Upload journal - recent failures
Write-Host ""
Write-Host "=== RECENT UPLOAD FAILURES ===" -ForegroundColor Red
try {
    $failures = Invoke-RestMethod -Uri "$base/rest/v1/upload_journal?select=*&status=eq.failed&order=created_at.desc&limit=20" -Method Get -Headers $h
    foreach ($f in $failures) {
        $sizeMB = if ($f.file_size) { [math]::Round($f.file_size / 1MB, 1) } else { "?" }
        Write-Host "  $($f.file_hash.Substring(0,8))... host=$($f.host) size=${sizeMB}MB error=$($f.error_msg) created=$($f.created_at)" -ForegroundColor Red
    }
    if (-not $failures -or $failures.Count -eq 0) { Write-Host "  None found" -ForegroundColor Green }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 4. Recordings by upload_status
Write-Host ""
Write-Host "=== RECORDINGS BY UPLOAD STATUS ===" -ForegroundColor Cyan
try {
    $recs = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=upload_status,node_id" -Method Get -Headers $h
    $total = $recs.Count
    $groups = $recs | Group-Object -Property upload_status
    Write-Host "  Total recordings: $total"
    foreach ($g in $groups) {
        $label = if ($g.Name) { $g.Name } else { "(null)" }
        $color = switch ($g.Name) { "uploaded" { "Green" } "failed" { "Red" } default { "Yellow" } }
        Write-Host "  ${label}: $($g.Count)" -ForegroundColor $color
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 5. Recordings without upload links (stuck on disk)
Write-Host ""
Write-Host "=== RECORDINGS WITHOUT UPLOAD LINKS (potential disk hogs) ===" -ForegroundColor Yellow
try {
    # Get recordings that have no upload_links
    $noLinks = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=username,node_id,size_bytes,created_at,upload_status&upload_status=is.null&order=size_bytes.desc&limit=20" -Method Get -Headers $h
    foreach ($r in $noLinks) {
        $sizeMB = if ($r.size_bytes) { [math]::Round($r.size_bytes / 1MB, 1) } else { "?" }
        Write-Host "  $($r.username) ($($r.node_id)) size=${sizeMB}MB created=$($r.created_at)" -ForegroundColor Yellow
    }
    if (-not $noLinks -or $noLinks.Count -eq 0) { Write-Host "  None found - all recordings have links" -ForegroundColor Green }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 6. Failed-upload recordings (stuck on disk)
Write-Host ""
Write-Host "=== FAILED UPLOAD RECORDINGS ===" -ForegroundColor Red
try {
    $failedRecs = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=username,node_id,size_bytes,created_at,upload_status&upload_status=eq.failed&order=size_bytes.desc&limit=20" -Method Get -Headers $h
    foreach ($r in $failedRecs) {
        $sizeMB = if ($r.size_bytes) { [math]::Round($r.size_bytes / 1MB, 1) } else { "?" }
        Write-Host "  $($r.username) ($($r.node_id)) size=${sizeMB}MB created=$($r.created_at)" -ForegroundColor Red
    }
    if (-not $failedRecs -or $failedRecs.Count -eq 0) { Write-Host "  None found" -ForegroundColor Green }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 7. Active pipeline states (files currently being processed)
Write-Host ""
Write-Host "=== ACTIVE PIPELINE STATES (files in flight) ===" -ForegroundColor Cyan
try {
    $pipes = Invoke-RestMethod -Uri "$base/rest/v1/pipeline_states?select=*&order=created_at.desc&limit=30" -Method Get -Headers $h
    $groups = $pipes | Group-Object -Property stage
    foreach ($g in $groups) {
        Write-Host "  Stage $($g.Name): $($g.Count) files" -ForegroundColor White
    }
    foreach ($p in $pipes) {
        $sizeMB = if ($p.file_size) { [math]::Round($p.file_size / 1MB, 1) } else { "?" }
        Write-Host "  $($p.file_path) stage=$($p.stage) retries=$($p.retries) size=${sizeMB}MB" -ForegroundColor DarkGray
    }
    if (-not $pipes -or $pipes.Count -eq 0) { Write-Host "  No active pipelines" -ForegroundColor Green }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 8. Check DeleteLocalAfterUpload setting
Write-Host ""
Write-Host "=== APP SETTINGS (delete config) ===" -ForegroundColor Cyan
try {
    $settings = Invoke-RestMethod -Uri "$base/rest/v1/app_settings?select=*&limit=5" -Method Get -Headers $h
    foreach ($s in $settings) {
        Write-Host "  Key: $($s.key)" -ForegroundColor White
        if ($s.value -is [string] -and $s.value.Length -lt 500) {
            Write-Host "  Value: $($s.value)" -ForegroundColor DarkGray
        } else {
            Write-Host "  Value: (large object)" -ForegroundColor DarkGray
        }
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }
