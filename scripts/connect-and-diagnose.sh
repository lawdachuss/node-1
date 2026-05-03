#!/bin/bash
# Connect to EC2 and diagnose recording issues
# Run this from your local machine (Mac/Linux)

EC2_IP="3.84.15.178"
KEY_PATH="aws-secrets/aws-key.pem"

echo "🔌 Connecting to EC2: $EC2_IP"
echo "================================"
echo ""

# Test connection first
echo "Testing connection..."
if ! ssh -i "$KEY_PATH" -o ConnectTimeout=10 -o StrictHostKeyChecking=no ubuntu@$EC2_IP "echo 'OK'" &>/dev/null; then
    echo "❌ Cannot connect to EC2 instance!"
    echo ""
    echo "Possible issues:"
    echo "1. Security Group doesn't allow SSH from your IP"
    echo "2. Instance is stopped or terminated"
    echo "3. Wrong IP address or key file"
    echo ""
    echo "🔧 Solutions:"
    echo ""
    echo "Option A: Fix Security Group (AWS Console)"
    echo "1. Go to EC2 Console → Security Groups"
    echo "2. Find your instance's security group"
    echo "3. Edit Inbound Rules"
    echo "4. Add rule: SSH (port 22) from 'My IP'"
    echo ""
    echo "Option B: Use AWS Systems Manager (No SSH needed)"
    echo "1. Go to EC2 Console → Instances"
    echo "2. Select your instance"
    echo "3. Click 'Connect' → 'Session Manager'"
    echo "4. Run: cd /home/ubuntu/goondvr && sudo docker logs goondvr --tail 100"
    echo ""
    exit 1
fi

echo "✅ Connected successfully!"
echo ""

# Run diagnostics
echo "🔍 Running diagnostics..."
echo ""

ssh -i "$KEY_PATH" -o StrictHostKeyChecking=no ubuntu@$EC2_IP << 'ENDSSH'
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
echo "Cloudflare blocks: $(sudo docker logs goondvr --tail 200 | grep -c 'Cloudflare' || echo '0')"
echo "Offline checks: $(sudo docker logs goondvr --tail 200 | grep -c 'channel is offline' || echo '0')"
echo ""
echo "Recent errors:"
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
    
    COOKIE_LEN=$(grep -o '"cookies":"[^"]*"' conf/settings.json | wc -c)
    echo "Cookie length: $COOKIE_LEN characters"
else
    echo '❌ settings.json not found'
fi

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '📊 ACTIVE CHANNELS'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
if [ -f conf/channels.json ]; then
    TOTAL=$(cat conf/channels.json | grep -c '"username"' || echo '0')
    ACTIVE=$(cat conf/channels.json | grep -c '"is_paused": false' || echo '0')
    echo "Total channels: $TOTAL"
    echo "Active channels: $ACTIVE"
else
    echo '❌ channels.json not found'
fi

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
echo '🎥 ACTIVE RECORDINGS'
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
RECORDING_COUNT=$(sudo docker exec goondvr sh -c 'ls -1 /usr/src/app/videos/*.ts /usr/src/app/videos/*.mp4 2>/dev/null | wc -l' 2>/dev/null || echo '0')
echo "Active recordings: $RECORDING_COUNT"

if [ "$RECORDING_COUNT" -gt 0 ]; then
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
curl -s http://localhost:8191/v1 | grep -o '"status":"[^"]*"' || echo 'Byparr not responding'

echo ''
echo '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━'
ENDSSH

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📊 DIAGNOSIS COMPLETE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "🌐 Web UI: http://$EC2_IP:8080"
echo ""
