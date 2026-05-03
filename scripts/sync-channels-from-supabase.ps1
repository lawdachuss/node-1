# Sync channels from Supabase to conf/channels.json
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

Write-Host "[INFO] Fetching channels from Supabase..." -ForegroundColor Cyan

try {
    $headers = @{
        "apikey" = $SupabaseApiKey
        "Authorization" = "Bearer $SupabaseApiKey"
    }
    
    $response = Invoke-RestMethod -Uri "$SupabaseUrl/rest/v1/channels?order=created_at.asc" `
        -Method Get `
        -Headers $headers `
        -ErrorAction Stop
    
    if ($response.Count -eq 0) {
        Write-Host "[INFO] No channels found in Supabase" -ForegroundColor Yellow
        
        # Keep existing channels.json if it exists
        if (Test-Path $ChannelsFile) {
            Write-Host "[INFO] Keeping existing channels.json" -ForegroundColor Cyan
        } else {
            "[]" | Out-File -FilePath $ChannelsFile -Encoding UTF8
        }
        exit 0
    }
    
    # Transform Supabase response to channels.json format
    $channels = @()
    foreach ($channel in $response) {
        $channelObj = @{
            is_paused = $channel.is_paused
            username = $channel.username
            site = $channel.site
            framerate = $channel.framerate
            resolution = $channel.resolution
            pattern = $channel.pattern
            max_duration = $channel.max_duration
            max_filesize = $channel.max_filesize
            created_at = $channel.created_at
        }
        
        # Add streamed_at if it exists
        if ($null -ne $channel.streamed_at) {
            $channelObj.streamed_at = $channel.streamed_at
        }
        
        $channels += $channelObj
    }
    
    # Ensure conf directory exists
    if (-not (Test-Path "conf")) {
        New-Item -ItemType Directory -Path "conf" | Out-Null
    }
    
    # Write to channels.json with proper formatting
    $channels | ConvertTo-Json -Depth 10 | Out-File -FilePath $ChannelsFile -Encoding UTF8
    
    Write-Host "[SUCCESS] Synced $($channels.Count) channels from Supabase" -ForegroundColor Green
}
catch {
    Write-Host "[ERROR] Failed to fetch channels from Supabase: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
