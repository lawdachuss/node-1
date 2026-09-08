#!/usr/bin/env python3
"""Duplicate-capture verdict since the 06:08 UTC rollout, plus new-binary behavior."""
from pathlib import Path
from curl_cffi import requests as cffi_requests

env = {}
for line in Path(".env").read_text().splitlines():
    line = line.strip()
    if not line or line.startswith("#") or "=" not in line:
        continue
    k, _, v = line.partition("=")
    env[k.strip()] = v.strip().strip('"\'"')

base = env.get("SUPABASE_URL", "").rstrip("/")
key = env.get("SUPABASE_SERVICE_ROLE_KEY") or env.get("SUPABASE_API_KEY", "")

def sql(query):
    r = cffi_requests.post(
        base + "/pg/query",
        json={"query": query},
        headers={"apikey": key, "Authorization": f"Bearer {key}", "Content-Type": "application/json"},
        impersonate="chrome124",
        timeout=90,
    )
    if r.status_code >= 400:
        raise SystemExit(f"HTTP {r.status_code}: {r.text[:500]}")
    d = r.json()
    if isinstance(d, dict) and "error" in d:
        raise SystemExit(f"SQL error: {d['error']}")
    return d

ROLLOUT = "2026-09-05T06:08:00Z"

print("=== 1. DUPLICATE CAPTURES SINCE ROLLOUT (both starts after %s) ===" % ROLLOUT)
pairs = sql(
    "select a.username, a.instance_id as node_a, b.instance_id as node_b, "
    "a.created_at as t_a, b.created_at as t_b, "
    "round(abs(extract(epoch from (b.created_at - a.created_at)))) as gap_sec "
    "from recordings a join recordings b on a.username = b.username and a.id < b.id "
    f"where a.created_at > '{ROLLOUT}' and b.created_at > '{ROLLOUT}' "
    "and a.instance_id <> b.instance_id "
    "and abs(extract(epoch from (b.created_at - a.created_at))) < 600 "
    "order by a.created_at desc limit 20"
)
print(f"  overlapping pairs: {len(pairs)}")
for r in pairs:
    print(f"  {r['username']:<24} {r['node_a']}+{r['node_b']} gap={r['gap_sec']}s @ {r['t_a']}")

print("\n=== 2. DUPLICATES SPANNING THE ROLLOUT (one started before, one after) ===")
spans = sql(
    "select a.username, a.instance_id as node_a, a.created_at as t_a, "
    "b.instance_id as node_b, b.created_at as t_b "
    "from recordings a join recordings b on a.username = b.username and a.id < b.id "
    f"where a.created_at < '{ROLLOUT}' and b.created_at > '{ROLLOUT}' "
    "and a.instance_id <> b.instance_id "
    "and extract(epoch from (b.created_at - a.created_at)) < 7200 "
    "order by b.created_at desc limit 10"
)
print(f"  spanning pairs: {len(spans)}")
for r in spans:
    print(f"  {r['username']:<24} {r['node_a']}@{r['t_a']} then {r['node_b']}@{r['t_b']}")

print("\n=== 3. STALE RECORDING ROWS ON ALIVE OWNERS (ghost check) ===")
stale = sql(
    "select ca.assigned_node, ca.username, "
    "round(extract(epoch from (now() - ca.last_heartbeat))) as age_sec "
    "from channel_assignments ca "
    "where ca.status='recording' and (ca.last_heartbeat is null or ca.last_heartbeat < now() - interval '2 minutes') "
    "and ca.assigned_node in (select node_id from nodes where status in ('online','draining') and last_heartbeat > now() - interval '20 minutes') "
    "order by ca.assigned_node"
)
print(f"  stale-on-alive: {len(stale)}")
for r in stale:
    print(f"  {r['assigned_node']:<9} {r['username']:<24} age={r['age_sec']}s")

print("\n=== 4. NEW-BINARY EVENTS IN channel_logs (assignment-sync / re-pin / ghost / reset, last 6h) ===")
ev = sql(
    "select instance_id, message, created_at from channel_logs "
    "where created_at > now() - interval '6 hours' "
    "and (message ilike '%re-pin%' or message ilike '%ghost%' or message ilike '%assignment-sync%' "
    "or message ilike '%reassigned%' or message ilike '%no longer assigned%') "
    "order by created_at desc limit 40"
)
print(f"  events: {len(ev)}")
for r in ev[:25]:
    print(f"  {r['instance_id']:<9} {str(r['message'])[:115]}")

print("\n=== 5. NODE HEALTH ===")
for n in sql("select node_id, status, last_heartbeat from nodes order by node_id"):
    print(f"  {n['node_id']:<9} {n['status']:<9} hb={n['last_heartbeat']}")

print("\n=== 6. RECORDINGS STARTED SINCE ROLLOUT (per node) ===")
c = sql(f"select instance_id, count(*) as n from recordings where created_at > '{ROLLOUT}' group by instance_id order by instance_id")
print("  " + (", ".join(f"{r['instance_id']}={r['n']}" for r in c) if c else "(none)"))

print("\n=== 7. RECORDING/CLAIMED TOTALS ===")
for r in sql("select status, count(*) as n from channel_assignments group by status order by status"):
    print(f"  {r['status']:<12} n={r['n']}")