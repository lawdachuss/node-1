param(
    [switch]$NoAppStart
)

$ErrorActionPreference = "Stop"
$ProjectDir = Split-Path -Parent $MyInvocation.MyCommand.Path

# ── Kill any lingering processes ─────────────────────────
Write-Host "Stopping any running chaturbate-dvr, ffmpeg, cloudflared processes..." -ForegroundColor Cyan
Get-Process | Where-Object { $_.ProcessName -match 'chaturbate-dvr|ffmpeg|cloudflared' } | Stop-Process -Force -ErrorAction SilentlyContinue
Write-Host "  ✅ Done" -ForegroundColor Green
Write-Host ""

Write-Host "╔════════════════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║     MiniDelectableService — Full Setup        ║" -ForegroundColor Cyan
Write-Host "╚════════════════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""

# ── 1. Install FFmpeg via winget ──────────────────────────
Write-Host "[1/7] Installing FFmpeg..." -ForegroundColor Yellow
$ffmpeg = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue
if (-not $ffmpeg) {
    winget install Gyan.FFmpeg.Essentials --accept-package-agreements --accept-source-agreements

    # Refresh PATH so we can find what winget just installed
    $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
    $ffmpeg = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue

    if (-not $ffmpeg) {
        # winget didn't add to PATH — search for the binary in the winget packages dir
        $wingetPkgRoot = "$env:LOCALAPPDATA\Microsoft\WinGet\Packages"
        $found = Get-ChildItem -Path "$wingetPkgRoot\Gyan.FFmpeg.Essentials*" -Recurse -Filter "ffmpeg.exe" -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($found) {
            $ffmpegDir = $found.Directory.FullName
            Write-Host "  Found ffmpeg at: $ffmpegDir" -ForegroundColor Cyan
            $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
            [Environment]::SetEnvironmentVariable("Path", "$userPath;$ffmpegDir", "User")
            $env:Path = "$env:Path;$ffmpegDir"
            $ffmpeg = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue
        }
    }

    if (-not $ffmpeg) {
        Write-Host "ERROR: FFmpeg could not be found after install" -ForegroundColor Red
        exit 1
    }
    Write-Host "  ✅ FFmpeg installed at $($ffmpeg.Source)" -ForegroundColor Green
} else {
    Write-Host "  ✅ FFmpeg already installed at $($ffmpeg.Source)" -ForegroundColor Green
}

# ── 2. Ensure ffmpeg is in PATH (for child processes like the DVR) ──
Write-Host "[2/7] Ensuring ffmpeg is on PATH..." -ForegroundColor Yellow
$ffmpeg = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue
if (-not $ffmpeg) {
    Write-Host "ERROR: ffmpeg not found in PATH — DVR will fail to mux/thumbnail" -ForegroundColor Red
    exit 1
}
$ffmpegDir = Split-Path -Parent $ffmpeg.Source
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$ffmpegDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$ffmpegDir", "User")
    Write-Host "  ✅ Added $ffmpegDir to user PATH" -ForegroundColor Green
} else {
    Write-Host "  ✅ Already in PATH: $ffmpegDir" -ForegroundColor Green
}
# Full PATH refresh: Machine + User + current session additions
$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")

# ── 3. Install cloudflared ────────────────────────────────
Write-Host "[3/7] Installing cloudflared..." -ForegroundColor Yellow
$cf = Get-Command cloudflared -ErrorAction SilentlyContinue
if (-not $cf) {
    winget install Cloudflare.cloudflared --accept-package-agreements --accept-source-agreements
    $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
    $cf = Get-Command cloudflared -ErrorAction SilentlyContinue
    if (-not $cf) {
        Write-Host "  ⚠️  cloudflared install failed (tunnel won't be available)" -ForegroundColor Yellow
    } else {
        Write-Host "  ✅ cloudflared installed" -ForegroundColor Green
    }
} else {
    Write-Host "  ✅ cloudflared already installed" -ForegroundColor Green
}

