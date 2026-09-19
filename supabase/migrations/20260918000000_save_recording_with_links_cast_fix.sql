-- save_recording_with_links: fix Postgres 42804 ("You will need to rewrite or
-- cast the expression") that made every atomic metadata save fail and silently
-- fall back to the sequential SaveRecording → GetRecording → SaveUploadLinks
-- path (up to 10 retries per call).
--
-- Diagnosis, from the live fleet logs plus PostgREST's own OpenAPI spec:
--
--   column "timestamp" is of type timestamp with time zone but expression is of type text
--
-- The RPC read its jsonb arguments with the -> / ->> operators and fed the
-- results straight into INSERT.  PostgREST's REST endpoint casts each JSON value
-- to the destination column type for us, but plpgsql does not: ->> always yields
-- text and -> always yields jsonb, and neither has an *assignment* cast to the
-- real column types.  Postgres therefore rejected the whole statement during
-- parse analysis — before a single row was written — so the RPC never once
-- succeeded since it was introduced.
--
-- Mismatches fixed here (jsonb/text expression -> actual column type):
--   recordings.timestamp         text  -> timestamptz  (RFC3339 from the DVR)
--   recordings.tags              jsonb -> text[]
--   upload_links.recording_id    text  -> uuid
--   preview_images.recording_id  text  -> uuid
--   preview_images.uploaded_at   text  -> timestamptz
--
-- The varchar columns (username, filename, resolution, gender, host) and the
-- ::int / ::bigint / ::double precision columns were already fine: text ->
-- varchar is binary-coercible in assignment context, and the numeric operators
-- produce exactly the declared types.
--
-- v_id is now declared uuid so that `DELETE ... WHERE recording_id = v_id`, the
-- upload_links SELECT and the preview_images VALUES all compare/insert uuid to
-- uuid (index- and FK-friendly); only the return value is cast back to text, so
-- RETURNS text — and therefore the Go caller, which unmarshals a JSON string —
-- is unchanged.
--
-- Behaviour deliberately kept identical to the REST path it replaces:
--   * recordings.timestamp is NOT NULL and always supplied by the DVR, so an
--     empty/invalid value still raises rather than being silently rewritten to
--     now(); orphaned saves are handled by the existing orphan-recovery sweep.
--   * preview_images.uploaded_at is nullable with DEFAULT now(), and the Go
--     caller omits it with omitempty — COALESCE(..., now()) reproduces the
--     "column absent -> default applies" semantics of the REST upsert.
--
-- Idempotent: CREATE OR REPLACE, safe to re-run.
-- Apply in the Supabase SQL editor (or psql "$SUPABASE_DB_URL" -f thisfile),
-- then reload the cache so PostgREST picks up the new definition:
--     SELECT pg_notify('pgrst', 'reload schema cache');

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
  v_id         uuid;
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
    (p_rec->>'timestamp')::timestamptz,
    p_rec->>'room_title',
    -- jsonb array of strings -> text[].  jsonb_typeof() is NULL for a missing
    -- key, so a non-array (including NULL) lands on '{}' exactly as the old
    -- COALESCE(p_rec->'tags', '[]'::jsonb) intended.
    ARRAY(
      SELECT jsonb_array_elements_text(
        CASE WHEN jsonb_typeof(p_rec->'tags') = 'array'
             THEN p_rec->'tags'
             ELSE '[]'::jsonb END
      )
    ),
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
  -- recording_id ties the preview_images row to the recording just upserted
  -- so FK-based joins (preview_images.recording_id → recordings.id) work.
  -- Without it every row stays NULL and consumers cannot link the two tables.
  IF p_preview IS NOT NULL THEN
    INSERT INTO preview_images (
      recording_id, filename, thumbnail_url, sprite_url, preview_url,
      thumbnail_mirrors, sprite_mirrors, preview_mirrors,
      uploaded_at, instance_id
    ) VALUES (
      v_id,
      v_filename,
      p_preview->>'thumbnail_url',
      p_preview->>'sprite_url',
      p_preview->>'preview_url',
      COALESCE(p_preview->'thumbnail_mirrors', '{}'::jsonb),
      COALESCE(p_preview->'sprite_mirrors', '{}'::jsonb),
      COALESCE(p_preview->'preview_mirrors', '{}'::jsonb),
      COALESCE(NULLIF(p_preview->>'uploaded_at', '')::timestamptz, now()),
      p_preview->>'instance_id'
    )
    ON CONFLICT (filename) DO UPDATE SET
      recording_id      = EXCLUDED.recording_id,
      thumbnail_url     = EXCLUDED.thumbnail_url,
      sprite_url        = EXCLUDED.sprite_url,
      preview_url       = EXCLUDED.preview_url,
      thumbnail_mirrors = EXCLUDED.thumbnail_mirrors,
      sprite_mirrors    = EXCLUDED.sprite_mirrors,
      preview_mirrors   = EXCLUDED.preview_mirrors,
      uploaded_at       = EXCLUDED.uploaded_at,
      instance_id       = EXCLUDED.instance_id;
  END IF;

  RETURN v_id::text;
END;
$$;

GRANT EXECUTE ON FUNCTION save_recording_with_links(jsonb, jsonb, jsonb) TO anon;
GRANT EXECUTE ON FUNCTION save_recording_with_links(jsonb, jsonb, jsonb) TO authenticated;

SELECT 'save_recording_with_links casts fixed' AS progress;
