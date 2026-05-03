# Supabase Channels - Single Source of Truth ✅

## Overview

Channels are now stored in and managed through **Supabase only**. All channel operations (add, edit, delete, pause, resume) are automatically synchronized with Supabase, ensuring changes persist across all workflow runs.

---

## What Changed

### ✅ Supabase is Now the Primary Channel Storage

**Before**:
- Channels stored in local `conf/channels.json` file
- Changes lost when GitHub Actions workflow ended
- Manual sync required

**After**:
- Channels stored in Supabase `channels` table
- All changes automatically saved to Supabase
- Local file used only as backup
- Changes persist forever

---

## New Supabase Functions

### Added to `supabase/supabase.go`:

1. **`GetAllChannels()`** - Retrieve all channels from Supabase
2. **`GetChannelByUsername(username)`** - Get specific channel
3. **`InsertChannel(channel)`** - Create new channel
4. **`UpdateChannel(username, channel)`** - Update existing channel
5. **`DeleteChannel(username)`** - Remove channel
6. **`UpsertChannel(channel)`** - Insert or update (used for saves)

### Channel Structure in Supabase:

```go
type ChannelConfig struct {
    ID          int    `json:"id,omitempty"`
    Username    string `json:"username"`
    Site        string `json:"site"`
    IsPaused    bool   `json:"is_paused"`
    Framerate   int    `json:"framerate"`
    Resolution  int    `json:"resolution"`
    Pattern     string `json:"pattern"`
    MaxDuration int    `json:"max_duration"`
    MaxFilesize int    `json:"max_filesize"`
    CreatedAt   int64  `json:"created_at"`
    StreamedAt  int64  `json:"streamed_at,omitempty"`
    UpdatedAt   string `json:"updated_at,omitempty"`
}
```

---

## Modified Manager Functions

### 1. `LoadConfig()` - Enhanced

**New Behavior**:
```
1. Try loading from Supabase first
   ├─ If successful: Use Supabase channels
   ├─ Save backup to local file
   └─ Log: "[CHANNELS] Loaded X channels from Supabase"

2. If Supabase fails or not configured:
   ├─ Fall back to local file
   └─ Log: "[CHANNELS] Loading channels from local file..."

3. Start all channels
```

**Logs You'll See**:
```
[CHANNELS] Loading channels from Supabase...
[CHANNELS] Loaded 30 channels from Supabase
[CHANNELS] Backup saved to local file
```

---

### 2. `SaveConfig()` - Enhanced

**New Behavior**:
```
1. Save all channels to Supabase
   ├─ Uses upsert (insert or update)
   ├─ Each channel saved individually
   └─ Log: "[CHANNELS] ✓ Channels saved to Supabase"

2. Also save to local file as backup
   └─ Log: "[CHANNELS] ✓ Channels saved to local file (backup)"
```

**Logs You'll See**:
```
[CHANNELS] Saving 30 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
[CHANNELS] ✓ Channels saved to local file (backup)
```

---

### 3. `StopChannel()` - Enhanced

**New Behavior**:
```
1. Stop the channel
2. Delete from Supabase
   └─ Log: "[CHANNELS] ✓ Channel username deleted from Supabase"
3. Save updated config to local file
```

**Logs You'll See**:
```
[CHANNELS] Deleting channel honeyyykate from Supabase...
[CHANNELS] ✓ Channel honeyyykate deleted from Supabase
[CHANNELS] ✓ Channels saved to local file (backup)
```

---

## Channel Operations Flow

### Adding a Channel via Web UI

```
User clicks "Add Channel" in Web UI
  ↓
CreateChannel() called
  ↓
Channel added to memory
  ↓
SaveConfig() called
  ├─ Channel saved to Supabase (upsert)
  └─ Backup saved to local file
  ↓
✓ Channel persisted forever
```

**Logs**:
```
[CHANNELS] Saving 31 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
[CHANNELS] ✓ Channels saved to local file (backup)
```

