import os, json, re, urllib.request, ssl
from curl_cffi.requests import Session

url = os.environ["SUPABASE_URL"]
key = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
s = Session(impersonate="chrome124")

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

rows = s.post(
    url + "/pg/query",
    json={"query": sql},
    headers={
        "apikey": key,
        "Authorization": "Bearer " + key,
        "Content-Type": "application/json",
    },
    timeout=120,
).json()

og_re = re.compile(r'(?i)<meta[^>]+property=["\']og:image["\'][^>]+content=["\']([^"\']+)["\']')


def code_of(u):
    u = u.strip()
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
    if all((ch.isalnum()) for ch in u):
        return u
    return ""


def resolve(code):
    page_url = "https://vidarae.live/e/" + code
    req = urllib.request.Request(
        page_url,
        headers={"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"},
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
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


print(f"Checking {len(rows)} Vidara recordings for recoverable thumbnails...\n")
ok = 0
fail = 0
print(f"{'idx':<5} {'filename':<48} {'username':<18} thumb")
print("-" * 120)
for i, row in enumerate(rows, 1):
    fn = (row.get("filename") or "")[:47]
    un = (row.get("username") or "")[:17]
    raw_url = row.get("linkurl") or ""
    code = code_of(raw_url)
    if code == "":
        print(f"{i:<5} {fn:<48} {un:<18} BAD VIDARA URL {raw_url[:40]}")
        fail += 1
        continue
    thumb, err = resolve(code)
    if err:
        print(f"{i:<5} {fn:<48} {un:<18} FAIL: {err}")
        fail += 1
    else:
        print(f"{i:<5} {fn:<48} {un:<18} {thumb}")
        ok += 1

print(f"\nSummary: {ok} recoverable, {fail} failed/unrecoverable out of {len(rows)} Vidara records")
print(f"\nNOTE: Recordings NOT returned above (3 of the 46) have no Vidara upload_link.")
print("They were uploaded to other hosts (GoFile/Mixdrop/etc.) and cannot be")
print("backfilled via the remote-thumb recovery path — their local video files")
print("are gone, so their thumbnails need a different source or must stay empty.")
