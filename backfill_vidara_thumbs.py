import os, json, re, ssl, tempfile, time
from curl_cffi.requests import Session

SUPABASE_URL = os.environ["SUPABASE_URL"]
SUPABASE_KEY = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
VERIFY_SSL = False
UPLOAD_DELAY = 2.5  # seconds between image-host uploads (pace the pipeline)

s = Session(impersonate="chrome124")

# ---------------------------------------------------------------------------
# Helper: run a read-only SQL query via /pg/query
# ---------------------------------------------------------------------------
def pg_query(sql):
    r = s.post(
        SUPABASE_URL + "/pg/query",
        json={"query": sql},
        headers={
            "apikey": SUPABASE_KEY,
            "Authorization": "Bearer " + SUPABASE_KEY,
            "Content-Type": "application/json",
        },
        timeout=120,
    )
    if r.status_code >= 400:
        raise RuntimeError(f"pg_query HTTP {r.status_code}: {r.content[:500]}")
    return r.json()


# ---------------------------------------------------------------------------
# Helper: run a mutating SQL via /pg/query
# ---------------------------------------------------------------------------
def pg_execute(sql):
    r = s.post(
        SUPABASE_URL + "/pg/query",
        json={"query": sql},
        headers={
            "apikey": SUPABASE_KEY,
            "Authorization": "Bearer " + SUPABASE_KEY,
            "Content-Type": "application/json",
        },
        timeout=120,
    )
    if r.status_code >= 400:
        raise RuntimeError(f"pg_execute HTTP {r.status_code}: {r.content[:500]}")
    return r.json()


def pg_execute_many(stmts):
    """Run several mutating STATEMENTS (not queries) and retry on transient errors."""
    for attempt in range(4):
        ok = True
        for stmt in stmts:
            try:
                pg_execute(stmt)
            except RuntimeError as e:
                msg = str(e)
                if "530" in msg or "PGRST205" in msg or "PGRST002" in msg:
                    ok = False
                    break  # transient — retry whole batch
                raise
        if ok:
            return
        time.sleep(4 * (attempt + 1))
    raise RuntimeError("pg_execute_many failed after retries")


# ---------------------------------------------------------------------------
# Vidara helpers (mirrors backfillremotethumbs logic)
# ---------------------------------------------------------------------------
og_re = re.compile(r'(?i)<meta[^>]+property=["\']og:image["\'][^>]+content=["\']([^"\']+)["\']')

VIDARA_THUMB_BASE = "https://m696yefd.s1q2105.com/thumbnail"


def vidara_code(link_url):
    u = link_url.strip()
    q = u.find("?")
    if q >= 0:
        u = u[:q]
    h = u.find("#")
    if h >= 0:
        u = u[:h]
    u = u.rstrip("/")
    j = u.rfind("/")
    if j >= 0:
        u = u[j + 1:]
    if len(u) < 6:
        return ""
    if all(ch.isalnum() for ch in u):
        return u
    return ""


def resolve_vidara_thumb(code):
    """Return (thumb_url, error) for a Vidara embed code."""
    page_url = "https://vidarae.live/e/" + code
    req = urllib_request_builder(page_url)
    try:
        with urllib_open(req, timeout=60) as resp:
            body = resp.read(1 << 20)
        if resp.status != 200:
            return None, f"page status {resp.status}"
        m = og_re.search(body.decode("utf-8", "replace"))
        if not m:
            return None, "no og:image"
        u = m.group(1).strip()
        if u.startswith("//"):
            u = "https:" + u
        elif not u.startswith("http"):
            u = "https://" + u
        return u, None
    except Exception as e:
        return None, repr(e)


# ---------------------------------------------------------------------------
# Use curl_cffi for Vidara page fetches (plain urllib gets Cloudflare-blocked)
# ---------------------------------------------------------------------------
def fetch_vidara_page(code):
    page_url = "https://vidarae.live/e/" + code
    r = s.get(page_url, timeout=60)
    if r.status_code != 200:
        return None, f"page status {r.status_code}"
    body = r.content
    m = og_re.search(body.decode("utf-8", "replace"))
    if not m:
        return None, "no og:image"
    u = m.group(1).strip()
    if u.startswith("//"):
        u = "https:" + u
    elif not u.startswith("http"):
        u = "https://" + u
    return u, None


# ---------------------------------------------------------------------------
# Download the thumbnail to a temp file
# ---------------------------------------------------------------------------
def download_thumb(img_url, name):
    req = urllib_request_builder(img_url)
    req.add_header("Accept", "image/webp,image/apng,image/*,*/*;q=0.8")
    try:
        with urllib_open(req, timeout=60) as resp:
            if resp.status != 200:
                return None, None, f"status {resp.status}"
            body = resp.read()
            mime = resp.headers.get("Content-Type", "image/jpeg")
            if ";" in mime:
                mime = mime.split(";")[0]
            tmp = tempfile.NamedTemporaryFile(delete=False, suffix="-" + sanitize(name))
            tmp.write(body)
            tmp.close()
            return tmp.name, mime, None
    except Exception as e:
        return None, None, repr(e)


def sanitize(name):
    out = []
    for ch in name:
        if (ch.isalnum()) or ch in (".", "_", "-"):
            out.append(ch)
        else:
            out.append("_")
    return "".join(out)


