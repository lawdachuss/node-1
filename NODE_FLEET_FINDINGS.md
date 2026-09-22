# Fleet-wide node bug report — live trycloudflare tunnels (2026-09-22 ~05:05Z)

Method: roster pulled from Supabase `nodes` (18/18 reachable), then per node the full
`/api/logs` ring, `/api/files`, `/api/orphans`, `/api/uploads`. Cross-checked against the
persisted (non-truncated) `channel_logs`, `channel_assignments`, `disk_usage` and
`upload_journal` in Supabase via `node _live_sql.js`.

Artifacts: `_node_fault_sweep.js` (re-runnable), `_fault_sweep.log`, `_fault_sweep.json`,
`_fault_lines.txt` (2,317 raw fault lines tagged by node).

Note on coverage: the ring buffer is `logs.Default = NewBuffer(5000)` (`logs/logs.go:104`).
node-7 had already dropped 2,625/7,625 lines (34%) and node-11 2,550/7,550 (34%) — 5,313 lines
lost fleet-wide. A clean tail is **not** a clean history; the `channel_logs` numbers below are
the authoritative ones.

---

## CRITICAL

### 1. Stuck recordings: 0-byte output files pinned for hours, invisible to every health check
**66 zero-byte files** on disk fleet-wide, **53 of them >1h old and never cleaned**.
node-11: 25, node-7: 18, node-8: 4, node-9: 5, node-14: 3, node-6: 2, node-2/15/10/4: 0.

```
node-11 /api/orphans:  cloverfarewell_2026-09-22_04-13-42.mp4   0 B  age=1h0m0s
                       dennabetters_2026-09-22_04-13-44.mp4     0 B  age=1h0m0s
                       carrieann6995_2026-09-22_04-13-41.mp4    0 B  age=1h0m0s
                       babygirlinlace_2026-09-22_04-13-39.mp4   0 B  age=1h0m0s
                       (+ 21 more, all created 04:13:38–04:14:22)
```

The owning rows are simultaneously `status='recording'` with a fresh heartbeat every ~20s:

| username | node | status | is_live | live_checked_at | last_heartbeat |
|---|---|---|---|---|---|
| cloverfarewell | node-11 | recording | **false** | **null** | 19s ago |
| dennabetters | node-11 | recording | **false** | **null** | 20s ago |
| carrieann6995 | node-11 | recording | **false** | **null** | 19s ago |
| babygirlinlace | node-11 | recording | **false** | 2026-08-24 | 20s ago |
| goddess_marylin | node-5 | recording | **false** | **null** | 23s ago |
| studiowahines | node-3 | recording | **false** | **null** | 14s ago |
| alena_hot_sex | node-7 | recording | **false** | 2026-08-26 | 29s ago |

Trigger: the 04:13 session-start cohort — every file is timestamped inside a 45-second window
across nodes, matching the logged CF/session block:
`[manager] 3 channel(s) hit a session-failure signature (session cut or CF block) in the last
2m0s (threshold 3) — re-minting cookies before the whole node 404s` (node-15, node-17).

Why nothing recovers it — four independent guards all miss:
- `channel.cleanupLocked` (`channel/channel_file.go:226`) **does** delete zero-byte files, but only
  when the channel closes. A channel stuck in the recording loop never closes.
- `channel.CleanupOrphanedFiles` (`channel/channel_file.go:1245-1257`) skips any path present in
  `activeRecs` (`manager.ActiveRecordingFiles()`, built at `manager/manager.go:757`). A stuck
  channel still owns its path, so the empty file is skipped forever.
- `router.scanOrphanFiles` (`router/router_handler.go:1288`) never consults `activeRecs`, so the
  same file is *also* reported as an orphan — operator sees noise, not "stuck recording".
- The admin reconciliation skips size 0 when computing `Stuck`
  (`_fleet_matrix_check.js`: `if (!o.size || ...) continue`), so the node verdict stays
  **HEALTHY** while 25 broken files sit next to it.
- The coordinator cannot reclaim it either: `ReclaimChannels` deliberately skips
  `status='recording'` rows (`database/supabase.go:2583`+), so a stuck recording survives its
  node's death too.

Net effect: a channel occupies a recording slot, heartbeats as healthy, produces nothing, and
leaves an empty file on disk until the node is restarted.

