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

# 1. Detailed failure messages per host (last 50)
Write-Host "=== DETAILED UPLOAD FAILURES (last 50) ===" -ForegroundColor Red
try {
    $failures = Invoke-RestMethod -Uri "$base/rest/v1/upload_journal?select=*&status=eq.failed&order=created_at.desc&limit=50" -Method Get -Headers $h
    $byHost = $failures | Group-Object -Property host
    foreach ($g in $byHost) {
        Write-Host ""
        Write-Host "  $($g.Name) ($($g.Count) failures):" -ForegroundColor Yellow
        $errors = $g.Group | Group-Object -Property error_msg
        foreach ($e in $errors) {
            $samples = $e.Group | Select-Object -First 2
            $sizes = $samples | ForEach-Object { if ($_.file_size) { [math]::Round($_.file_size / 1MB, 1) } else { "?" } }
            Write-Host "    Error: $($e.Name) ($($e.Count)x)" -ForegroundColor Red
            Write-Host "    Sample sizes: $($sizes -join ', ')MB" -ForegroundColor DarkGray
        }
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 2. Success entries that still exist (not cleaned up)
Write-Host ""
Write-Host "=== EXISTING SUCCESS ENTRIES (should be cleaned up) ===" -ForegroundColor Green
try {
    $succ = Invoke-RestMethod -Uri "$base/rest/v1/upload_journal?select=host,status,file_size,created_at&status=eq.success&limit=20" -Method Get -Headers $h
    if ($succ -and $succ.Count -gt 0) {
        foreach ($s in $succ) {
            $sizeMB = if ($s.file_size) { [math]::Round($s.file_size / 1MB, 1) } else { "?" }
            Write-Host "  $($s.host) ${sizeMB}MB created=$($s.created_at)" -ForegroundColor Green
        }
        Write-Host "  Total: $($succ.Count) (should be 0 after cleanup)" -ForegroundColor Yellow
    } else {
        Write-Host "  None found (correct - successes are cleaned up)" -ForegroundColor Green
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 3. Upload links per host
Write-Host ""
Write-Host "=== UPLOAD LINKS BY HOST ===" -ForegroundColor Cyan
try {
    $links = Invoke-RestMethod -Uri "$base/rest/v1/upload_links?select=host" -Method Get -Headers $h
    $groups = $links | Group-Object -Property host
    foreach ($g in $groups) {
        Write-Host "  $($g.Name): $($g.Count) links" -ForegroundColor Green
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 4. Check if recordings with embed_url have upload_links
Write-Host ""
Write-Host "=== RECORDINGS WITH EMBED BUT NO LINKS (metadata saved, links lost?) ===" -ForegroundColor Yellow
try {
    $recs = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=id,username,embed_url&embed_url=not.is.null&limit=10" -Method Get -Headers $h
    foreach ($r in $recs) {
        try {
            $links = Invoke-RestMethod -Uri "$base/rest/v1/upload_links?select=id&recording_id=eq.$($r.id)" -Method Get -Headers $h
            $linkCount = $links.Count
            $color = if ($linkCount -gt 0) { "Green" } else { "Red" }
            Write-Host "  $($r.username) links=$linkCount embed=$($r.embed_url.Substring(0, [Math]::Min(50, $r.embed_url.Length)))..." -ForegroundColor $color
        } catch {
            Write-Host "  $($r.username) link check failed" -ForegroundColor Yellow
        }
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 5. Pipeline states with more detail
Write-Host ""
Write-Host "=== PIPELINE STATES - FULL DETAIL ===" -ForegroundColor Cyan
try {
    $pipes = Invoke-RestMethod -Uri "$base/rest/v1/pipeline_states?select=*&order=created_at.desc&limit=5" -Method Get -Headers $h
    foreach ($p in $pipes) {
        Write-Host "  ---"
        $p.PSObject.Properties | ForEach-Object {
            $val = if ($_.Value -is [string] -and $_.Value.Length -gt 80) { $_.Value.Substring(0,80)+"..." } else { $_.Value }
            Write-Host "    $($_.Name): $val"
        }
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 6. Disk usage trend - check if it's growing
Write-Host ""
Write-Host "=== DISK USAGE READING (node-9 specifically) ===" -ForegroundColor Cyan
try {
    $disk9 = Invoke-RestMethod -Uri "$base/rest/v1/disk_usage?select=*&node_id=eq.node-9&order=recorded_at.desc&limit=5" -Method Get -Headers $h
    foreach ($d in $disk9) {
        $usedGB = [math]::Round($d.used_bytes / 1GB, 2)
        Write-Host "  $usedGBGB at $($d.recorded_at)"
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }
