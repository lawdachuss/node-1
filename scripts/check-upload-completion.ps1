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

# 1. Recordings WITHOUT upload links (recordings row exists but no links = never uploaded)
Write-Host "=== RECORDINGS WITHOUT UPLOAD LINKS (never uploaded) ===" -ForegroundColor Red
try {
    # Use an RPC or raw query to find recordings with no links
    # PostgREST doesn't support LEFT JOIN, so we'll check via embed
    $noLinks = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=id,username,filename,created_at,filesize&upload_links=*)&order=created_at.desc&limit=50" -Method Get -Headers $h
    if ($noLinks -and $noLinks.Count -gt 0) {
        foreach ($r in $noLinks) {
            $sizeMB = if ($r.filesize) { [math]::Round($r.filesize / 1MB, 1) } else { "?" }
            Write-Host "  $($r.username) size=${sizeMB}MB created=$($r.created_at) file=$($r.filename)" -ForegroundColor Yellow
        }
        Write-Host "  Count: $($noLinks.Count)" -ForegroundColor Yellow
    } else {
        Write-Host "  None found via embed query" -ForegroundColor Green
    }
} catch {
    Write-Host "  Embed query failed: $($_.Exception.Message)" -ForegroundColor Yellow
}

# 2. Upload journal success vs failure ratio
Write-Host ""
Write-Host "=== UPLOAD JOURNAL STATUS ===" -ForegroundColor Cyan
try {
    $uj = Invoke-RestMethod -Uri "$base/rest/v1/upload_journal?select=status" -Method Get -Headers $h
    $total = $uj.Count
    $success = ($uj | Where-Object { $_.status -eq "success" }).Count
    $failed = ($uj | Where-Object { $_.status -eq "failed" }).Count
    $other = $total - $success - $failed
    Write-Host "  Total journal entries: $total"
    Write-Host "  Success: $success ($([math]::Round($success/$total*100,1))%)" -ForegroundColor Green
    Write-Host "  Failed: $failed ($([math]::Round($failed/$total*100,1))%)" -ForegroundColor Red
    if ($other -gt 0) { Write-Host "  Other/pending: $other" -ForegroundColor Yellow }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 3. Upload journal failures by host
Write-Host ""
Write-Host "=== FAILURES BY HOST ===" -ForegroundColor Red
try {
    $failures = Invoke-RestMethod -Uri "$base/rest/v1/upload_journal?select=host&status=eq.failed" -Method Get -Headers $h
    $groups = $failures | Group-Object -Property host
    foreach ($g in $groups) {
        Write-Host "  $($g.Name): $($g.Count) failures" -ForegroundColor Red
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 4. Upload journal successes by host
Write-Host ""
Write-Host "=== SUCCESSES BY HOST ===" -ForegroundColor Green
try {
    $successes = Invoke-RestMethod -Uri "$base/rest/v1/upload_journal?select=host&status=eq.success" -Method Get -Headers $h
    $groups = $successes | Group-Object -Property host
    foreach ($g in $groups) {
        Write-Host "  $($g.Name): $($g.Count) uploads" -ForegroundColor Green
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 5. Check a sample of recent recordings to see if they have links
Write-Host ""
Write-Host "=== SAMPLE: 20 RECENT RECORDINGS WITH LINK STATUS ===" -ForegroundColor Cyan
try {
    $recs = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=id,username,filename,created_at,filesize,embed_url&order=created_at.desc&limit=20" -Method Get -Headers $h
    foreach ($r in $recs) {
        $hasEmbed = if ($r.embed_url) { "YES" } else { "NO" }
        $hasLinks = "?"  # Can't tell from recordings alone
        $sizeMB = if ($r.filesize) { [math]::Round($r.filesize / 1MB, 1) } else { "?" }
        # Check if upload_links exist for this recording
        try {
            $links = Invoke-RestMethod -Uri "$base/rest/v1/upload_links?select=id&recording_id=eq.$($r.id)" -Method Get -Headers $h
            $linkCount = $links.Count
            $linkColor = if ($linkCount -gt 0) { "Green" } else { "Red" }
            Write-Host "  $($r.username) ${sizeMB}MB embed=$hasEmbed links=$linkCount created=$($r.created_at)" -ForegroundColor $linkColor
        } catch {
            Write-Host "  $($r.username) ${sizeMB}MB embed=$hasEmbed links=?? created=$($r.created_at)" -ForegroundColor Yellow
        }
    }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 6. Recordings with embed_url (uploaded to at least VOE) vs without
Write-Host ""
Write-Host "=== RECORDINGS WITH/WITHOUT EMBED_URL ===" -ForegroundColor Cyan
try {
    $withEmbed = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=id&embed_url=not.is.null" -Method Get -Headers $h
    $withoutEmbed = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=id&embed_url=is.null" -Method Get -Headers $h
    Write-Host "  With embed_url (uploaded): $($withEmbed.Count)" -ForegroundColor Green
    Write-Host "  Without embed_url (NOT uploaded): $($withoutEmbed.Count)" -ForegroundColor Red
} catch { Write-Host "  Error: $_" -ForegroundColor Red }

# 7. Oldest recordings without embed (potential stuck files)
Write-Host ""
Write-Host "=== OLDEST RECORDINGS WITHOUT EMBED (potential stuck) ===" -ForegroundColor Yellow
try {
    $stuck = Invoke-RestMethod -Uri "$base/rest/v1/recordings?select=username,filename,created_at,filesize&embed_url=is.null&order=created_at.asc&limit=15" -Method Get -Headers $h
    foreach ($r in $stuck) {
        $sizeMB = if ($r.filesize) { [math]::Round($r.filesize / 1MB, 1) } else { "?" }
        Write-Host "  $($r.username) ${sizeMB}MB created=$($r.created_at) file=$($r.filename)" -ForegroundColor Yellow
    }
    if (-not $stuck -or $stuck.Count -eq 0) { Write-Host "  None - all recordings have embed_url" -ForegroundColor Green }
} catch { Write-Host "  Error: $_" -ForegroundColor Red }
