-- save_recording_with_links: atomic metadata save for the upload pipeline.
--
-- The upload pipeline previously made 3 sequential Supabase REST calls
-- (SaveRecording → GetRecording → SaveUploadLinks) with up to 10 retries
-- each.  Under Supabase load this could stall a pipeline for minutes.  This
-- RPC replaces all three with a single transaction:
--
--   1. Upsert the recordings row (including thumbnail/sprite/preview URLs).
--   2. Delete stale upload_links for this recording and insert fresh ones.
--   3. Upsert preview_images so the UI can read thumbnails without a
--      separate backfill sweep.
--
-- SECURITY DEFINER lets the anon-key caller write to tables that may have
-- restrictive RLS policies.  The function runs inside a single transaction,
-- so a failure in any step rolls back everything — no orphaned rows.
--
-- Apply in the Supabase SQL editor (or psql "$SUPABASE_DB_URL" -f thisfile).

CREATE OR REPLACE FUNCTION save_recording_with_links(
  p_rec         jsonb,
  p_links       jsonb,
  p_preview     jsonb
)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
AS $$
DECLARE
  v_id         text;
  v_filename   text;
  v_thumb      text;
  v_sprite     text;
  v_preview    text;
  v_embed_url  text;
BEGIN
  -- ── 1. Upsert recording ────────────────────────────────────────────
  v_filename  := p_rec->>'filename';
  v_thumb     := p_rec->>'thumbnail_url';
  v_sprite    := p_rec->>'sprite_url';
  v_preview   := p_rec->>'preview_url';
  v_embed_url := p_rec->>'embed_url';

  INSERT INTO recordings (
    username, filename, timestamp, room_title, tags, viewers,
    resolution, framerate, filesize, duration, gender, end_reason,
    embed_url, thumbnail_url, sprite_url, preview_url,
    instance_id
  ) VALUES (
    p_rec->>'username',
    v_filename,
    p_rec->>'timestamp',
    p_rec->>'room_title',
    COALESCE(p_rec->'tags', '[]'::jsonb),
    (p_rec->>'viewers')::int,
    p_rec->>'resolution',
    (p_rec->>'framerate')::int,
    (p_rec->>'filesize')::bigint,
    (p_rec->>'duration')::double precision,
    p_rec->>'gender',
    p_rec->>'end_reason',
    v_embed_url,
    v_thumb,
    v_sprite,
    v_preview,
    p_rec->>'instance_id'
  )
  ON CONFLICT (filename) DO UPDATE SET
    username      = EXCLUDED.username,
    timestamp     = EXCLUDED.timestamp,
    room_title    = EXCLUDED.room_title,
    tags          = EXCLUDED.tags,
    viewers       = EXCLUDED.viewers,
    resolution    = EXCLUDED.resolution,
    framerate     = EXCLUDED.framerate,
    filesize      = EXCLUDED.filesize,
    duration      = EXCLUDED.duration,
    gender        = EXCLUDED.gender,
    end_reason    = EXCLUDED.end_reason,
    embed_url     = EXCLUDED.embed_url,
    thumbnail_url = EXCLUDED.thumbnail_url,
    sprite_url    = EXCLUDED.sprite_url,
    preview_url   = EXCLUDED.preview_url,
    instance_id   = EXCLUDED.instance_id,
    updated_at    = now()
  RETURNING id INTO v_id;

  -- ── 2. Upload links (delete-then-insert is atomic per-transaction) ─
  IF p_links IS NOT NULL AND jsonb_array_length(p_links) > 0 THEN
    DELETE FROM upload_links WHERE recording_id = v_id;

    INSERT INTO upload_links (recording_id, host, url, instance_id)
    SELECT
      v_id,
      l->>'host',
      l->>'url',
      l->>'instance_id'
    FROM jsonb_array_elements(p_links) l;
  END IF;

  -- ── 3. Preview images ──────────────────────────────────────────────
  IF p_preview IS NOT NULL THEN
    INSERT INTO preview_images (
      filename, thumbnail_url, sprite_url, preview_url,
      thumbnail_mirrors, sprite_mirrors, preview_mirrors,
      uploaded_at, instance_id
    ) VALUES (
      v_filename,
      p_preview->>'thumbnail_url',
      p_preview->>'sprite_url',
      p_preview->>'preview_url',
      COALESCE(p_preview->'thumbnail_mirrors', '{}'::jsonb),
      COALESCE(p_preview->'sprite_mirrors', '{}'::jsonb),
      COALESCE(p_preview->'preview_mirrors', '{}'::jsonb),
      p_preview->>'uploaded_at',
      p_preview->>'instance_id'
    )
    ON CONFLICT (filename) DO UPDATE SET
      thumbnail_url     = EXCLUDED.thumbnail_url,
      sprite_url        = EXCLUDED.sprite_url,
      preview_url       = EXCLUDED.preview_url,
      thumbnail_mirrors = EXCLUDED.thumbnail_mirrors,
      sprite_mirrors    = EXCLUDED.sprite_mirrors,
      preview_mirrors   = EXCLUDED.preview_mirrors,
      uploaded_at       = EXCLUDED.uploaded_at,
      instance_id       = EXCLUDED.instance_id;
  END IF;

  RETURN v_id;
END;
$$;

GRANT EXECUTE ON FUNCTION save_recording_with_links(jsonb, jsonb, jsonb) TO anon;
GRANT EXECUTE ON FUNCTION save_recording_with_links(jsonb, jsonb, jsonb) TO authenticated;
