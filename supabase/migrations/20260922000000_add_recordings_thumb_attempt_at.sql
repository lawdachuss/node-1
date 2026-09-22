-- thumb_attempt_at: fleet-wide lease/backoff for remote-thumbnail recovery.
--
-- SyncRemoteThumbnails runs on EVERY node (its doc comment calls it "safe to
-- call on every sweep from every node"), but its failure cooldown was an
-- in-process map.  So the same unfixable recording was re-downloaded by all 15
-- nodes inside the same five minutes: the live fleet logged 270
-- "[remote-thumb] <file>: no recoverable thumbnail" lines in one retained
-- window — exactly 18 files, retried on all 15 nodes — hammering the very
-- Streamtape ticket / Vidara og:image limiters the same code warns about.
--
-- This column turns that into one attempt per file per cooldown, fleet-wide:
--   * a node skips any row whose thumb_attempt_at is inside the cooldown, and
--   * for the rest it PATCHes the row with a conditional filter
--     (thumb_attempt_at IS NULL OR older than the cutoff) and only proceeds
--     when the PATCH actually matched — so exactly one node wins the race.
--
-- Nullable with no default, so existing rows and the DVR's own inserts are
-- unaffected.  The Go client degrades gracefully when this is not applied:
-- GetRecordingsMissingThumbnails falls back to the lease-less query and the
-- per-node cooldown still applies (see database.RecordingThumbAttemptColumnMissing).
--
-- Idempotent: safe to re-run.

ALTER TABLE recordings ADD COLUMN IF NOT EXISTS thumb_attempt_at TIMESTAMPTZ;

-- The sweep filters on "missing thumbnail AND attempt lease expired", so index
-- the lease column alongside the filenames it already scans.
CREATE INDEX IF NOT EXISTS idx_recordings_thumb_attempt_at
  ON recordings (thumb_attempt_at);

SELECT 'recordings.thumb_attempt_at added' AS progress;

-- Reload the PostgREST schema cache so the new column is queryable immediately.
SELECT pg_notify('pgrst', 'reload schema cache');
