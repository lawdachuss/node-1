# Supabase Setup Guide

This guide will help you set up Supabase to persist your video uploads and channel configurations across GitHub Actions workflow runs.

## Why Supabase?

Without Supabase, every time your GitHub Actions workflow restarts (every 5 hours):
- ❌ All video records are lost (you can't see past uploads in the UI)
- ❌ Channel changes are lost (added/modified channels don't persist)

With Supabase:
- ✅ All uploaded videos are visible in the Web UI forever
- ✅ Channels added via Web UI persist across workflow restarts
- ✅ Free tier is more than enough for this use case

## Setup Steps

### 1. Run SQL Migration

1. Go to your Supabase project: https://xhfbhgklqylmfmfjtgkq.supabase.co
2. Click **SQL Editor** in the left sidebar
3. Click **New Query**
4. Copy the entire contents of `supabase/migrations/000_complete_setup.sql`
5. Paste it into the SQL Editor
6. Click **Run** (or press Ctrl+Enter)

You should see:
```
Success. No rows returned
```

### 2. Verify Tables Were Created

Run this query in the SQL Editor:
```sql
SELECT 
    'video_uploads' as table_name, 
    COUNT(*) as row_count 
FROM video_uploads
UNION ALL
SELECT 
    'channels' as table_name, 
    COUNT(*) as row_count 
FROM channels;
```

You should see both tables with 0 rows (they're empty initially).

### 3. Configure GitHub Secrets

Go to your repository settings: https://github.com/vasud3v/record/settings/secrets/actions

Add these secrets (if not already added):

| Secret Name | Value | Where to Find |
|-------------|-------|---------------|
| `SUPABASE_URL` | `https://xhfbhgklqylmfmfjtgkq.supabase.co` | Already in your settings.json |
| `SUPABASE_API_KEY` | `eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...` | Already in your settings.json (the long token) |
| `SETTINGS_JSON` | Full contents of `settings.json` | Copy entire file |
| `CHANNELS_JSON` | Full contents of `conf/channels.json` | Copy entire file |
| `IMGBB_API_KEY` | `9f48b991edad5d980312c5f187c7ba7f` | Already in your settings.json |

### 4. Test the Setup

1. Go to **Actions** tab in your GitHub repository
2. Click **Continuous Recording (24/7) with Web UI**
3. Click **Run workflow** → **Run workflow**
4. Wait for the workflow to start (about 2-3 minutes)
5. Look for the Cloudflare Tunnel URL in the logs (e.g., `https://xyz-abc.trycloudflare.com`)
6. Open the URL in your browser

## How It Works

### Video Uploads
When a video is uploaded to GoFile/Streamtape/etc:
1. The app saves the download links to Supabase `video_uploads` table
2. The Web UI reads from Supabase to display all videos
3. Videos persist forever (even after workflow restarts)

### Channel Persistence
**On Workflow Start:**
1. Tries to load channels from Supabase `channels` table
2. If empty, uses `CHANNELS_JSON` secret as fallback
3. Syncs channels to Supabase for future runs

**During Recording:**
- Channels added/modified via Web UI are saved in the container

**On Workflow End:**
1. Extracts channels from Docker container
2. Syncs them back to Supabase
3. Next workflow run will load these updated channels

## Verification

### Check Video Uploads
Run this in Supabase SQL Editor:
```sql
SELECT 
    streamer_name,
    filename,
    gofile_link,
    upload_date
FROM video_uploads
ORDER BY upload_date DESC
LIMIT 10;
```

### Check Channels
Run this in Supabase SQL Editor:
```sql
SELECT 
    username,
    site,
    is_paused,
    resolution,
    framerate
FROM channels
ORDER BY created_at DESC;
```

## Troubleshooting

### Videos not showing in UI
1. Check if Supabase tables exist:
   ```sql
   SELECT table_name FROM information_schema.tables 
   WHERE table_schema = 'public';
   ```
2. Check if videos are being saved:
   ```sql
   SELECT COUNT(*) FROM video_uploads;
   ```
3. Check GitHub Actions logs for Supabase errors

### Channels not persisting
1. Check if channels table exists (see above)
2. Check if channels are being synced:
   ```sql
   SELECT COUNT(*) FROM channels;
   ```
3. Look for sync errors in GitHub Actions logs (search for "Syncing channels")

### Environment Variables Not Set
If you see errors like "Supabase credentials not set":
1. Verify secrets are added in GitHub repository settings
2. Check secret names match exactly (case-sensitive)
3. Re-run the workflow after adding secrets

## Free Tier Limits

Supabase free tier includes:
- ✅ 500 MB database storage (plenty for metadata)
- ✅ 1 GB file storage (we don't store videos, only links)
- ✅ 2 GB bandwidth per month
- ✅ 50,000 monthly active users
- ✅ Unlimited API requests

This is more than enough for storing video metadata and channel configs.

## Next Steps

After setup:
1. ✅ Run the SQL migration
2. ✅ Add GitHub secrets
3. ✅ Trigger a workflow run
4. ✅ Add a test channel via Web UI
5. ✅ Wait for workflow to complete
6. ✅ Trigger another workflow run
7. ✅ Verify the test channel persisted
8. ✅ Check that uploaded videos appear in the UI

## Support

If you encounter issues:
1. Check the GitHub Actions logs for errors
2. Run the verification queries in Supabase SQL Editor
3. Ensure all secrets are set correctly
4. Make sure the SQL migration ran successfully
