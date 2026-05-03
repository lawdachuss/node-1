#!/bin/bash
# Quick EC2 Crash Diagnosis
# Run this in AWS Session Manager

echo "🔍 EC2 Crash Diagnosis"
echo "======================"
echo ""

cd /home/ubuntu/record || {
    echo "❌ ERROR: /home/ubuntu/record directory not found!"
    echo "   Checking alternative locations..."
    if [ -d "/home/ubuntu/goondvr" ]; then
        echo "   Found: /home/ubuntu/goondvr"
        cd /home/ubuntu/goondvr
    else
        echo "   No project directory found!"
        exit 1
    fi
}

echo "📍 Working directory: $(pwd)"
echo ""

# 1. Container Status
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🐳 CONTAINER STATUS"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
sudo docker compose ps
echo ""

# 2. Check for OOM (Out of Memory) kills
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "💀 OUT OF MEMORY KILLS"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
OOM_KILLS=$(sudo dmesg | grep -i "killed process" | tail -10)
if [ -z "$OOM_KILLS" ]; then
    echo "✅ No OOM kills detected"
else
    echo "❌ FOUND OOM KILLS:"
    echo "$OOM_KILLS"
fi
echo ""

# 3. Memory Status
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🧠 MEMORY USAGE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
free -h
echo ""
MEM_USAGE=$(free | grep Mem | awk '{printf "%.0f", $3/$2 * 100}')
echo "Memory usage: ${MEM_USAGE}%"
if [ "$MEM_USAGE" -gt 90 ]; then
    echo "❌ CRITICAL: Memory usage is very high!"
elif [ "$MEM_USAGE" -gt 80 ]; then
    echo "⚠️  WARNING: Memory usage is high"
else
    echo "✅ Memory usage is acceptable"
fi
echo ""

# 4. Disk Space
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "💾 DISK SPACE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
df -h /
echo ""
DISK_USAGE=$(df / | tail -1 | awk '{print $5}' | sed 's/%//')
echo "Disk usage: ${DISK_USAGE}%"
if [ "$DISK_USAGE" -gt 90 ]; then
    echo "❌ CRITICAL: Disk is almost full!"
elif [ "$DISK_USAGE" -gt 80 ]; then
    echo "⚠️  WARNING: Disk space is low"
else
    echo "✅ Disk space is acceptable"
fi
echo ""

# 5. Recent Container Logs
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📋 RECENT ERRORS (Last 50 lines)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
sudo docker compose logs --tail=50 2>&1 | grep -i "error\|fatal\|panic\|killed\|crash" || echo "No critical errors in recent logs"
echo ""

# 6. Container Exit Codes
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🔍 CONTAINER EXIT CODES"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
sudo docker ps -a --format "table {{.Names}}\t{{.Status}}\t{{.State}}"
echo ""
echo "Exit code meanings:"
echo "  137 = Killed by OOM (Out of Memory)"
echo "  139 = Segmentation fault"
echo "  143 = Terminated (SIGTERM)"
echo ""

# 7. System Uptime
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "⏱️  SYSTEM INFO"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Uptime: $(uptime -p)"
echo "Load average: $(uptime | awk -F'load average:' '{print $2}')"
echo ""

# 8. Active Channels
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📺 CHANNEL STATUS"
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

# Summary
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📊 DIAGNOSIS SUMMARY"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Determine most likely cause
if [ ! -z "$OOM_KILLS" ]; then
    echo "🎯 LIKELY CAUSE: OUT OF MEMORY"
    echo ""
    echo "The system ran out of RAM and killed processes."
    echo ""
    echo "🔧 SOLUTIONS:"
    echo "1. Reduce Byparr instances:"
    echo "   sudo docker compose up -d --scale byparr=1 --no-recreate"
    echo ""
    echo "2. Reduce active channels (keep 5-8 max for t2.micro)"
    echo ""
    echo "3. Add swap space (temporary fix):"
    echo "   sudo fallocate -l 2G /swapfile"
    echo "   sudo chmod 600 /swapfile"
    echo "   sudo mkswap /swapfile"
    echo "   sudo swapon /swapfile"
    echo ""
elif [ "$DISK_USAGE" -gt 90 ]; then
    echo "🎯 LIKELY CAUSE: DISK FULL"
    echo ""
    echo "🔧 SOLUTIONS:"
    echo "1. Clean up old recordings:"
    echo "   find videos -name '*.mp4' -mtime +1 -delete"
    echo ""
    echo "2. Clean Docker cache:"
    echo "   sudo docker system prune -af"
    echo ""
elif [ "$MEM_USAGE" -gt 85 ]; then
    echo "🎯 LIKELY CAUSE: HIGH MEMORY PRESSURE"
    echo ""
    echo "🔧 SOLUTIONS:"
    echo "1. Restart containers to free memory:"
    echo "   sudo docker compose restart"
    echo ""
    echo "2. Reduce Byparr instances:"
    echo "   sudo docker compose up -d --scale byparr=1 --no-recreate"
    echo ""
else
    echo "🤔 CAUSE UNCLEAR - Check logs above for clues"
    echo ""
    echo "🔧 GENERAL RECOVERY:"
    echo "1. Restart everything:"
    echo "   sudo docker compose restart"
    echo ""
    echo "2. View full logs:"
    echo "   sudo docker compose logs --tail=200"
    echo ""
fi

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
