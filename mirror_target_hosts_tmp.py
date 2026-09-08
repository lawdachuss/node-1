import os, re
from collections import Counter
from curl_cffi.requests import Session

url = os.environ["SUPABASE_URL"]
key = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
s = Session(impersonate="chrome124")


def q(sql):
    r = s.post(url + "/pg/query", json={"query": sql}, headers={"apikey": key, "Authorization": "Bearer " + key, "Content-Type": "application/json"}, timeout=120)
    if r.status_code >= 400:
        raise RuntimeError(f"HTTP {r.status_code}: {r.content[:500]}")
    return r.json()


rows = q("""
select p.filename, p.thumbnail_url, p.sprite_url, p.preview_url,
       r.timestamp::text as ts
from preview_images p
join recordings r on r.filename = p.filename
where r.timestamp >= now() - interval '7 days'
  and (p.thumbnail_mirrors is null or p.thumbnail_mirrors::text in ('{}','null',''))
  and (p.sprite_mirrors is null or p.sprite_mirrors::text in ('{}','null',''))
  and (p.preview_mirrors is null or p.preview_mirrors::text in ('{}','null',''))
order by r.timestamp desc;
""")
print(f"Target rows (last 7d, all three mirror sets empty): {len(rows)}\n")


def host_of(u):
    if not u:
        return "(empty)"
    m = re.match(r"https?://([^/]+)", u)
    return m.group(1) if m else u[:30]


th = Counter(host_of(r["thumbnail_url"]) for r in rows)
sp = Counter(host_of(r["sprite_url"]) for r in rows)
pv = Counter(host_of(r["preview_url"]) for r in rows)
print("thumbnail_url hosts:", dict(th))
print("sprite_url hosts:   ", dict(sp))
print("preview_url hosts:  ", dict(pv))

print("\nSample 8 rows:")
for r in rows[:8]:
    print(f"  {r['ts'][:16]}  {r['filename'][:44]}")
    print(f"      thumb:   {(r['thumbnail_url'] or '(none)')[:80]}")
    print(f"      sprite:  {(r['sprite_url'] or '(none)')[:80]}")
    print(f"      preview: {(r['preview_url'] or '(none)')[:80]}")