### 2. Fleet-wide remote-thumbnail stampede: the same 18 files re-downloaded by all 15 nodes
270 `[remote-thumb]` failures in the retained window — **exactly 18 per node on every node**,
all inside 04:14–04:19. The filenames are identical across nodes:

```
15x [remote-thumb] bright_diamonds_054_2026-09-14_10-26-21.mp4: no recoverable thumbnail
15x [remote-thumb] sappysex_2026-09-15_16-13-08_1.mp4.merged.mp4: no recoverable thumbnail
15x [remote-thumb] Mr_Genghis_Khan_2026-09-22_00-50-16.mp4: no recoverable thumbnail
... (18 distinct files)
```

`manager/remote_thumb.go` states the intent — *"failures are cooled down per-node so the fleet
does not re-download the same unfixable file on every 30-minute tick"* — but the cooldown is
`remoteThumbLast` (`:24`), an **in-process, per-node map** (`:112-115`), and the doc comment on
`SyncRemoteThumbnails` says it is *"safe to call on every sweep from every node"*. There is no
cross-node claim/lease, so N nodes × the same unfixable set every 3h. It also resets on every
node restart.

Impact: 15× the upstream load on exactly the limiter the same comment warns about ("Streamtape's
ticket rate limiter"), 15 nodes concurrently writing `recordings`/`preview_images` for the same
filename, and `remoteThumbPacing = 3s` only throttles *successes* — these 18 fail-fast in a burst.
Only 24 recordings have no thumbnail at all, so the whole fleet is churning on a fixed 18-file
dead set.

### 3. VidMoly is *quota-capped*, not dead — and the error was misreported, so no node ever disabled it
**Probed live 2026-09-22 05:32 UTC** (`GET https://vidmoly.me/api/upload/server?key=<first key>`):

```json
{"status":429,"msg":"Daily API limit reached (50).","limit":50,"used_today":2416,"remaining_today":0}
```

The key is valid — the account is simply capped at **50 API requests/day and the fleet had spent
2,416**. The deployable bug behind the count: VidMoly reports that cap as `status:429`, and
`isUploadRateLimited` matches a bare `"429"`, so the `isVidMolyDailyLimit(err)` branch in
`uploader/vidmoly.go` was **unreachable**. Every file walked all 3 retries on a limit that only resets
at midnight, then surfaced `all keys exhausted` — which no host-disable branch matches either. So
the host was never skipped, and each of the 18 nodes rediscovered the cap on every file.

`channel_logs`, 24h, `log_level='error'`:

| count | signature | nodes |
|---|---|---|
| 2,049 | `VidMoly failed for ...: VidMoly upload failed: all keys exhausted` | 18 |
| 686 | `VidMoly failed 3 files in a row — disabling it for the rest of this run` | 18 |
| 676 | `[VidMoly] failed: VidMoly upload failed: all keys exhausted` | 18 |
| 15 | `VidMoly ... get upload server: server status not ok` | 7 |
| 12 | `VidMoly ... get upload server failed` | 7 |

`upload_journal`, 48h: **83 failed VidMoly rows across all 18 nodes** — the single largest failure
bucket. The auto-disable (`uploader/uploader.go:298`, `failingHostsThreshold = 3`) fires 686 times
per day because each node re-attempts after its session cycle. Every recording burns minutes on a
host that cannot succeed, on every node.

**Fixed** (`uploader/vidmoly.go`, `uploader/vidmoly_dailyapi_test.go`): the daily-limit check now
runs *before* the generic 429 matcher, so the host is skipped for ~24h on the first rejection — one
API request per node per day instead of three-per-key-per-file. The terminal error also keeps the
host's own wording (`… all keys exhausted (last rejection: … Daily API limit reached (50).)`) instead
of hiding it. A multi-key ring still rotates past only the spent key. VidMoly will re-enable itself
when the daily budget resets; to take it out entirely, add `VidMoly` to `DISABLED_UPLOAD_HOSTS`.

**Also fixed — the cap is shared, so discovery is now fleet-wide.** Detecting the cap locally still
left every OTHER node spending its own request to rediscover it (and again after each ~6h CI
restart). `upload_host_backoffs` (migration `20260922010000`) now carries the verdict between nodes:
`uploader.disableHostFor` publishes the expiry + reason, peers poll it every 5 minutes and skip the
host. Cost after the first discovery of the day: one request fleet-wide instead of eighteen.
Degrades to the old per-node behaviour when the table is absent (`database.HostBackoffTableMissing`).

### 4. Vidara + VOE.sx capacity failures, and partial-coverage persistence
- Vidara: 73 + 70 + 70 errors/24h — `Daily upload limit reached`, `Vidara hit its daily upload
  limit — skipping it for ~N hours (auto re-enables)`.
- VOE.sx: 14 journal failures/48h — `file body was already fully transmitted in a previous attempt
  but the response was lost` (the `fullSend` idempotency path, `uploader/uploader.go:121-129`). Fixed
  twice over: `fullSend` is now keyed per (host, file) instead of per host, and `uploader/voesx.go`
  has the same full-body guard as VidMoly, so the uploader's own retry loop never re-streams a file
  whose bytes already reached VOE.sx.
- 689 warnings/24h: `#/# hosts succeeded for <file> despite errors (# hosts still pending) —
  persisting partial` — recordings are being persisted with reduced mirror coverage.

With VidMoly effectively gone and Vidara capped daily, the host chain is thinner than the
`startup` line advertises (`48 retry workers, 12 pipelines/channel`).

---

## HIGH

### 5. Thumbnail/preview pipeline: the image hosts are NOT the bottleneck (corrected 2026-09-22)

> **Correction.** This section originally blamed rate-limited / circuit-broken image hosts. A 7-day
> query of the persisted logs does not support that. Three findings replace it.

| count (7d) | signature |
|---|---|
| 6,160 | `timed out after 3m0s waiting for **preview** asset` |
| 82 | `timed out after 3m0s waiting for thumbnail asset` |
| 42 | `timed out after 3m0s waiting for sprite asset` |
| 11 | `all hosts rejected or saturated` (the cross-host failure the original text assumed) |
| 55 | `preview: failed for …` (the preview actually *failing*) |
| 5,954 | `thumbnail stage … exceeded 15m0s` |

**5a. 98% of the 3-minute timeouts are the preview, and it almost never fails.** 6,160 preview
timeouts against 55 outright failures: the preview goroutine *overruns* the shared
`thumbnailAssetTimeout` budget and is abandoned by the collect, then finishes in the background
(the late `onHost` persistence saves it). So this is a budget/scheduling problem, not a host failure.
The preview is uniquely exposed because it is the only asset that must serially extract 12 clips →
concat → libwebp encode → `sess.Wait()` for every mirror, and its own internal budget
(`previewTimeout`, up to 45m, plus a 5-minute single-clip fallback) is *longer* than the 3-minute
deadline the collect holds it to — so any slow path is guaranteed to report a timeout.

**5a fixed (the counting half).** The abandon was logged at **ERROR** level, which is what made
6,160 scheduling decisions look like upload failures. It is now informational, and the outcome is
recorded separately by a tracker that follows the abandoned goroutine to its end: a late landing logs
an info success ("landed late after 12s — not a failure"), whereas the genuinely-missing cases are
what keep an error. The three cases are split so the count stays honest rather than merely smaller:

| outcome | level | why |
|---|---|---|
| landed after the abandon | INFO | the asset was delivered; ~98% of the 6,160 |
| finished empty | WARN | the goroutine already logged its own error, so an error here would double-count every real failure |
| never finished | ERROR | nothing else will ever report it — its own failure line is never written |

Fixing that exposed a second, latent bug in the same collect: the shared deadline was
`time.NewTimer(budget)`, and a timer delivers its value to exactly **one** ready receiver (the timer
channel is unbuffered since Go 1.23). With three assets selecting on it, only the first was ever
actually abandoned — the other two blocked inside the collect until their goroutine finished, which is
exactly the stall the shared deadline exists to prevent. It is now a closed channel
(`time.AfterFunc`), which broadcasts to all waiters. The `82` thumbnail and `42` sprite timeouts above
are probably an artefact of the same one-winner race: those assets are usually *finished* before the
deadline, so the preview (the slowest) almost always won the single timer value. Still open: 5a's
budget mismatch (the preview's internal 45m+5m exceeds the 3m deadline it is measured against), which
is why it is abandoned at all — the tracking makes it honest, it does not make the preview faster.

**5b. "fast seek failed … retrying with slow seek" is mostly a mislabelled ffmpeg-slot timeout.**
Of 1,330 such messages, the dominant error is `context deadline exceeded` (905) — which is exactly
what `config.AcquireFFmpegFor` returns (`config/config.go:437`, `return ctx.Err()`), not an ffmpeg
exit. `thumbnailFFmpegAcquireTimeout` is 30s, so a starved pool logs a *seek* failure, retries with
a slow seek (another 30s, and `-ss` after `-i` decodes from position 0), then falls back to a blank
frame (another acquire). Node-7 logged 1,007 of these and node-13 570 — the two most
pool-starved nodes. 1,102 clips/tiles exhausted every attempt (`both seeks failed`).

**5b fixed.** `runFFmpegFresh` now wraps the pool wait as `errFFmpegSlotStarved` and
`ErrFFmpegUnavailable` (`channel/ffmpeg_unavailable.go`), so the message names the saturated pool and
the wait instead of a seek that never happened. `IsFFmpegSlotStarved` lets the thumbnail, sprite-tile
and preview-clip paths skip the slow-seek retry, the blank-frame tile and the single-clip fallback —
all of which need the same slot - so a starved pool stops paying ~30s per step to learn nothing. The
same guard is re-applied after the slow seek, so a pool that starves mid-fallback skips the blank
step too instead of queueing one more doomed acquire. (The three probe/remux acquire sites already
named the pool correctly and are unchanged.)
Those paths therefore fail with the pool as the cause, which also feeds 5c's retry-later handling
(the phrase is part of the node-tool class). Deliberately unchanged: the blank-frame fallback still
runs for GENUINE seek failures, because a missing tile index makes the image2 assembly stop early.
*Note:* the histogram bin `0xffffffea` (242) is unrelated to all of this — it is ffmpeg's real EINVAL
media verdict and stays on the finalize path.

**5c. The missing thumbnails come from per-node ffmpeg spawn failures, not the host chain.**
Every recording with an empty `thumbnail_url` also has no sprite and no preview. `Mr_Genghis_Khan_2026-09-22_00-50-16.mp4`
(node-18) is representative — thumbnail, all 16 tiles and the preview all failed **within one second**
with `exit status 0xc000026b`, and the ffprobe duration probe failed too — silently, since
`generateThumbnailForFile` logs nothing when the probe itself errors (only the downstream
`duration unknown` consequence reaches the channel log). Zero image-host errors in that file's logs. Fleet-wide over 7 days: `0xc000026b` ×133, `0xffffffea` ×242, `0xbebbb1b7` ×234,
concentrated on node-18/node-7/node-13/node-14 — i.e. a node-wide window where ffmpeg cannot spawn,
during which `generateThumbnailForFile` treats an environment failure as "this file is not
thumbnailable" and the stage saves a thumbnail-less row.

So the Imgbox breaker and ImgPile 429s are **mirror-only** noise: `isPreferredPrimaryHost` is
Catbox/Pixhost/freeimage.host, Imgbox is skipped entirely for `.webp`, and the cross-host failure
that would actually cost a thumbnail happened 11 times in 7 days.

**5c fixed** (`channel/ffmpeg_unavailable.go`, `channel/channel_thumbnail.go`, `channel/pipeline.go`).
The generator now counts ffmpeg *spawn* failures (wrapping `errFn`, so every call site and all three
asset goroutines are covered) and reports `ThumbnailResult.Unavailable` when nothing was produced
**and** a process failed to start. The pipeline keeps the recording and retries on a flat 5-minute
delay (`pipelineRetryDelay`) instead of finalizing metadata and burning the 30s/60s/120s ramp inside
the same outage, and the logs now name the node instead of the file. `0xffffffea` (EINVAL) is
deliberately still treated as a media verdict, and the previously-silent ffprobe failure is now
logged — it was the earliest signal of the node-18 burst and nothing recorded it.
*Known limit:* with 3 retries × 5 minutes a window longer than ~15 minutes still exhausts the budget;
the file is kept and the recovery scan re-picks it, but a longer outage needs the operator alarm
(this change adds one) rather than a bigger retry cap.

### 6. Windows file-in-use races in finalize / output-dir move
```
finalize X: remove original before replace: remove videos/X.mp4:
  The process cannot access the file because it is being used by another process
output-dir: move X to D:\videos: could not remove source after copy: remove videos/X.mp4 ...
```
Plus 21 × `finalize X: exit status 0xc000026b — keeping original recording` (all on node-1) and
19 × `finalize X:  — retrying with corrupt-tail recovery`. Leaves duplicate `_1` files and
un-finalized recordings. Related: 21 × `min-duration: could not probe <file> (probe ... exit
status 0xc000026b) — deferred to pending`.

### 7. Sprite tile extraction failing
```
26x  sprite: tile N fast seek failed for <file>: exit status 0xffffffea — retrying with slow seek
21x  sprite: tile N fast seek failed for <file>: exit status 0xc000...  — retrying with slow seek
17x  sprite: tile N at Ns skipped (both seeks failed): exit status 0xc000...
29x  sprite: tile N at Ns: blank fallback also failed      (3 nodes)
```
Fast seek returns EINVAL, slow seek then dies (0xc000026b-class), and the blank fallback fails
too — so sprites ship with holes. `0xffffffea` is the same EINVAL the code already special-cases
for header-only fMP4 under 100 KB (`channel/channel_thumbnail.go:227`).

### 8. Cloudflare cookie path: primary grabber broken, only the fallback works
```
32x  ERROR: No Cloudflare challenge found.
17x  curl_cffi status: 403, url: https://www.cb.xxx/
```
Cookie refresh still completes (`[OK] Cookie grab succeeded - fresh cookies saved to Supabase`,
`Kept existing csrftoken (len 32)`) only because of the Playwright/Scrapling fallback. With 485
`media endpoint forbidden (session expired or stream stopped)` and 614 `stream stalled N time(s) —
backing off to full interval` errors/24h, session interruption is the dominant recording risk and
the primary refresh path is not contributing.

### 9. Stale `nodes.web_url` breaks cross-node probes during tunnel rotation
```
[coordinator] stuck-pause check: node-18 api/pool: client do:
  Get "https://ought-panels-roll-daily.trycloudflare.com/api/pool":
  dns_resolve ...: no such host
```
`ought-panels-roll-daily.trycloudflare.com` is **not** any of the 18 current `nodes.web_url`
values. Quick tunnels mint a new hostname on every restart, and the coordinator reads
`nodes.WebURL` fresh each cycle (`coordinator/stuck_pause.go:84,132`) — so the row itself was
stale at 04:18. During that window the stuck-pause fan-out silently skips the node and the
dashboard's node link 404s. The `tunnels` table is not a fallback: it holds a single row, inactive
since 2026-08-10.

### 10. Assignment leak: 165 orphan assignments, 379 rows not heartbeated in >7 days
`channel_assignments` totals **1,125 rows**:
- **165** reference a `username` that no longer exists in `channels` — unsatisfiable forever.
- 1,056 `status='claimed'`, only **2** heartbeated within 5 minutes.
- Stale `claimed` buckets: 413 × 1–7d, **379 × >7d** (oldest `2026-08-05`), 247 × 1–24h.

Symptom in the live ring on node-1 / node-4 / node-7 / node-11 / node-16:
`INFO [desibalika] channel not found (deleted/renamed), try again in 1 min(s)` **49–50× per node
per retained window**, i.e. a node polls a dead channel every minute indefinitely.

### 11. `status='recording'` + `is_live=false` — the liveness write clobbers recording channels
Every stuck row in #1 has `is_live=false`, several with `live_checked_at = null`. Cause:
`SetChannelsNotLive` (`database/supabase.go:2564`) step 1 is a blanket
`PATCH /channel_assignments?is_live=eq.true → is_live=false` with **no `status` filter**, and step 2
re-marks only the pairs the probe confirmed live this cycle. Any recording channel whose liveness
probe failed in that cycle ends up `recording` + `is_live=false`. Consumers that key off `is_live`
(fair-share claiming, release filters like `ReleaseNodeOfflineChannels`) are then misled.

---

## MEDIUM / LOW

12. **Ring buffer saturation** — node-7 dropped 2,625/7,625 (34%), node-11 2,550/7,550 (34%),
    node-2 138; 5,313 lines lost. The two worst nodes are exactly the two with the stuck-recording
    storm (#1), so the buffer loss correlates with the noise it is hiding.
13. **`/api/logs` ignores `?limit`** — `router/logs_handler.go:22` returns the entire ring
    regardless, so every probe pulls up to 5,000 entries per node.
14. **`/api/orphans` reports live recordings** — `scanOrphanFiles` omits the `activeRecs` check that
    `CleanupOrphanedFiles` and the thumbnail sweep both apply, so in-progress files
    (`age=0s`, 0 B) pollute the orphan list and the reconciliation counts.
15. **Orphan `age` resolution** — `time.Since(mtime).Round(time.Hour).String()`
    (`router/router_handler.go:1345`) rounds to the nearest hour, so `1h0m0s` covers 31–90 minutes
    and cannot distinguish a fresh failure from an old one.
16. **`live_checked_at` only advances for live rows** — rows that stay offline freeze at their last
    live check (babygirlinlace: 2026-08-24), so the column cannot be used to judge liveness freshness.

---

## CI / workflow — what the completed GitHub Actions logs show (all 18 repos)

Every `node-*` repo runs the same workflow (`.github/workflows/secure-rdp.yml` →
`scripts/keep-alive.ps1`). Pulling the last completed run from all 18 repos (18 × ~1,300 lines,
the 21:0x–22:0x sessions) surfaced five problems.

### C1. Sessions end **past GitHub's 6-hour job cap** — so the job log is never uploaded

| repo | job | ran for | conclusion | log |
|---|---|---|---|---|
| node-1 | 22:08:18 → 04:13:18 | **365m** | cancelled | `BlobNotFound` |
| node-7 | 22:07:06 → 04:12:07 | **365m** | cancelled | `BlobNotFound` |
| node-16 | 22:09:58 → 04:14:59 | **365m** | cancelled | `BlobNotFound` |
| node-5 | 22:09:22 → 04:08:47 | 359m | **failure** | `BlobNotFound` |
| node-15 | 22:09:34 → 04:06:31 | 357m | **failure** | `BlobNotFound` |

In every case **step 9 “Keep Runner Alive” never completed** and steps 10–12 never started. A job
that reaches 360 minutes is reclaimed by GitHub mid-step: the runner cannot upload the log blob (the
API really answers `BlobNotFound` — this is why the failed sessions could not be read at all), and no
further step runs. The arithmetic is 355-min keep-alive window + the post-loop handoff + the final
steps = **362–365 min**. `timeout-minutes: 420` was above the platform cap, so it never fired — the
platform won, and took the log with it. Notably the Go side already *assumed* the correct budget
(`coordinator.computeSessionDeadline`: “335m leaves a ~13m buffer before the **348m self-cancel / 360m
hard kill**”) — the workflow was simply running 17 minutes over its own documented contract.

**C1 fixed.** One budget block in `keep-alive.ps1` now defines `SESSION_WINDOW_MIN=340` +
`SESSION_RESERVE_MIN=8` = a **348-minute lifetime** (exactly what `coordinator.go` assumes), and
`RUN_DEADLINE`, the post-loop handoff deadline and the arbiter window all derive from it. The job's
`timeout-minutes` is now **355** — a backstop *under* the platform cap, so an overrun is cancelled
cleanly with its log intact instead of being reclaimed. The post-loop "wait for
`upload-complete.flag`" is also bounded by the handoff deadline now: it is a fixed 240s otherwise,
which on its own was enough to push a session over the cap.

### C2. The Init self-cancel window was shorter than a real session → duplicate sessions

The Init step cancelled itself only when another run had started `< 355` minutes ago, but a live
session is alive for up to ~365 minutes (it drains uploads long after the loop window). So a run
arriving at minute 356–365 concluded "that run can't still be recording", started on top of it, and
two runners recorded the same node's channels. The fleet’s own arbiter caught them — **12 ×
`(ARBITER) Duplicate in_progress run … — cancelling`** across the 18 sessions in one window — and the
cancelled duplicate lost its log too (C1).

**C2 fixed.** The test is now `SESSION_RUN_MAX_MIN` (the other job's actual timeout, 355), not the
loop window, and it is the same value in the workflow and in `keep-alive.ps1`'s arbiter.

### C3. Every `if: always()` step was skipped on a reclaimed session

Because the job was reclaimed rather than cancelled cleanly, a dead session never ran `Save tools
cache`, `Upload Tunnel URL`, `Restore Network`, or `Cleanup on Cancel` — and the script's own
tail (`Clear-NodeStatus offline` + dispatch the next run) never executed either. That leaves the node
row saying `online` with a `web_url` pointing at a tunnel that just died (section 9) and no
next-session dispatch. This is a consequence of C1, so the same fix applies: finish inside the job's
own timeout so the always() steps run.

### C4. `web_url` upsert gave up on transient edge errors (6 of 18 nodes in one session)

On nodes 1, 4, 7, 8, 13 and 16, at the same wall-clock minute (17:17–17:20Z), the 5-minute refresh
failed all three curl attempts and the REST fallback:

```
(WARN) web_url curl attempt 1/2/3 returned HTTP 530 / 502 / 404
(WARN) Invoke-RestMethod web_url update failed: … 502 (Bad Gateway)
(WARN) Failed to update web_url after all attempts
```

`530`/`502` are the Supabase front (Cloudflare) failing to reach the origin — a shared, transient
outage, not a node fault — but the upsert treats every code the same, retries for ~10s and then gives
up for the whole 5-minute window, leaving `nodes.web_url` stale. **Improved:** transient codes now
get an escalating (5s → 10s) backoff before the REST fallback, and a curl that produces no usable code
is reported as `000` (transport failure) instead of an empty HTTP code. The real staleness defence
remains the tunnel liveness check that blanks a dead URL.

### C5. A secret whose value is `-` masks **every hyphen** in every node's step output — NOT fixed

Across all 18 logs, the step output contains **zero hyphens**: `[2026-09-21 16:16:35]` prints as
`[2026***09***21 16:16***35]`, `-ErrorAction` as `***ErrorAction`, `chrome-win64` as
`chrome***win64`, `lawdachuss/node-16` as `lawdachuss/***`. The runner masks any occurrence of a
secret value, so this can only be a secret whose value is exactly `-`; it is a *log-legibility* bug
only (env values are unaffected), but it makes every step's output hard to read. `keep-alive.ps1`
already works around it in one place (`Show-Url` prints U+2011 instead of `-` for the tunnel URL) —
but the workaround is cosmetic and the underlying placeholder secret is still set.

**Not fixed** (it is a repository secret, not code, and I can neither read nor safely guess it). The
offender is one of the workflow's `env:` secrets — `DISABLED_UPLOAD_HOSTS` is *empty* (not masked),
and the SUPABASE_URL placeholder guard in `keep-alive.ps1` never fired, so it is NOT that one.
Clearing the placeholder restores every log line on all 18 repos at once.

### C6. Actions were still on the removed Node 20 runtime

Every run carried `##[warning]Node.js 20 actions are deprecated: actions/checkout@v4,
actions/setup-go@v5, actions/upload-artifact@v4`, kept alive only by the
`ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION=true` escape hatch. **Fixed:** the minimum Node-24 majors
(`checkout@v5`, `setup-go@v6`, `upload-artifact@v6`; `cache/restore`+`save` were already v5) and the
escape hatch is gone.

---

## Suggested priority

1. **#1 stuck recording** — reap `status='recording'` rows whose output file is 0 bytes past a
   threshold (`.pending`-style timeout), and make the admin reconciliation count zero-byte
   orphans instead of skipping them.
2. **#3/#4 host chain** — *(fixed: VidMoly daily-cap detection, the `upload_host_backoffs` fleet
   lease, the VOE.sx idempotent retry, per-(host,file) `fullSend`, and the keys-exhausted cooldown)*.
   Still open: raise/rotate the Vidara daily cap, and consider per-node VidMoly keys — the fleet
   backoff removes the waste, but 50 requests/day still cannot cover the whole fleet's uploads.
3. **#5 thumbnails** — *(re-diagnosed: see 5a–5c)*. The image-host pool is **not** the bottleneck.
   *(5a's counting, 5b and 5c fixed — see the notes in section 5.)* Still open: bring the preview's
   internal budget (45-minute encode deadline + 5-minute fallback) under the 3-minute asset deadline it
   is measured against, or the timeout keeps firing on every long recording by construction — 5a now
   reports that honestly instead of calling it a failure, but it does not make the preview faster.
4. **#2 backfill stampede** — *(fixed: `recordings.thumb_attempt_at` + a conditional-PATCH lease, see
   `supabase/migrations/20260922000000_add_recordings_thumb_attempt_at.sql` and
   `database.Client.ClaimRecordingThumbAttempt`)* — apply the migration to activate it; the client
   degrades to the old per-node cooldown until then.
5. **#10 assignment leak** — sweep assignments whose username no longer exists in `channels` and
   whose `last_heartbeat` is older than the node's session length.
6. **#9 stale web_url** — have nodes re-register `web_url` when the tunnel hostname changes, and
   expire node rows whose URL stops resolving.
7. **#11 liveness** — add `status=neq.recording` to the blanket clear in `SetChannelsNotLive`.
8. **#C5 masking secret** — clear the `-` placeholder secret; it is the only finding here that needs a
   repository setting rather than code, and it is the reason the fleet's CI logs read like
   `[2026***09***21]`. Everything else in the C-series is fixed and only needs this commit deployed.