---

### Editing a Channel (Pause/Resume)

```
User clicks "Pause" or "Resume"
  ↓
PauseChannel() or ResumeChannel() called
  ↓
Channel state updated in memory
  ↓
SaveConfig() called
  ├─ Updated channel saved to Supabase
  └─ Backup saved to local file
  ↓
✓ Change persisted forever
```

**Logs**:
```
[CHANNELS] Saving 30 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
[CHANNELS] ✓ Channels saved to local file (backup)
```

---

### Deleting a Channel

```
User clicks "Stop" (delete)
  ↓
StopChannel() called
  ↓
Channel stopped and removed from memory
  ↓
DeleteChannel() called on Supabase
  ├─ Channel deleted from Supabase
  └─ Log: "✓ Channel deleted from Supabase"
  ↓
SaveConfig() called
  └─ Updated list saved to local file
  ↓
✓ Channel permanently removed
```

**Logs**:
```
[CHANNELS] Deleting channel honeyyykate from Supabase...
[CHANNELS] ✓ Channel honeyyykate deleted from Supabase
[CHANNELS] Saving 29 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
```

---

### Next Workflow Run

```
Workflow starts
  ↓
LoadConfig() called
  ↓
Loads channels from Supabase
  ├─ All previous changes included
  ├─ New channels present
  ├─ Deleted channels absent
  └─ Pause states preserved
  ↓
✓ All changes from previous run loaded
```

**Logs**:
```
[CHANNELS] Loading channels from Supabase...
[CHANNELS] Loaded 29 channels from Supabase
[CHANNELS] Backup saved to local file
```

---

## Supabase Table Structure

### Table: `channels`

```sql
CREATE TABLE channels (
    id SERIAL PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    site TEXT NOT NULL DEFAULT 'chaturbate',
    is_paused BOOLEAN NOT NULL DEFAULT false,
    framerate INTEGER NOT NULL DEFAULT 30,
    resolution INTEGER NOT NULL DEFAULT 1080,
    pattern TEXT NOT NULL,
    max_duration INTEGER NOT NULL DEFAULT 45,
    max_filesize INTEGER NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL,
    streamed_at BIGINT,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
```

**Indexes**:
- `idx_channels_username` - Fast username lookups
- `idx_channels_site` - Fast site filtering

**Security**:
- Row Level Security enabled
- Policy allows all operations (adjust as needed)

---

## Testing the Implementation

### Test 1: Add Channel via Web UI

1. **Open Web UI** (Cloudflare tunnel URL)
2. **Click "Add Channel"**
3. **Enter username**: `test_user`
4. **Click Submit**

**Expected Logs**:
```
[CHANNELS] Saving 31 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
[CHANNELS] ✓ Channels saved to local file (backup)
```

**Verify in Supabase**:
- Go to Supabase dashboard
- Open `channels` table
- Should see `test_user` in the list

---

### Test 2: Pause Channel

1. **Find channel** in Web UI
2. **Click "Pause"**

**Expected Logs**:
```
[CHANNELS] Saving 31 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
```

**Verify in Supabase**:
- Check `channels` table
- `is_paused` should be `true` for that channel

---

### Test 3: Delete Channel

1. **Find channel** in Web UI
2. **Click "Stop"** (delete)

**Expected Logs**:
```
[CHANNELS] Deleting channel test_user from Supabase...
[CHANNELS] ✓ Channel test_user deleted from Supabase
[CHANNELS] Saving 30 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
```

**Verify in Supabase**:
- Check `channels` table
- `test_user` should be gone

---

### Test 4: Persistence Across Runs

1. **Add a channel** via Web UI
2. **Wait for workflow to end** (or stop manually)
3. **Start new workflow run**

**Expected Logs on New Run**:
```
[CHANNELS] Loading channels from Supabase...
[CHANNELS] Loaded 31 channels from Supabase
[CHANNELS] Backup saved to local file
```

