-- recordings_without_upload_links: the fleet's "no host at all" list.
--
-- A recordings row is written at ENQUEUE time (save_recording_basics), before any
-- host upload, so a row is NOT proof that the file's content reached a host.  The
-- only such proof is an upload_links row.  Anything that asks "is this recording
-- safe in the cloud?" — the /api/orphans list, the admin reconciliation panel and
-- the startup link-restore sweep — therefore needs the set of recordings with ZERO
-- upload_links.
--
-- PostgREST cannot answer that question directly:
--   * upload_links has no foreign key to recordings in this schema, so an embedded
--     select (`recordings?select=filename,upload_links(host)`) is rejected with
--     PGRST200;
--   * diffing client-side means downloading every upload_links row (~125k of them,
--     against ~27k recordings).
--
-- This one read-only function answers it exactly, and returns names only, so the
-- payload is the size of the broken set (tens to hundreds of rows) rather than of
-- the fleet.
--
-- Non-SECURITY-DEFINER on purpose: recordings/upload_links both carry a public
-- SELECT policy, so an invoker-rights read works for anon/authenticated as well as
-- for the service role, and no security-advisor lint (0028/0029) is introduced.
-- No SECURITY INVOKER keyword needed either — that is the default.
--
-- Idempotent: safe to re-run.  Apply in the Supabase SQL editor, or with
-- psql "$SUPABASE_DB_URL" -f thisfile.  Callers degrade to "no host index
-- available" while this is missing (database.NoHostsFuncMissing).

CREATE OR REPLACE FUNCTION recordings_without_upload_links()
RETURNS TABLE (filename text)
LANGUAGE sql
STABLE
SET search_path = ''
AS $$
  SELECT r.filename
  FROM public.recordings r
  WHERE r.filename IS NOT NULL
    AND NOT EXISTS (
      SELECT 1
      FROM public.upload_links ul
      WHERE ul.recording_id = r.id
    );
$$;

GRANT EXECUTE ON FUNCTION recordings_without_upload_links() TO anon;
GRANT EXECUTE ON FUNCTION recordings_without_upload_links() TO authenticated;
