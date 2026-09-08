import os, json
from curl_cffi import requests as req
from curl_cffi.requests import Session
url = os.environ["SUPABASE_URL"]
key = os.environ["SUPABASE_SERVICE_ROLE_KEY"]
s = Session(impersonate="chrome", timeout=60)

def q(sql):
    r = s.post(url + "/pg/query", json={"query": sql}, headers={"apikey": key, "Authorization": "Bearer " + key})
    if r.status_code != 200:
        print("HTTP", r.status_code, r.text[:500])
        return None
    return r.json()

print("=== claimed+recording per node ===")
rows = q("""
SELECT assigned_node AS node,
       count(*) FILTER (WHERE status='claimed')  AS claimed,
       count(*) FILTER (WHERE status='recording') AS recording,
       count(*) FILTER (WHERE status='unassigned') AS unassigned,
       count(*) AS total
FROM channel_assignments
GROUP BY assigned_node
ORDER BY total DESC;
""")
for r in rows or []:
    print(f"{r['node'] or 'NULL':>18}  claimed={r['claimed']:>4}  rec={r['recording']:>3}  unassigned={r['unassigned']:>4}  total={r['total']:>5}")

print()
print("=== node heartbeat freshness ===")
rows = q("""
SELECT id, status, last_heartbeat, (now() - last_heartbeat) AS age
FROM recording_nodes
ORDER BY status, age DESC NULLS LAST;
""")
for r in rows or []:
    print(f"{r['id']:>18}  {r['status']:>9}  age={r['age']}")

print()
print("=== totals ===")
rows = q("SELECT count(*) AS total FROM channel_assignments;")
print(rows)