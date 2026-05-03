-- Complete Supabase Setup for GoOnDVR
-- Run this entire file in your Supabase SQL Editor
-- This will create all necessary tables for video uploads and channel persistence

-- ============================================================================
-- 1. VIDEO UPLOADS TABLE (for storing uploaded video links)
-- ============================================================================

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

-- Create policy to allow all operations
CREATE POLICY "Allow all operations on video_uploads" ON video_uploads
    FOR ALL
    USING (true)
    WITH CHECK (true);

-- Add comment
COMMENT ON TABLE video_uploads IS 'Stores video upload links from multiple hosting services (GoFile, TurboViPlay, VOE.sx, Streamtape)';

-- ============================================================================
-- 2. CHANNELS TABLE (for storing channel configurations)
-- ============================================================================

-- Create channels table to store channel configurations
CREATE TABLE IF NOT EXISTS channels (
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

-- Create indexes for faster lookups
CREATE INDEX IF NOT EXISTS idx_channels_username ON channels(username);
CREATE INDEX IF NOT EXISTS idx_channels_site ON channels(site);

-- Enable Row Level Security
ALTER TABLE channels ENABLE ROW LEVEL SECURITY;

-- Create policy to allow all operations
CREATE POLICY "Allow all operations on channels" ON channels
    FOR ALL
    USING (true)
    WITH CHECK (true);

-- Add comment
COMMENT ON TABLE channels IS 'Stores channel configurations for the recorder';

-- ============================================================================
-- VERIFICATION QUERIES
-- ============================================================================

-- Run these to verify tables were created successfully:
-- SELECT COUNT(*) as video_uploads_count FROM video_uploads;
-- SELECT COUNT(*) as channels_count FROM channels;
-- SELECT * FROM video_uploads ORDER BY upload_date DESC LIMIT 10;
-- SELECT * FROM channels ORDER BY created_at DESC LIMIT 10;
