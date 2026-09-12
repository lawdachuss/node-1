-- upload_journal.link
--
-- Persist the download URL alongside each successful upload journal row.
-- Previously the Supabase upload_journal carried host/status/error only; the
-- actual download link lived solely in the per-node local journal, which is
-- destroyed with the ephemeral runner.  When a node dies between a successful
-- host upload (journal written) and the upload_links save on the recordings
-- row, the link was unrecoverable and the recording was permanently stuck at
-- "recorded but not uploaded" with no way to rebuild it.
--
-- With this column the reconciliation sweep (server.ReconcileMissingUploadLinks)
-- can re-insert upload_links rows from journal successes, so a crash never
-- orphans a link that a host already received.
--
-- Idempotent: safe to re-run.

ALTER TABLE public.upload_journal
    ADD COLUMN IF NOT EXISTS link TEXT;