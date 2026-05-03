# Verify channels in Supabase
# PowerShell version

param(
    [string]$SupabaseUrl = $env:SUPABASE_URL,
    [string]$SupabaseApiKey = $env:SUPABASE_API_KEY
)

$ErrorActionPreference = "Stop"

# Read from settings.json if not provided
if ([string]::IsNullOrEmpty($SupabaseUrl) -or [string]::IsNullOrEmpty($SupabaseApiKey)) {
    if (Test-Path "settings.json") {
        $settings = Get-Content "settings.json" | ConvertFrom-Json
        $SupabaseUrl = $settings.supabase_url
        $SupabaseApiKey = $settings.supabase_api_key
    }
}

if ([string]::IsNullOrEmpty($SupabaseUrl) -or [string]::IsNullOrEmpty($SupabaseApiKey)) {
    Write-Host "[ERROR] Supabase credentials not set" -ForegroundColor Red
    exit 1
}

Write-Host "[INFO] Fetching channels from Supabase..." -ForegroundColor Cyan

try {
    $headers = @{
        "apikey" = $SupabaseApiKey
        "Authorization" = "Bearer $SupabaseApiKey"
    }
    
    $response = Invoke-RestMethod -Uri "$SupabaseUrl/rest/v1/channels?select=*&order=username.asc" `
        -Method Get `
        -Headers $headers `
        -ErrorAction Stop
    
    Write-Host "`n[SUCCESS] Found $($response.Count) channels in Supabase:" -ForegroundColor Green
    Write-Host "=" * 80
    
    foreach ($channel in $response) {
        $status = if ($channel.is_paused) { "PAUSED" } else { "ACTIVE" }
        $statusColor = if ($channel.is_paused) { "Yellow" } else { "Green" }
        
        Write-Host "Username: " -NoNewline
        Write-Host $channel.username -ForegroundColor Cyan -NoNewline
        Write-Host " | Site: " -NoNewline
        Write-Host $channel.site -ForegroundColor White -NoNewline
        Write-Host " | Status: " -NoNewline
        Write-Host $status -ForegroundColor $statusColor -NoNewline
        Write-Host " | Resolution: " -NoNewline
        Write-Host "$($channel.resolution)p@$($channel.framerate)fps" -ForegroundColor Magenta
    }
    
    Write-Host "=" * 80
    
    # Summary
    $activeCount = ($response | Where-Object { -not $_.is_paused }).Count
    $pausedCount = ($response | Where-Object { $_.is_paused }).Count
    
    Write-Host "`n[SUMMARY]" -ForegroundColor Cyan
    Write-Host "Total Channels: $($response.Count)" -ForegroundColor White
    Write-Host "Active: $activeCount" -ForegroundColor Green
    Write-Host "Paused: $pausedCount" -ForegroundColor Yellow
}
catch {
    Write-Host "[ERROR] Failed to fetch channels: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
