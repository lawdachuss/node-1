#!/usr/bin/env pwsh

# Test the Upload Completed button functionality
# This script tests the /api/upload/completed endpoint

$ErrorActionPreference = "Stop"

Write-Host ""
Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║        🧪 Testing Upload Completed Button                  ║" -ForegroundColor Cyan
Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""

# Check if server is running
Write-Host "🔍 Step 1: Checking if server is running..." -ForegroundColor Yellow
try {
    $null = Invoke-WebRequest -Uri "http://localhost:8080" -Method GET -TimeoutSec 5 -UseBasicParsing
    Write-Host "✅ Server is running" -ForegroundColor Green
} catch {
    Write-Host "❌ Server is not running on port 8080" -ForegroundColor Red
    Write-Host ""
    Write-Host "Please start the server first:" -ForegroundColor Yellow
    Write-Host "  .\goondvr.exe --port 8080 --enable-gofile-upload" -ForegroundColor White
    Write-Host ""
    exit 1
}
Write-Host ""

# Check settings
Write-Host "🔍 Step 2: Checking settings..." -ForegroundColor Yellow
if (Test-Path "conf/settings.json") {
    Write-Host "✅ conf/settings.json exists" -ForegroundColor Green
    
    $settings = Get-Content "conf/settings.json" -Raw
    if ($settings -match '"enable_gofile_upload":\s*true') {
        Write-Host "✅ Multi-host upload is enabled" -ForegroundColor Green
    } else {
        Write-Host "⚠️  Multi-host upload is disabled" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "To enable, update conf/settings.json:" -ForegroundColor Yellow
        Write-Host '  "enable_gofile_upload": true' -ForegroundColor White
        Write-Host ""
    }
} else {
    Write-Host "⚠️  conf/settings.json not found" -ForegroundColor Yellow
}
Write-Host ""

# Check completed directory
Write-Host "🔍 Step 3: Checking completed directory..." -ForegroundColor Yellow
$completedDir = "videos/completed"
if (Test-Path $completedDir) {
    Write-Host "✅ Completed directory exists: $completedDir" -ForegroundColor Green
    
    # Count video files
    $videoFiles = Get-ChildItem -Path $completedDir -File -Include "*.mp4","*.mkv" -Recurse -ErrorAction SilentlyContinue
    $tsFiles = Get-ChildItem -Path $completedDir -File -Filter "*.ts" -Recurse -ErrorAction SilentlyContinue
    
    $videoCount = ($videoFiles | Measure-Object).Count
    $tsCount = ($tsFiles | Measure-Object).Count
    
    Write-Host "📊 Files found:" -ForegroundColor Cyan
    Write-Host "   • MP4/MKV files: $videoCount" -ForegroundColor White
    Write-Host "   • TS files: $tsCount (will be skipped)" -ForegroundColor White
    
    if ($videoCount -eq 0) {
        Write-Host ""
        Write-Host "⚠️  No video files to upload" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "To test, add some .mp4 or .mkv files to: $completedDir" -ForegroundColor Yellow
        Write-Host ""
        exit 0
    }
} else {
    Write-Host "⚠️  Completed directory not found: $completedDir" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Creating directory..." -ForegroundColor Yellow
    New-Item -ItemType Directory -Path $completedDir -Force | Out-Null
    Write-Host "✅ Directory created" -ForegroundColor Green
    Write-Host ""
    Write-Host "Add some .mp4 or .mkv files to test" -ForegroundColor Yellow
    exit 0
}
Write-Host ""

# Test the endpoint
Write-Host "🚀 Step 4: Testing upload endpoint..." -ForegroundColor Yellow
Write-Host ""
Write-Host "Sending POST request to /api/upload/completed..." -ForegroundColor Cyan
Write-Host ""

try {
    $response = Invoke-RestMethod -Uri "http://localhost:8080/api/upload/completed" -Method POST -ContentType "application/json"
    
    Write-Host "📥 Response:" -ForegroundColor Cyan
    $response | ConvertTo-Json -Depth 10 | Write-Host
    Write-Host ""
    
    if ($response.error) {
        Write-Host "❌ Upload request failed" -ForegroundColor Red
        Write-Host ""
        Write-Host "Error details:" -ForegroundColor Yellow
        Write-Host $response.error -ForegroundColor Red
        Write-Host ""
        exit 1
    } elseif ($response.message) {
        Write-Host "✅ Upload request successful" -ForegroundColor Green
        Write-Host ""
        Write-Host "📊 Details:" -ForegroundColor Cyan
        Write-Host "   • Message: $($response.message)" -ForegroundColor White
        Write-Host "   • File count: $($response.count)" -ForegroundColor White
        Write-Host ""
        
        if ($response.count -gt 0) {
            Write-Host "📋 Check the server logs for upload progress" -ForegroundColor Cyan
            Write-Host ""
            Write-Host "You should see logs like:" -ForegroundColor Yellow
            Write-Host "  📤 Starting manual upload of completed files..." -ForegroundColor White
            Write-Host "  📦 Found X video file(s) to upload" -ForegroundColor White
            Write-Host "  📹 Processing file 1/X: filename.mp4" -ForegroundColor White
            Write-Host "  🚀 Starting parallel uploads to all configured hosts..." -ForegroundColor White
            Write-Host "  ✅ Upload completed: X/X hosts successful" -ForegroundColor White
            Write-Host ""
            
            # Wait a bit and check if files are still there
            Write-Host "⏳ Waiting 10 seconds for upload to process..." -ForegroundColor Yellow
            Start-Sleep -Seconds 10
            Write-Host ""
            
            $remainingFiles = Get-ChildItem -Path $completedDir -File -Include "*.mp4","*.mkv" -Recurse -ErrorAction SilentlyContinue
            $remaining = ($remainingFiles | Measure-Object).Count
            
            if ($remaining -lt $videoCount) {
                $uploaded = $videoCount - $remaining
                Write-Host "✅ Upload in progress: $uploaded file(s) uploaded so far" -ForegroundColor Green
                Write-Host "   Remaining: $remaining file(s)" -ForegroundColor White
            } elseif ($remaining -eq $videoCount) {
                Write-Host "⏳ Files still present (upload may be in progress)" -ForegroundColor Yellow
                Write-Host "   Check server logs for details" -ForegroundColor White
            } else {
                Write-Host "✅ All files uploaded and deleted!" -ForegroundColor Green
            }
        } else {
            Write-Host "ℹ️  No files were uploaded (directory empty or no valid files)" -ForegroundColor Cyan
        }
    } else {
        Write-Host "⚠️  Unexpected response format" -ForegroundColor Yellow
        $response | ConvertTo-Json -Depth 10 | Write-Host
    }
} catch {
    Write-Host "❌ Request failed: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host ""
    exit 1
}

Write-Host ""
Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║                  ✅ Test Complete                          ║" -ForegroundColor Cyan
Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""
Write-Host "💡 Tips:" -ForegroundColor Yellow
Write-Host "  • Check server logs for detailed upload progress" -ForegroundColor White
Write-Host "  • Verify uploads in Supabase database" -ForegroundColor White
Write-Host "  • Check that files are deleted after successful upload" -ForegroundColor White
Write-Host ""
