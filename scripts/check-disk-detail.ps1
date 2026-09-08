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

Write-Host "=== DELETE LOCAL CONFIG ===" -ForegroundColor Cyan
Get-Content ".env" | Where-Object { $_ -match "delete-local|DELETE_LOCAL|Finalize|finalize" } | ForEach-Object { Write-Host "  $_" }

Write-Host ""
Write-Host "=== LATEST DISK USAGE PER NODE ===" -ForegroundColor Cyan
try {
    $raw = Invoke-RestMethod -Uri "$base/rest/v1/disk_usage?select=node_id,used_bytes,total_bytes,recorded_at&order=recorded_at.desc&limit=200" -Method Get -Headers $h
    $seen = @{}
    foreach ($r in $raw) {
        if (-not $seen.ContainsKey($r.node_id)) { $seen[$r.node_id] = $r }
    }
    foreach ($kv in $seen.GetEnumerator() | Sort-Object { $_.Key }) {
        $d = $kv.Value
        $usedGB = [math]::Round($d.used_bytes / 1GB, 2)
        $totalGB = [math]::Round($d.total_bytes / 1GB, 2)
        $pct = if ($d.total_bytes -gt 0) { [math]::Round(($d.used_bytes / $d.total_bytes) * 100, 1) } else { 0 }
        $color = if ($pct -gt 90) { "Red" } elseif ($pct -gt 75) { "Yellow" } else { "Green" }
        Write-Host "  $($d.node_id): ${usedGB}GB / ${totalGB}GB (${pct}%) at $($d.recorded_at)" -ForegroundColor $color
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

Write-Host ""
Write-Host "=== RECORDINGS TABLE COLUMNS ===" -ForegroundColor Cyan
try {
    $rec = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=*&limit=1" -Method Get -Headers $h
    if ($rec -and $rec.Count -gt 0) {
        $rec[0].PSObject.Properties | ForEach-Object {
            $val = if ($_.Value -is [string] -and $_.Value.Length -gt 60) { $_.Value.Substring(0,60)+"..." } else { $_.Value }
            Write-Host "  $($_.Name): $val"
        }
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

Write-Host ""
Write-Host "=== PIPELINE STATES DETAIL ===" -ForegroundColor Cyan
try {
    $pipes = Invoke-RestMethod -Uri "$base/rest/v1/pipeline_states?select=*&order=created_at.desc&limit=25" -Method Get -Headers $h
    foreach ($p in $pipes) {
        $sizeMB = if ($p.file_size) { [math]::Round($p.file_size / 1MB, 1) } else { "?" }
        $age = ""
        if ($p.created_at) {
            try {
                $created = [DateTime]::Parse($p.created_at)
                $diff = (Get-Date) - $created
                if ($diff.TotalMinutes -lt 60) { $age = "$([int]$diff.TotalMinutes)m" }
                else { $age = "$([int]$diff.TotalHours)h$([int]$diff.Minutes)m" }
            } catch { $age = "?" }
        }
        $fname = if ($p.file_path) { [System.IO.Path]::GetFileName($p.file_path) } else { "?" }
        Write-Host "  $fname size=${sizeMB}MB retries=$($p.retries) stage=$($p.stage) age=$age"
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

Write-Host ""
Write-Host "=== RECORDINGS & LINKS COUNT ===" -ForegroundColor Cyan
try {
    $recs = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=id" -Method Get -Headers $h
    Write-Host "  Total recordings: $($recs.Count)"
} catch { Write-Host "  recordings error: $_" -ForegroundColor Red }
try {
    $links = Invoke-RestMethod -Uri "$base/rest/v1/upload_links?select=id" -Method Get -Headers $h
    Write-Host "  Total upload_links: $($links.Count)"
} catch { Write-Host "  upload_links error: $_" -ForegroundColor Red }

Write-Host ""
Write-Host "=== OLDEST RECORDINGS (potential disk hogs) ===" -ForegroundColor Yellow
try {
    $old = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=username,filename,created_at,size_bytes&order=created_at.asc&limit=15" -Method Get -Headers $h
    foreach ($r in $old) {
        $sizeMB = if ($r.size_bytes) { [math]::Round($r.size_bytes / 1MB, 1) } else { "?" }
        Write-Host "  $($r.username) size=${sizeMB}MB created=$($r.created_at) file=$($r.filename)"
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }
