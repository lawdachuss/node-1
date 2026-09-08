# Check live API logs from all fleet nodes via TryCloudflare tunnels
# Also queries channel_logs from Supabase for persisted diagnostics
# Usage: .\scripts\check-live-logs.ps1 [-Channel <name>] [-Limit <n>] [-SupabaseOnly] [-NodeLogs] [-Tail]

param(
    [string]$Channel = "",
    [int]$Limit = 50,
    [switch]$SupabaseOnly,
    [switch]$NodeLogs,
    [switch]$Tail
)

$ErrorActionPreference = "Stop"

# Load .env
if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        if ($_ -match '^\s*([^#][^=]+)=(.*)$') {
            $name = $matches[1].Trim()
            $value = $matches[2].Trim().Trim('"').Trim("'")
            [Environment]::SetEnvironmentVariable($name, $value, "Process")
        }
    }
}

$SUPABASE_URL = $env:SUPABASE_URL
$SUPABASE_API_KEY = $env:SUPABASE_API_KEY

if (-not $SUPABASE_URL -or -not $SUPABASE_API_KEY) {
    Write-Host "Error: SUPABASE_URL and SUPABASE_API_KEY must be set in .env" -ForegroundColor Red
    exit 1
}

$headers = @{
    "apikey" = $SUPABASE_API_KEY
    "Authorization" = "Bearer $SUPABASE_API_KEY"
    "Content-Type" = "application/json"
}

# ─── 1. Fetch persisted channel_logs from Supabase ──────────────────────────
Write-Host ""
Write-Host "=== SUPABASE channel_logs (persisted WARN/ERROR) ===" -ForegroundColor Cyan
Write-Host ""

try {
    $logQuery = "/channel_logs?order=created_at.desc&limit=$Limit"
    if ($Channel -ne "") {
        $logQuery = "/channel_logs?username=eq.$Channel&order=created_at.desc&limit=$Limit"
    }
    $logs = Invoke-RestMethod -Uri "$SUPABASE_URL/rest/v1$logQuery" -Method Get -Headers $headers

    if ($logs -and $logs.Count -gt 0) {
        foreach ($entry in $logs) {
            $color = switch ($entry.log_level) {
                "ERROR" { "Red" }
                "WARN"  { "Yellow" }
                default { "Gray" }
            }
            $ts = if ($entry.created_at) { $entry.created_at.Substring(0, 19) } else { "??" }
            $node = if ($entry.instance_id) { $entry.instance_id } else { "?" }
            Write-Host "[$ts] " -NoNewline -ForegroundColor DarkGray
            Write-Host "[$($entry.log_level)] " -NoNewline -ForegroundColor $color
            Write-Host "$($entry.username) " -NoNewline -ForegroundColor White
            Write-Host "($node) " -NoNewline -ForegroundColor DarkCyan
            Write-Host $entry.message
        }
        Write-Host ""
        Write-Host "  Total: $($logs.Count) entries" -ForegroundColor DarkGray
    } else {
        Write-Host "  No channel_logs found" -ForegroundColor DarkGray
    }
} catch {
    Write-Host "  Error fetching channel_logs: $_" -ForegroundColor Red
}

if ($SupabaseOnly) { exit 0 }

# ─── 2. Fetch online nodes with tunnel URLs ─────────────────────────────────
Write-Host ""
Write-Host "=== FLEET NODES (tunnel endpoints) ===" -ForegroundColor Cyan
Write-Host ""

try {
    $nodes = Invoke-RestMethod -Uri "$SUPABASE_URL/rest/v1/nodes?select=*&order=node_id.asc" -Method Get -Headers $headers
} catch {
    Write-Host "  Error fetching nodes: $_" -ForegroundColor Red
    exit 1
}

