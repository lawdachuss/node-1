#!/bin/bash
# Remote EC2 Diagnosis Script
# Run from local machine

EC2_IP="52.70.112.6"
KEY_PATH="aws-secrets/aws-key.pem"

echo "🔍 Running remote diagnostics on $EC2_IP..."
echo ""

ssh -i "$KEY_PATH" -o StrictHostKeyChecking=no ubuntu@$EC2_IP << 'ENDSSH'
cd /home/ubuntu/record

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🐳 CONTAINER STATUS"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
sudo docker compose ps
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "💀 OUT OF MEMORY KILLS (Last 10)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
OOM_KILLS=$(sudo dmesg | grep -i "killed process" | tail -10)
if [ -z "$OOM_KILLS" ]; then
    echo "✅ No OOM kills detected"
else
    echo "❌ FOUND OOM KILLS:"
    echo "$OOM_KILLS"
fi
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🧠 MEMORY USAGE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
free -h
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "💾 DISK SPACE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
df -h /
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🔍 CONTAINER EXIT CODES"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
sudo docker ps -a --format "table {{.Names}}\t{{.Status}}\t{{.State}}"
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📋 RECENT ERRORS (Last 50 lines)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
sudo docker compose logs --tail=50 2>&1 | grep -i "error\|fatal\|panic\|killed\|crash" | head -20 || echo "No critical errors found"
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📺 ACTIVE CHANNELS"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if [ -f conf/channels.json ]; then
    TOTAL=$(cat conf/channels.json | grep -c '"username"' || echo '0')
    ACTIVE=$(cat conf/channels.json | grep -c '"is_paused": false' || echo '0')
    echo "Total channels: $TOTAL"
    echo "Active channels: $ACTIVE"
else
    echo "⚠️  channels.json not found"
fi
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "⏱️  SYSTEM UPTIME"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
uptime
echo ""

ENDSSH

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ Diagnosis complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