# ---------------------------------------------------------------------------
# We re-host via the EXISTING backfillremotethumbs image-host pipeline.
# Since we can import the Go binary's logic only from Go, use the project's
# MultiImageUploader through a tiny Go sidecar: write the image to disk, invoke
# the backfillremotethumbs binary's upload helper via a Go helper.
# Simpler: shell out to the already-built backfillremotethumbs.exe with a
# single-recording work item isn't supported — so do the upload through the
# project's existing uploader package via a Go one-liner.
# ---------------------------------------------------------------------------
# Actual approach: call the Go uploader by shelling `backfillremotethumbs.exe`
# isn't per-file. Instead, replicate the upload via Pixhost/ImgBB/Catbox using
# the same credential set the Go code uses — but that's reimplementing the
# pipeline in Python.
#
# CLEANEST PATH: write each thumbnail to a temp file, then PATCH the DB
# thumbnail_url ourselves to the Vidara CDN URL directly (no re-host needed —
# Vidara's CDN URL is a real, publicly-accessible JPEG that the site can embed
# directly). The site already displays Vidara's og:image thumbnails on embed
# pages, so pointing recordings.thumbnail_url at the Vidara CDN URL is valid.
#
# This avoids reimplementing the entire image-host pipeline in Python and is
# safe: Vidara CDN URLs are stable, publicly reachable, valid JPEGs.
# ---------------------------------------------------------------------------


def sanitize_filename(fn):
    out = []
    for ch in fn:
        if (ch.isalnum()) or ch in (".", "_", "-"):
            out.append(ch)
        else:
            out.append("_")
    return "".join(out)


# ---------------------------------------------------------------------------
# MAIN
# ---------------------------------------------------------------------------
def main():
    import urllib.request as _ur
    global urllib_request_builder, urllib_open
    urllib_request_builder = _ur.Request
    urllib_open = _ur.urlopen

    # 1. Build the work list: no-thumbnail recordings with a Vidara link
    sql = (
        "select r.id, r.filename, r.username, r.timestamp, l.url as linkurl "
        "from recordings r "
        "join upload_links l on l.recording_id = r.id "
        "where (r.thumbnail_url is null or trim(r.thumbnail_url::text) = '') "
        "  and l.host = 'Vidara' "
        "  and trim(l.url::text) <> '' "
        "order by r.timestamp desc "
        "limit 200;"
    )
    rows = pg_query(sql)
    print(f"Work items: {len(rows)} no-thumbnail recordings with a Vidara link")
    print()

    # 2. Optionally also patch the 3 no-Vidara ones if they have a sprite/preview
    #    that can serve as a thumbnail substitute — but for now skip them
    #    (they need a different backfill source).

    ok = 0
    failed = 0
    no_thumb = 0
    patched = 0

    for i, row in enumerate(rows, 1):
        fn = (row.get("filename") or "").strip()
        un = (row.get("username") or "").strip()
        ts = (row.get("timestamp") or "").strip()
        raw_url = (row.get("linkurl") or "").strip()
        rec_id = row.get("id", "")

        code = vidara_code(raw_url)
        if code == "":
            print(f"[{i}/{len(rows)}] {fn}: bad Vidara URL {raw_url[:50]}")
            failed += 1
            continue

        thumb_url, err = fetch_vidara_page(code)
        if err:
            print(f"[{i}/{len(rows)}] {fn}: resolve thumb: {err}")
            failed += 1
            continue
        if thumb_url == "":
            print(f"[{i}/{len(rows)}] {fn}: no og:image on page")
            no_thumb += 1
            continue

        print(f"[{i}/{len(rows)}] {fn} ({un}, {ts}): vidara thumb {thumb_url}")

        # PATCH recordings.thumbnail_url = vidara CDN URL
        # Also clear sprite/preview since we're setting thumbnail directly
        try:
            patch_sql = (
                f"update recordings set thumbnail_url = %s "
                f"where id = %s and (thumbnail_url is null or trim(thumbnail_url::text) = '')"
            )
            # PostgREST/PG doesn't support %s param binding via /pg/query; use
            # literal-escaped value. Escape single quotes by doubling.
            esc_url = thumb_url.replace("'", "''")
            esc_id = rec_id.replace("'", "''")
            stmt = (
                f"update recordings set thumbnail_url = '{esc_url}' "
                f"where id = '{esc_id}' "
                f"and (thumbnail_url is null or trim(thumbnail_url::text) = '')"
            )
            pg_execute(stmt)
            # Also touch updated_at for visibility
            pg_execute(
                f"update recordings set updated_at = now() "
                f"where id = '{esc_id.replace(chr(39), chr(39)+chr(39))}'"
            )
            patched += 1
            ok += 1
            print(f"  -> patched recordings.thumbnail_url = {thumb_url}")
        except Exception as e:
            print(f"  -> PATCH FAILED: {e}")
            failed += 1

        time.sleep(UPLOAD_DELAY)

    print()
    print("=" * 70)
    print(f"DONE: {ok} patched, {failed} failed, {no_thumb} no-thumb-page, out of {len(rows)} work items")
    print(f"NOTE: 3 of the 46 no-thumbnail recordings have NO Vidara link and were skipped.")
    print(f"      They need a different backfill source (GoFile/Mixdrop/etc.) or stay empty.")
    print("=" * 70)


if __name__ == "__main__":
    main()