$onlineNodes = @()
foreach ($node in $nodes) {
    $status = $node.status
    $color = switch ($status) {
        "online"  { "Green" }
        "draining" { "Yellow" }
        default   { "Red" }
    }
    $age = ""
    if ($node.last_heartbeat) {
        try {
            $hb = [DateTime]::Parse($node.last_heartbeat)
            $diff = (Get-Date) - $hb
            if ($diff.TotalMinutes -lt 1) { $age = "just now" }
            elseif ($diff.TotalHours -lt 1) { $age = "$([int]$diff.TotalMinutes)m ago" }
            else { $age = "$([int]$diff.TotalHours)h ago" }
        } catch { $age = "?" }
    }

    $hasUrl = if ($node.web_url) { "YES" } else { "no" }
    Write-Host "  $($node.node_id) " -NoNewline -ForegroundColor White
    Write-Host "[$status] " -NoNewline -ForegroundColor $color
    Write-Host "load=$($node.current_load) " -NoNewline -ForegroundColor DarkGray
    Write-Host "heartbeat=$age " -NoNewline -ForegroundColor DarkGray
    Write-Host "tunnel=$hasUrl" -ForegroundColor $(if ($node.web_url) { "Green" } else { "Red" })

    if ($node.web_url -and $status -eq "online") {
        $onlineNodes += $node
    }
}

Write-Host ""
Write-Host "  Online with tunnel: $($onlineNodes.Count) / $($nodes.Count) nodes" -ForegroundColor DarkGray

if (-not $NodeLogs) {
    Write-Host ""
    Write-Host "  Tip: Use -NodeLogs to hit each node's /api/logs endpoint" -ForegroundColor Yellow
    Write-Host "  Tip: Use -Channel <name> to filter by channel" -ForegroundColor Yellow
    Write-Host "  Tip: Use -Tail to continuously poll every 5s" -ForegroundColor Yellow
    exit 0
}

# ─── 3. Hit each online node's /api/logs ────────────────────────────────────
Write-Host ""
Write-Host "=== LIVE NODE LOGS (from /api/logs via tunnels) ===" -ForegroundColor Cyan
Write-Host ""

foreach ($node in $onlineNodes) {
    $baseUrl = $node.web_url.TrimEnd('/')
    Write-Host "--- $($node.node_id) ($baseUrl) ---" -ForegroundColor Yellow

    try {
        $resp = Invoke-WebRequest -Uri "$baseUrl/api/logs?after=0" -TimeoutSec 10 -UseBasicParsing
        $data = $resp.Content | ConvertFrom-Json
        $lines = $data.lines
        if ($lines -and $lines.Count -gt 0) {
            $display = $lines | Select-Object -Last 30
            foreach ($line in $display) {
                $color = switch -Wildcard ($line.line) {
                    "*ERROR*" { "Red" }
                    "*WARN*"  { "Yellow" }
                    "*[INFO]*" { "White" }
                    default   { "DarkGray" }
                }
                $ts = if ($line.time) { $line.time.Substring(0, [Math]::Min(19, $line.time.Length)) } else { "??" }
                Write-Host "  [$ts] " -NoNewline -ForegroundColor DarkGray
                Write-Host $line.line -ForegroundColor $color
            }
            Write-Host "  ... ($($data.total) total entries, showing last 30)" -ForegroundColor DarkGray
        } else {
            Write-Host "  (empty log buffer)" -ForegroundColor DarkGray
        }
    } catch {
        Write-Host "  Connection failed: $($_.Exception.Message)" -ForegroundColor Red
    }
    Write-Host ""
}

# ─── 4. Tail mode ────────────────────────────────────────────────────────────
if ($Tail) {
    Write-Host "Tailing... (Ctrl+C to stop)" -ForegroundColor Cyan
    $lastTotal = @{}
    foreach ($node in $onlineNodes) {
        $lastTotal[$node.node_id] = 0
    }

    while ($true) {
        Start-Sleep -Seconds 5
        foreach ($node in $onlineNodes) {
            $baseUrl = $node.web_url.TrimEnd('/')
            try {
                $after = $lastTotal[$node.node_id]
                $resp = Invoke-WebRequest -Uri "$baseUrl/api/logs?after=$after" -TimeoutSec 10 -UseBasicParsing
                $data = $resp.Content | ConvertFrom-Json
                if ($data.lines -and $data.lines.Count -gt 0) {
                    foreach ($line in $data.lines) {
                        $color = switch -Wildcard ($line.line) {
                            "*ERROR*" { "Red" }
                            "*WARN*"  { "Yellow" }
                            default   { "White" }
                        }
                        $ts = if ($line.time) { $line.time.Substring(0, [Math]::Min(19, $line.time.Length)) } else { "??" }
                        Write-Host "[$($node.node_id)] [$ts] " -NoNewline -ForegroundColor DarkCyan
                        Write-Host $line.line -ForegroundColor $color
                    }
                }
                $lastTotal[$node.node_id] = $data.total
            } catch {
                # silently skip failed nodes during tail
            }
        }
    }
}
