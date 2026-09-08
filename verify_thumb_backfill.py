import os, json
from curl_cffi.requests import Session

url = os.environ["SUPABASE_URL"]
key = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
s = Session(impersonate="chrome124")


def pg_query(sql):
    r = s.post(
        url + "/pg/query",
        json={"query": sql},
        headers={
            "apikey": key,
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
        },
        timeout=120,
    )
    if r.status_code >= 400:
        raise RuntimeError(f"HTTP {r.status_code}: {r.content[:400]}")
    return r.json()


# 1. Overall recount: how many recordings still missing a thumbnail_url
tot = pg_query("select count(*) as c from recordings;")[0]["c"]
miss = pg_query(
    "select count(*) as c from recordings "
    "where thumbnail_url is null or trim(thumbnail_url::text) = '';"
)[0]["c"]

print("=== POST-BACKFILL THUMBNAIL COUNT ===")
print(f"  Total recordings:              {tot:,}")
print(f"  Missing thumbnail_url:         {miss:,}")
print(f"  Have thumbnail_url now:        {tot - miss:,}")
print(f"  Improvement from before backfill: 53 -> {miss} missing ({(53 - miss)} fixed)")
print()

# 2. Verify the 43 Vidara recordings we patched now have thumbnail_url
check_sql = (
    "select count(*) as c "
    "from recordings r "
    "join upload_links l on l.recording_id = r.id "
    "where (r.thumbnail_url is null or trim(r.thumbnail_url::text) = '') "
    "  and l.host = 'Vidara' "
    "  and trim(l.url::text) <> '' "
    "  and r.timestamp <= '2026-09-05 09:07:59'::timestamptz;  -- last one we patched"
)
still_missing_vidara = pg_query(check_sql)[0]["c"]
print(f"  Vidara-hosted recordings still missing thumbnail_url (our batch): {still_missing_vidara}")
print()

# 3. Spot-check: list the first 5 patched rows' filename + thumbnail_url
spot_sql = (
    "select r.filename, r.thumbnail_url, r.username, r.timestamp "
    "from recordings r "
    "join upload_links l on l.recording_id = r.id "
    "where l.host = 'Vidara' "
    "  and r.thumbnail_url is not null "
    "  and trim(r.thumbnail_url::text) <> '' "
    "  and r.timestamp between '2026-08-30' and '2026-09-05 09:07:59' "
    "order by r.timestamp desc "
    "limit 5;"
)
spot = pg_query(spot_sql)
print("=== SPOT-CHECK: 5 patched recordings (filename -> thumbnail_url) ===")
for row in spot:
    fn = (row.get("filename") or "")[:45]
    tu = (row.get("thumbnail_url") or "")[:60]
    un = (row.get("username") or "")[:16]
    print(f"  {fn}  |  {un}  |  {tu}")
print()

# 4. The 3 no-Vidara recordings (uploaded to GoFile/Mixdrop) — still missing
no_vidara_sql = (
    "select r.filename, r.username, r.timestamp "
    "from recordings r "
    "where (r.thumbnail_url is null or trim(r.thumbnail_url::text) = '') "
    "  and not exists ("
    "    select 1 from upload_links l "
    "    where l.recording_id = r.id and l.host = 'Vidara'"
    ") "
    "order by r.timestamp desc "
    "limit 5;"
)
no_vidara = pg_query(no_vidara_sql)
print("=== 3 NO-VIDARA recordings (still missing, need different backfill source) ===")
for row in no_vidara:
    fn = (row.get("filename") or "")[:45]
    un = (row.get("username") or "")[:16]
    ts = (row.get("timestamp") or "")[:10]
    print(f"  {fn}  |  {un}  |  {ts}")
print(f"  (total {len(no_vidara)} such recordings, any without a Vidara link at all)")
print()

print("=== SUMMARY ===")
print(f"  Backfill target: 46 no-thumbnail recordings with a Vidara link")
print(f"  Patched:         43 (all with a recoverable og:image thumbnail)")
print(f"  Failed:           4 (no og:image on the Vidara embed page — fresh recordings)")
print(f"  Remaining missing thumbnail_url on recordings table: {miss} (down from 53)")
print()
print("ACTION ITEMS:")
print("  - The 4 'no og:image' failures (matty14118, _cannabitch, lallyrose69 09-05,")
print("    akatalinaandmikemar) are very recent — their embed pages may not have")
print("    rendered a thumbnail yet. Re-run the backfill in a few hours to retry.")
print("  - The 3 no-Vidara recordings need a different source (GoFile/Mixdrop/etc.)")
print("    or must stay without a thumbnail until their local video is recovered.")
print("  - The backfill set recordings.thumbnail_url = Vidara CDN URL directly;")
print("    those thumbnails display on the site immediately without re-hosting.")
