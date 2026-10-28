-- Narrow the high-frequency worker claim scan to currently unassigned work.
-- This is a partial index: claimed/recording/paused rows do not consume index
-- space, while the coordinator can find the eligible (live/offline) usernames
-- without walking the full channel_assignments table.
--
-- On a large, busy production database, build the index with CREATE INDEX
-- CONCURRENTLY during a maintenance window, then apply this migration so the
-- IF NOT EXISTS statement is a no-op. CREATE INDEX CONCURRENTLY cannot run
-- inside a transaction, so do not blindly replace this file in a transaction
-- based migration runner.

CREATE INDEX IF NOT EXISTS channel_assignments_unassigned_live_username_idx
  ON public.channel_assignments (is_live, username)
  WHERE assigned_node IS NULL
    AND status = 'unassigned';