**Verify**:
- New channel should be loaded and active
- All previous changes should be present

---

## Fallback Behavior

### If Supabase is Down

```
LoadConfig() called
  ↓
Try Supabase → Fails
  ├─ Log: "Failed to load from Supabase: [error]"
  └─ Log: "falling back to local file"
  ↓
Load from local file
  ├─ Uses last saved backup
  └─ Log: "Loaded X channels from local file"
  ↓
✓ Continues working with local backup
```

### If Supabase Credentials Missing

```
LoadConfig() called
  ↓
Check credentials → Not configured
  ├─ Log: "Loading channels from local file..."
  └─ Skip Supabase entirely
  ↓
Load from local file
  ↓
✓ Works without Supabase
```

---

## Configuration Required

### Supabase Credentials

**In `conf/settings.json`**:
```json
{
  "supabase_url": "https://xhfbhgklqylmfmfjtgkq.supabase.co",
  "supabase_api_key": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
}
```

**In GitHub Secrets**:
- `SUPABASE_URL`: Your Supabase project URL
- `SUPABASE_API_KEY`: Your Supabase anon/public key

---

## Files Modified

1. **`supabase/supabase.go`**
   - Added `ChannelConfig` struct
   - Added 6 new channel management functions
   - Total: ~200 lines added

2. **`manager/manager.go`**
   - Enhanced `LoadConfig()` to load from Supabase
   - Enhanced `SaveConfig()` to save to Supabase
   - Enhanced `StopChannel()` to delete from Supabase
   - Added comprehensive logging
   - Added `log` and `supabase` imports

---

## Benefits

### ✅ Persistence
- All channel changes saved to Supabase
- Changes survive workflow restarts
- No manual sync needed

### ✅ Reliability
- Supabase as primary storage
- Local file as backup
- Automatic fallback if Supabase fails

### ✅ Visibility
- Comprehensive logging with `[CHANNELS]` prefix
- Clear success/failure indicators
- Easy debugging

### ✅ Simplicity
- No manual channel sync scripts needed
- No GitHub secrets updates required
- Just use the Web UI

---

## Migration from Old System

### First Run After Update

```
1. Workflow starts with new code
2. LoadConfig() tries Supabase → Empty
3. Falls back to local file
4. Loads existing channels from conf/channels.json
5. SaveConfig() uploads all to Supabase
6. ✓ Migration complete
```

**Logs**:
```
[CHANNELS] Loading channels from Supabase...
[CHANNELS] Failed to load from Supabase: no channels found
[CHANNELS] Loading channels from local file...
[CHANNELS] Loaded 30 channels from local file
[CHANNELS] Saving 30 channels to Supabase...
[CHANNELS] ✓ Channels saved to Supabase
```

### Subsequent Runs

```
1. Workflow starts
2. LoadConfig() loads from Supabase
3. ✓ All channels loaded from Supabase
```

---

## Summary

✅ **Supabase is now the single source of truth for channels**

✅ **All operations (add/edit/delete) automatically sync to Supabase**

✅ **Changes persist forever across all workflow runs**

✅ **Local file used as backup for reliability**

✅ **Comprehensive logging for debugging**

✅ **Automatic fallback if Supabase unavailable**

✅ **No manual sync scripts needed**

✅ **Just use the Web UI - everything is automatic!**

---

## Next Steps

1. **Deploy the changes**:
   ```bash
   git add .
   git commit -m "Feat: Supabase as single source of truth for channels"
   git push origin main
   ```

2. **Monitor first run**:
   - Watch for `[CHANNELS]` log messages
   - Verify channels load from Supabase
   - Check Supabase dashboard

3. **Test channel operations**:
   - Add a test channel
   - Pause/resume it
   - Delete it
   - Verify all changes in Supabase

4. **Verify persistence**:
   - Make changes
   - Wait for workflow to end
   - Start new run
   - Verify changes are preserved

---

**Status**: Ready to deploy! All channel operations now use Supabase! 🚀
