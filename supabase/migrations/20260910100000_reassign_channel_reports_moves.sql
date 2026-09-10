-- reassign_channel: report whether the row actually moved.
--
-- The controller relies on the RPC's return value to keep its in-memory
-- snapshot in sync with the database: after a real move it re-points the row
-- and shifts its load counters, which is what stops the sweep from re-picking
-- the same channel repeatedly within one tick. The previous void version
-- returned success even when its guarded UPDATE matched zero rows (the row had
-- already moved elsewhere, or flipped to 'recording' in between), so a stale
-- snapshot "moved" one channel six times in a single controller tick on the
-- live fleet (2026-09-10, chaturbate/avrora_jessie).
--
-- Guard semantics are UNCHANGED: assigned_node = p_from_node AND status <>
-- 'recording'. A row that is actively recording can still never be moved; a
-- race now surfaces as a reported no-op (empty SETOF → false) instead of a
-- silent success.
--
-- Apply in the Supabase SQL editor (or psql "$SUPABASE_DB_URL" -f thisfile).
-- The DROP is required because CREATE OR REPLACE cannot change a function's
-- return type in place. Safe to re-run.

DROP FUNCTION IF EXISTS reassign_channel(text, text, text, text);

CREATE OR REPLACE FUNCTION reassign_channel(p_username text, p_site text, p_from_node text, p_to_node text)
RETURNS SETOF boolean
LANGUAGE plpgsql
SECURITY DEFINER
AS $$
BEGIN
  RETURN QUERY
  UPDATE channel_assignments
     SET assigned_node = p_to_node,
         status        = 'claimed',
         assigned_at   = now(),
         updated_at    = now()
   WHERE username      = p_username
     AND site          = p_site
     AND assigned_node = p_from_node
     AND status        <> 'recording'
  RETURNING true;
END;
$$;

GRANT EXECUTE ON FUNCTION reassign_channel(text, text, text, text) TO anon;
GRANT EXECUTE ON FUNCTION reassign_channel(text, text, text, text) TO authenticated;