# ── 4. Install Go via winget ──────────────────────────────
Write-Host "[4/7] Installing Go..." -ForegroundColor Yellow
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    winget install GoLang.Go --accept-package-agreements --accept-source-agreements
    $env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
    $go = Get-Command go -ErrorAction SilentlyContinue
    if (-not $go) {
        Write-Host "ERROR: Go install failed" -ForegroundColor Red
        exit 1
    }
    Write-Host "  ✅ Go installed" -ForegroundColor Green
} else {
    Write-Host "  ✅ Go already installed" -ForegroundColor Green
}

# ── 5. Install Go dependencies ────────────────────────────
Write-Host "[5/7] Installing Go dependencies..." -ForegroundColor Yellow
Set-Location -LiteralPath $ProjectDir
go mod download
Write-Host "  ✅ Go modules downloaded" -ForegroundColor Green

# ── 6. Build Go binary ────────────────────────────────────
Write-Host "[6/7] Building Go binary..." -ForegroundColor Yellow
go build -o chaturbate-dvr.exe .
Write-Host "  ✅ Build complete" -ForegroundColor Green

# ── 7. Install Node.js dependencies ───────────────────────
Write-Host "[7/7] Installing Node.js dependencies..." -ForegroundColor Yellow
npm install
Write-Host "  ✅ Node.js deps installed" -ForegroundColor Green

# ── Copy .env if missing ──────────────────────────────────
if (-not (Test-Path "$ProjectDir\.env")) {
    Copy-Item "$ProjectDir\.env.example" "$ProjectDir\.env"
    Add-Content "$ProjectDir\.env" "`n# Safety: don't delete local files after upload`nDELETE_LOCAL_AFTER_UPLOAD=false"
    Write-Host "  📝 Created .env from .env.example — edit it with your API keys!" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "╔════════════════════════════════════════════════╗" -ForegroundColor Green
Write-Host "║            ✅  Setup complete!                 ║" -ForegroundColor Green
Write-Host "╚════════════════════════════════════════════════╝" -ForegroundColor Green
Write-Host ""

if (-not $NoAppStart) {
    Write-Host "🚀 Starting chaturbate-dvr (local only, no tunnel)..." -ForegroundColor Cyan
    Write-Host "   Local:  http://localhost:8080" -ForegroundColor White
    Write-Host ""

    $dvrProc = Start-Process -FilePath "$ProjectDir\chaturbate-dvr.exe" -ArgumentList "--no-tunnel" -NoNewWindow -PassThru

    Start-Sleep -Seconds 3

    # ── Start Cloudflare tunnel in a NEW terminal window ──────────────
    Write-Host "🚇 Starting Cloudflare tunnel in a new window..." -ForegroundColor Cyan
    $tunnelScript = @'
$logFile = "$env:TEMP\cloudflared_tunnel_url.txt"
$p = Start-Process -FilePath "cloudflared" -ArgumentList "tunnel --url http://localhost:8080 --protocol http2" -NoNewWindow -RedirectStandardError $logFile -PassThru
Write-Host "`n🌍 Waiting for tunnel URL..." -ForegroundColor Yellow
$timeout = 30
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$url = $null
while ($sw.Elapsed.TotalSeconds -lt $timeout) {
    Start-Sleep -Milliseconds 500
    if (Test-Path $logFile) {
        $content = Get-Content -Path $logFile -Raw
        if ($content -match 'https://[a-zA-Z0-9-]+\.trycloudflare\.com') {
            $url = $matches[0]
            break
        }
    }
}
if ($url) {
    Write-Host "`n🌍 Public: $url" -ForegroundColor Green
    Write-Host "   Press Ctrl+C to stop the tunnel`n" -ForegroundColor Gray
} else {
    Write-Host "`n⚠️  Tunnel URL not detected within $timeout seconds" -ForegroundColor Yellow
    Write-Host "   Check $logFile for details" -ForegroundColor Gray
}
$p.WaitForExit()
'@
    Start-Process -FilePath "powershell" -ArgumentList "-NoExit", "-Command", $tunnelScript -WindowStyle Normal

    $dvrProc.WaitForExit()
} else {
    Write-Host "Run '.\chaturbate-dvr.exe --no-tunnel' to start the app" -ForegroundColor Yellow
    Write-Host "Then start the tunnel separately with: cloudflared tunnel --url http://localhost:8080" -ForegroundColor Gray
}
