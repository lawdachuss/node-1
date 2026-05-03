#!/bin/bash

# Test the Upload Completed button functionality
# This script tests the /api/upload/completed endpoint

set -e

echo "╔════════════════════════════════════════════════════════════╗"
echo "║        🧪 Testing Upload Completed Button                  ║"
echo "╚════════════════════════════════════════════════════════════╝"
echo ""

# Check if server is running
echo "🔍 Step 1: Checking if server is running..."
if ! curl -s -f http://localhost:8080 > /dev/null 2>&1; then
    echo "❌ Server is not running on port 8080"
    echo ""
    echo "Please start the server first:"
    echo "  ./goondvr --port 8080 --enable-gofile-upload"
    echo ""
    exit 1
fi
echo "✅ Server is running"
echo ""

# Check settings
echo "🔍 Step 2: Checking settings..."
if [ -f "conf/settings.json" ]; then
    echo "✅ conf/settings.json exists"
    
    if grep -q '"enable_gofile_upload": true' conf/settings.json; then
        echo "✅ Multi-host upload is enabled"
    else
        echo "⚠️  Multi-host upload is disabled"
        echo ""
        echo "To enable, update conf/settings.json:"
        echo '  "enable_gofile_upload": true'
        echo ""
    fi
else
    echo "⚠️  conf/settings.json not found"
fi
echo ""

# Check completed directory
echo "🔍 Step 3: Checking completed directory..."
COMPLETED_DIR="videos/completed"
if [ -d "$COMPLETED_DIR" ]; then
    echo "✅ Completed directory exists: $COMPLETED_DIR"
    
    # Count video files
    VIDEO_COUNT=$(find "$COMPLETED_DIR" -type f \( -name "*.mp4" -o -name "*.mkv" \) 2>/dev/null | wc -l)
    TS_COUNT=$(find "$COMPLETED_DIR" -type f -name "*.ts" 2>/dev/null | wc -l)
    
    echo "📊 Files found:"
    echo "   • MP4/MKV files: $VIDEO_COUNT"
    echo "   • TS files: $TS_COUNT (will be skipped)"
    
    if [ "$VIDEO_COUNT" -eq 0 ]; then
        echo ""
        echo "⚠️  No video files to upload"
        echo ""
        echo "To test, add some .mp4 or .mkv files to: $COMPLETED_DIR"
        echo ""
        exit 0
    fi
else
    echo "⚠️  Completed directory not found: $COMPLETED_DIR"
    echo ""
    echo "Creating directory..."
    mkdir -p "$COMPLETED_DIR"
    echo "✅ Directory created"
    echo ""
    echo "Add some .mp4 or .mkv files to test"
    exit 0
fi
echo ""

# Test the endpoint
echo "🚀 Step 4: Testing upload endpoint..."
echo ""
echo "Sending POST request to /api/upload/completed..."
echo ""

RESPONSE=$(curl -s -X POST http://localhost:8080/api/upload/completed)

echo "📥 Response:"
echo "$RESPONSE" | jq '.' 2>/dev/null || echo "$RESPONSE"
echo ""

# Check response
if echo "$RESPONSE" | grep -q '"error"'; then
    echo "❌ Upload request failed"
    echo ""
    echo "Error details:"
    echo "$RESPONSE" | jq -r '.error' 2>/dev/null || echo "$RESPONSE"
    echo ""
    exit 1
elif echo "$RESPONSE" | grep -q '"message"'; then
    MESSAGE=$(echo "$RESPONSE" | jq -r '.message' 2>/dev/null || echo "$RESPONSE")
    COUNT=$(echo "$RESPONSE" | jq -r '.count' 2>/dev/null || echo "0")
    
    echo "✅ Upload request successful"
    echo ""
    echo "📊 Details:"
    echo "   • Message: $MESSAGE"
    echo "   • File count: $COUNT"
    echo ""
    
    if [ "$COUNT" -gt 0 ]; then
        echo "📋 Check the server logs for upload progress"
        echo ""
        echo "You should see logs like:"
        echo "  📤 Starting manual upload of completed files..."
        echo "  📦 Found X video file(s) to upload"
        echo "  📹 Processing file 1/X: filename.mp4"
        echo "  🚀 Starting parallel uploads to all configured hosts..."
        echo "  ✅ Upload completed: X/X hosts successful"
        echo ""
        
        # Wait a bit and check if files are still there
        echo "⏳ Waiting 10 seconds for upload to process..."
        sleep 10
        echo ""
        
        REMAINING=$(find "$COMPLETED_DIR" -type f \( -name "*.mp4" -o -name "*.mkv" \) 2>/dev/null | wc -l)
        
        if [ "$REMAINING" -lt "$VIDEO_COUNT" ]; then
            UPLOADED=$((VIDEO_COUNT - REMAINING))
            echo "✅ Upload in progress: $UPLOADED file(s) uploaded so far"
            echo "   Remaining: $REMAINING file(s)"
        elif [ "$REMAINING" -eq "$VIDEO_COUNT" ]; then
            echo "⏳ Files still present (upload may be in progress)"
            echo "   Check server logs for details"
        else
            echo "✅ All files uploaded and deleted!"
        fi
    else
        echo "ℹ️  No files were uploaded (directory empty or no valid files)"
    fi
else
    echo "⚠️  Unexpected response format"
    echo "$RESPONSE"
fi

echo ""
echo "╔════════════════════════════════════════════════════════════╗"
echo "║                  ✅ Test Complete                          ║"
echo "╚════════════════════════════════════════════════════════════╝"
echo ""
echo "💡 Tips:"
echo "  • Check server logs for detailed upload progress"
echo "  • Verify uploads in Supabase database"
echo "  • Check that files are deleted after successful upload"
echo ""
