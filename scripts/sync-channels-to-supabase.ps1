# Sync channels from conf/channels.json to Supabase
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
    Write-Host "[WARN] Supabase credentials not set, skipping channel sync" -ForegroundColor Yellow
    exit 0
}

$ChannelsFile = "conf/channels.json"
if (-not (Test-Path $ChannelsFile)) {
    Write-Host "[WARN] channels.json not found, skipping sync" -ForegroundColor Yellow
    exit 0
}

Write-Host "[INFO] Syncing channels to Supabase..." -ForegroundColor Cyan

# Read channels
$channels = Get-Content $ChannelsFile | ConvertFrom-Json

$successCount = 0
$errorCount = 0

foreach ($channel in $channels) {
    $username = $channel.username
    
    # Prepare payload
    $payload = @{
        username = $channel.username
        site = $channel.site
        is_paused = $channel.is_paused
        framerate = $channel.framerate
        resolution = $channel.resolution
        pattern = $channel.pattern
        max_duration = $channel.max_duration
        max_filesize = $channel.max_filesize
        created_at = $channel.created_at
    }
    
    # Add streamed_at if it exists
    if ($null -ne $channel.streamed_at) {
        $payload.streamed_at = $channel.streamed_at
    }
    
    $jsonPayload = $payload | ConvertTo-Json -Compress
    
    try {
        # Upsert to Supabase (insert or update if exists)
        $headers = @{
            "apikey" = $SupabaseApiKey
            "Authorization" = "Bearer $SupabaseApiKey"
            "Content-Type" = "application/json"
            "Prefer" = "resolution=merge-duplicates"
        }
        
        $response = Invoke-RestMethod -Uri "$SupabaseUrl/rest/v1/channels" `
            -Method Post `
            -Headers $headers `
            -Body $jsonPayload `
            -ErrorAction Stop
        
        Write-Host "[SUCCESS] Synced channel: $username" -ForegroundColor Green
        $successCount++
    }
    catch {
        Write-Host "[ERROR] Failed to sync channel: $username - $($_.Exception.Message)" -ForegroundColor Red
        $errorCount++
    }
}

Write-Host "`n[INFO] Channel sync completed" -ForegroundColor Cyan
Write-Host "[INFO] Success: $successCount, Errors: $errorCount" -ForegroundColor Cyan
