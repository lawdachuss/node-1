# Connect to EC2 and diagnose recording issues
# Run this from your local machine

$EC2_IP = "3.84.15.178"
$KEY_PATH = "aws-secrets/aws-key.pem"

Write-Host "🔌 Connecting to EC2: $EC2_IP" -ForegroundColor Cyan
Write-Host "================================" -ForegroundColor Cyan
Write-Host ""

# Test connection first
Write-Host "Testing connection..." -ForegroundColor Yellow
$testResult = ssh -i $KEY_PATH -o ConnectTimeout=10 -o StrictHostKeyChecking=no ubuntu@$EC2_IP "echo 'OK'" 2>&1

if ($LASTEXITCODE -ne 0) {
    Write-Host "❌ Cannot connect to EC2 instance!" -ForegroundColor Red
    Write-Host ""
    Write-Host "Possible issues:" -ForegroundColor Yellow
    Write-Host "1. Security Group doesn't allow SSH from your IP" -ForegroundColor White
    Write-Host "2. Instance is stopped or terminated" -ForegroundColor White
    Write-Host "3. Wrong IP address or key file" -ForegroundColor White
    Write-Host ""
    Write-Host "🔧 Solutions:" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "Option A: Fix Security Group (AWS Console)" -ForegroundColor Yellow
    Write-Host "1. Go to EC2 Console → Security Groups" -ForegroundColor White
    Write-Host "2. Find your instance's security group" -ForegroundColor White
    Write-Host "3. Edit Inbound Rules" -ForegroundColor White
    Write-Host "4. Add rule: SSH (port 22) from 'My IP'" -ForegroundColor White
    Write-Host ""
    Write-Host "Option B: Use AWS Systems Manager (No SSH needed)" -ForegroundColor Yellow
    Write-Host "1. Go to EC2 Console → Instances" -ForegroundColor White
    Write-Host "2. Select your instance" -ForegroundColor White
    Write-Host "3. Click 'Connect' → 'Session Manager'" -ForegroundColor White
    Write-Host "4. Run these commands:" -ForegroundColor White
    Write-Host ""
    Write-Host "   cd /home/ubuntu/goondvr" -ForegroundColor Gray
    Write-Host "   sudo docker logs goondvr --tail 100" -ForegroundColor Gray
    Write-Host "   sudo docker compose ps" -ForegroundColor Gray
    Write-Host ""
    Write-Host "Option C: Check via AWS CloudWatch Logs" -ForegroundColor Yellow
    Write-Host "If you have CloudWatch configured, check logs there" -ForegroundColor White
    Write-Host ""
    
    exit 1
}

Write-Host "✅ Connected successfully!" -ForegroundColor Green
Write-Host ""

# Run diagnostics
Write-Host "🔍 Running diagnostics..." -ForegroundColor Yellow
Write-Host ""

ssh -i $KEY_PATH -o StrictHostKeyChecking=no ubuntu@$EC2_IP @"
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '📊 CONTAINER STATUS'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
cd /home/ubuntu/goondvr
sudo docker compose ps

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '📋 RECENT LOGS (Last 50 lines)'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
sudo docker logs goondvr --tail 50

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '🔍 ERROR SUMMARY'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo 'Cloudflare blocks:'
sudo docker logs goondvr --tail 200 | grep -c 'Cloudflare' || echo '0'

echo 'Offline checks:'
sudo docker logs goondvr --tail 200 | grep -c 'channel is offline' || echo '0'

echo 'Recent errors:'
sudo docker logs goondvr --tail 200 | grep 'ERROR' | tail -5

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '🍪 COOKIE STATUS'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
if [ -f conf/settings.json ]; then
    if grep -q 'cf_clearance=' conf/settings.json; then
        echo '✅ cf_clearance cookie found'
    else
        echo '❌ cf_clearance cookie MISSING'
    fi
    
    if grep -q 'csrftoken=' conf/settings.json; then
        echo '✅ csrftoken cookie found'
    else
        echo '⚠️  csrftoken cookie missing'
    fi
    
    COOKIE_LEN=\$(grep -o '\"cookies\":\"[^\"]*\"' conf/settings.json | wc -c)
    echo \"Cookie length: \$COOKIE_LEN characters\"
else
    echo '❌ settings.json not found'
fi

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '📊 ACTIVE CHANNELS'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
if [ -f conf/channels.json ]; then
    TOTAL=\$(cat conf/channels.json | grep -c '\"username\"' || echo '0')
    ACTIVE=\$(cat conf/channels.json | grep -c '\"is_paused\": false' || echo '0')
    echo \"Total channels: \$TOTAL\"
    echo \"Active channels: \$ACTIVE\"
else
    echo '❌ channels.json not found'
fi

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '🎥 ACTIVE RECORDINGS'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
RECORDING_COUNT=\$(sudo docker exec goondvr sh -c 'ls -1 /usr/src/app/videos/*.ts /usr/src/app/videos/*.mp4 2>/dev/null | wc -l' 2>/dev/null || echo '0')
echo \"Active recordings: \$RECORDING_COUNT\"

if [ \"\$RECORDING_COUNT\" -gt 0 ]; then
    echo ''
    echo 'Current recordings:'
    sudo docker exec goondvr sh -c 'ls -lh /usr/src/app/videos/*.ts /usr/src/app/videos/*.mp4 2>/dev/null | tail -5'
fi

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '💾 DISK SPACE'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
df -h / | grep -E 'Filesystem|/dev'

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '🧠 MEMORY USAGE'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
free -h | grep Mem

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '🔧 BYPARR STATUS'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
curl -s http://localhost:8191/v1 | grep -o '\"status\":\"[^\"]*\"' || echo 'Byparr not responding'

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
"@

Write-Host ""
Write-Host "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" -ForegroundColor Cyan
Write-Host "📊 DIAGNOSIS COMPLETE" -ForegroundColor Cyan
Write-Host "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━" -ForegroundColor Cyan
Write-Host ""

Write-Host "🌐 Web UI: http://$EC2_IP:8080" -ForegroundColor Yellow
Write-Host ""
Write-Host "Press Enter to exit..." -ForegroundColor Green
Read-Host
