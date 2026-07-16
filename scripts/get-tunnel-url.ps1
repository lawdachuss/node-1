# Get the current tunnel URL from Supabase
# Usage: .\scripts\get-tunnel-url.ps1

$ErrorActionPreference = "Stop"

# Load environment variables from .env if it exists
if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        if ($_ -match '^\s*([^#][^=]+)=(.*)$') {
            $name = $matches[1].Trim()
            $value = $matches[2].Trim()
            if ($value.StartsWith('"') -and $value.EndsWith('"')) { $value = $value.Substring(1, $value.Length - 2) }
            if ($value.StartsWith("'") -and $value.EndsWith("'")) { $value = $value.Substring(1, $value.Length - 2) }
            [Environment]::SetEnvironmentVariable($name, $value, "Process")
        }
    }
}

$SUPABASE_URL = $env:SUPABASE_URL
$SUPABASE_API_KEY = $env:SUPABASE_API_KEY

if (-not $SUPABASE_URL -or -not $SUPABASE_API_KEY) {
    Write-Host "Error: SUPABASE_URL and SUPABASE_API_KEY must be set" -ForegroundColor Red
    Write-Host "Set them in .env file or as environment variables" -ForegroundColor Yellow
    exit 1
}

Write-Host "Fetching active tunnel URLs from Supabase (tunnels table)..." -ForegroundColor Cyan

try {
    $tunnelUri = $SUPABASE_URL + "/rest/v1/tunnels?select=instance_id,url,is_active,created_at" + [char]38 + "is_active=eq.true" + [char]38 + "order=created_at.desc"
    $response = Invoke-RestMethod -Uri $tunnelUri -Method Get -Headers @{
        "apikey" = $SUPABASE_API_KEY
        "Content-Type" = "application/json"
    }

    if ($response -and $response.Count -gt 0) {
        $count = $response.Count
        Write-Host ""
        Write-Host "=== ACTIVE WEB UI TUNNELS ($count) ===" -ForegroundColor Green
        Write-Host ""
        $urls = @()
        foreach ($tunnel in $response) {
            Write-Host "   [$($tunnel.instance_id)] $($tunnel.url)" -ForegroundColor Cyan
            Write-Host "::notice title=Web UI ($($tunnel.instance_id))::$($tunnel.url)"
            $urls += $tunnel.url
        }
        Write-Host ""

        # Copy the first/most-recent URL to clipboard if possible
        try {
            Set-Clipboard -Value $urls[0]
            Write-Host "Latest URL copied to clipboard!" -ForegroundColor Green
        } catch {
            Write-Host "Copy a URL above to access the UI" -ForegroundColor Yellow
        }
    } else {
        Write-Host "No active tunnels found in Supabase" -ForegroundColor Red
        Write-Host "Make sure the GitHub Actions workflow has run and the DVR posted its tunnel" -ForegroundColor Yellow
    }
} catch {
    Write-Host "Error fetching tunnel URLs: $_" -ForegroundColor Red
    Write-Host "Check your Supabase credentials and ensure the tunnels table exists" -ForegroundColor Yellow
    exit 1
}
