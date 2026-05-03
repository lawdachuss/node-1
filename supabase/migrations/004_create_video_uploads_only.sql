-- Create video_uploads table only (channels table already exists)
-- Run this in your Supabase SQL Editor

-- Drop the old gofile_uploads table if it exists (we're renaming to video_uploads)
DROP TABLE IF EXISTS gofile_uploads CASCADE;

-- Create table for storing video upload records from multiple hosts
CREATE TABLE IF NOT EXISTS video_uploads (
    id SERIAL PRIMARY KEY,
    streamer_name TEXT NOT NULL,
    filename TEXT,
    gofile_link TEXT,
    turboviplay_link TEXT,
    voesx_link TEXT,
    streamtape_link TEXT,
    thumbnail_link TEXT,
    upload_date TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Create indexes for faster queries
CREATE INDEX IF NOT EXISTS idx_video_uploads_streamer_name ON video_uploads(streamer_name);
CREATE INDEX IF NOT EXISTS idx_video_uploads_upload_date ON video_uploads(upload_date DESC);

-- Enable Row Level Security
ALTER TABLE video_uploads ENABLE ROW LEVEL SECURITY;

-- Drop existing policy if it exists
DROP POLICY IF EXISTS "Allow all operations on video_uploads" ON video_uploads;

-- Create policy to allow all operations
CREATE POLICY "Allow all operations on video_uploads" ON video_uploads
    FOR ALL
    USING (true)
    WITH CHECK (true);

-- Add comment
COMMENT ON TABLE video_uploads IS 'Stores video upload links from multiple hosting services (GoFile, TurboViPlay, VOE.sx, Streamtape)';

-- Verification query
SELECT 
    'video_uploads' as table_name, 
    COUNT(*) as row_count 
FROM video_uploads;
