package channel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/server"
)

// ─── Session continuity merge ───────────────────────────────────────────────
// Chaturbate's HLS token refreshes roughly every 20 minutes, which makes the
// recorder finalize the current file and start a new one (a "cycle").  Left
// alone this produces one ~20-minute fragment per live session.  To honour the
// "record continuously, split only at max duration" rule we merge consecutive
// cycles of the SAME live session into a single long recording.
//
// Each finalized cycle file is a valid, self-contained MP4/MKV.  We join them
// with ffmpeg's concat demuxer (-c copy), which re-bases the timelines so the
// merged file plays continuously.  We never merge across a max-duration cut
// (that is an intentional split), and we never destroy originals until the
// merge is verified — on any failure the original files are uploaded
// individually so nothing is ever lost.

type sessionMergeEntry struct {
	path              string
	endedContinuation bool
	updatedAt         time.Time
}

var (
	sessionMergeMu     sync.Mutex
	sessionMergeByUser = map[string]*sessionMergeEntry{}
	// sessionMergeLockByUser serializes merges for a single channel so two
	// finalized cycles can never be merged concurrently (which would race on
	// the shared running-merge file and the group map).
	sessionMergeLockByUser sync.Map // map[string]*sync.Mutex
)

func sessionMergeLock(user string) *sync.Mutex {
	v, _ := sessionMergeLockByUser.LoadOrStore(user, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// isContinuationReason reports whether endReason means the stream is still live
// and the next HLS cycle belongs to the SAME session — so fragments must be
// merged, not uploaded as separate recordings.  Chaturbate rotates its HLS
// token roughly every 20 minutes; that rotation surfaces as a stall whose
// reason is "stream session expired (no new segments)" or
// "...(HLS session/token) — reconnecting".  Both are continuations, not real
// session ends.  This matches how streamlink, yt-dlp and hls.js handle token
// expiry: they re-fetch a fresh token and keep the SAME recording going instead
// of finalizing and starting a new file every ~20 minutes.
func isContinuationReason(r string) bool {
	return strings.Contains(r, "reconnecting") ||
		strings.Contains(r, "stream session expired") ||
		strings.Contains(r, "no new segments")
}

func isMaxDurationReason(r string) bool {
	return r == "max duration or filesize reached"
}

// trySessionMerge decides whether finalPath should be merged into the running
// session for this channel instead of being uploaded on its own.  It returns
// true when the file has been consumed into a merge (the caller must NOT
// upload it individually); false when the file should proceed through the
// normal upload path (either it is a lone recording, or a merge failure forced
// a safe fallback to individual upload).
func (ch *Channel) trySessionMerge(finalPath, endReason string) bool {
	if server.Config == nil || (server.Config.FinalizeMode != "remux" && server.Config.FinalizeMode != "transcode") {
		return false
	}
	if _, err := os.Stat(finalPath); err != nil {
		return false
	}
	user := ch.Config.Username

	// Serialize merges for this channel so concurrent finalizations of the
	// same session can't race on the running-merge file.
	lock := sessionMergeLock(user)
	lock.Lock()
	defer lock.Unlock()

	sessionMergeMu.Lock()
	prev := sessionMergeByUser[user]
	sessionMergeMu.Unlock()

	switch {
	case isMaxDurationReason(endReason):
		// Intentional cut.  Flush the pre-cut group as its own file, then start
		// a fresh group with this file so later cycles merge into a new chunk.
		if prev != nil {
			MarkUploadDone(prev.path)
			ch.flushSessionEntry(prev)
		}
		sessionMergeMu.Lock()
		sessionMergeByUser[user] = &sessionMergeEntry{path: finalPath, endedContinuation: false, updatedAt: time.Now()}
		sessionMergeMu.Unlock()
		MarkUploadInFlight(finalPath)
		return true

	case isContinuationReason(endReason):
		// Continuation of the same live session.
		if prev == nil {
			sessionMergeMu.Lock()
			sessionMergeByUser[user] = &sessionMergeEntry{path: finalPath, endedContinuation: true, updatedAt: time.Now()}
			sessionMergeMu.Unlock()
			MarkUploadInFlight(finalPath)
			return true
		}
		merged, err := mergeTwoFiles(prev.path, finalPath)
		if err != nil {
			// Merge failed: keep both files and let the normal path upload them
			// individually so nothing is lost.  Release prev's in-flight marker
			// too — the log line says "uploading separately", so dropping only
			// the map entry (the old behavior) leaked prev as a stranded file:
			// still marked in-flight, never enqueued, invisible to every scan.
			ch.Error("session merge %s + %s failed: %s — uploading both separately", filepath.Base(prev.path), filepath.Base(finalPath), err.Error())
			sessionMergeMu.Lock()
			delete(sessionMergeByUser, user)
			sessionMergeMu.Unlock()
			MarkUploadDone(prev.path)
			return false
		}
		MarkUploadInFlight(merged)
		sessionMergeMu.Lock()
		sessionMergeByUser[user] = &sessionMergeEntry{path: merged, endedContinuation: true, updatedAt: time.Now()}
		sessionMergeMu.Unlock()
		return true

	default:
		// Session ended.  If we have a running group, merge it with this final
		// file and upload the result.  Otherwise this is a lone recording.
		if prev == nil {
			return false
		}
		if !prev.endedContinuation {
			// prev was a max-duration flush start; upload it, then let this
			// final file upload on its own (the cut boundary is respected).
			MarkUploadDone(prev.path)
			ch.flushSessionEntry(prev)
			return false
		}
		merged, err := mergeTwoFiles(prev.path, finalPath)
		if err != nil {
			// Same leak guard as the continuation branch: release the parked
			// file's marker so both halves actually reach the upload path.
			ch.Error("session merge (end) %s + %s failed: %s — uploading both separately", filepath.Base(prev.path), filepath.Base(finalPath), err.Error())
			sessionMergeMu.Lock()
			delete(sessionMergeByUser, user)
			sessionMergeMu.Unlock()
			MarkUploadDone(prev.path)
			return false
		}
		sessionMergeMu.Lock()
		delete(sessionMergeByUser, user)
		sessionMergeMu.Unlock()
		MarkUploadDone(prev.path)
		// Upload the finished, merged session recording.
		ch.MoveToOutputDir(merged, endReason)
		return true
	}
}

// flushSessionEntry uploads a stored (already-merged or single) session file so
// it is no longer held in the running group.
func (ch *Channel) flushSessionEntry(e *sessionMergeEntry) {
	if e == nil || e.path == "" {
		return
	}
	if _, err := os.Stat(e.path); err != nil {
		return
	}
	ch.MoveToOutputDir(e.path, "max duration or filesize reached")
}

// FlushHeldSessionMerge releases the file this channel parked in the
// session-merge hold, if any, by uploading it on its own.
//
// The hold is normally released by the NEXT cycle of the same session (a merge
// or a max-duration flush).  But when the stream ends with no further cycle —
// the HLS session expired and the channel went offline, or the final file was
// empty and deleted — nothing ever merges into the parked file.  It used to sit
// in the map forever: marked in-flight (so every re-claim hit the
// "already uploading — skipping duplicate" early-out), never enqueued, no
// recordings row, invisible to every recovery scan, until the runner was torn
// down and the disk wiped.  Called from ProcessPending, so every channel-stop
// handoff and session-boundary drain releases the hold before its uploads are
// awaited.
func (ch *Channel) FlushHeldSessionMerge() {
	if ch.Config == nil {
		return
	}
	user := ch.Config.Username
	lock := sessionMergeLock(user)
	lock.Lock()
	defer lock.Unlock()

	sessionMergeMu.Lock()
	e := sessionMergeByUser[user]
	delete(sessionMergeByUser, user)
	sessionMergeMu.Unlock()

	if e == nil || e.path == "" {
		return
	}
	if _, err := os.Stat(e.path); err != nil {
		return
	}
	ch.Info("session merge: flushing held recording %s — no further merge will occur, uploading on its own", filepath.Base(e.path))
	MarkUploadDone(e.path)
	ch.flushSessionEntry(e)
}

// mergeTwoFiles concatenates a then b into a single MP4/MKV via ffmpeg's concat
// demuxer (timeline-rebasing copy).  On success it removes both inputs and
// returns the merged file path.  On failure it leaves the inputs intact and
// returns an error.
func mergeTwoFiles(a, b string) (string, error) {
	outExt := ".mp4"
	if server.Config != nil && server.Config.FFmpegContainer == "mkv" {
		outExt = ".mkv"
	}
	stem := a
	for strings.HasSuffix(stem, ".merged"+outExt) {
		stem = strings.TrimSuffix(stem, ".merged"+outExt)
	}
	mergedPath := stem + ".merged" + outExt

	// Write to a unique temp file first so the output never collides with an
	// input (a re-merge reuses the stable name as its input).
	tmpPath := mergedPath + ".tmp" + outExt
	_ = os.Remove(tmpPath)

	listPath := mergedPath + ".concat.txt"
	// Use absolute paths so ffmpeg's concat demuxer doesn't resolve them
	// relative to the concat file's directory (which would double the prefix
	// when the inputs are already under videos/).
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		absA, absB = a, b // fall back to raw paths
	}
	list := fmt.Sprintf("file '%s'\nfile '%s'\n", escapeConcatPath(absA), escapeConcatPath(absB))
	if err := os.WriteFile(listPath, []byte(list), 0666); err != nil {
		return "", fmt.Errorf("write concat list: %w", err)
	}
	defer os.Remove(listPath)

	args := []string{"-nostdin", "-y", "-fflags", "+genpts", "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy"}
	if outExt == ".mp4" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, tmpPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := config.AcquireFFmpegHeavyFor(config.FFmpegHeavyAcquireTimeout); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("could not acquire ffmpeg slot to merge: %w", err)
	}
	defer config.ReleaseFFmpegHeavy()
	out, err := config.FFmpegCommandContext(ctx, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmpPath)
		msg := strings.TrimSpace(string(out))
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return "", fmt.Errorf("%s", msg)
	}

	// Validate the merged duration is (approximately) the sum of the inputs.
	inA, errA := VideoDurationSeconds(a)
	inB, errB := VideoDurationSeconds(b)
	if errA == nil && errB == nil {
		want := inA + inB
		got, errG := VideoDurationSeconds(tmpPath)
		if errG == nil && want > 0 && got < want*0.85 {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("merged duration %.1fs < 85%% of inputs %.1fs", got, want)
		}
	}

	// Success: publish the merged file FIRST, then remove the inputs.
	// The old order (remove inputs → rename) destroyed both recordings on
	// any rename failure: Windows os.Rename fails when the destination
	// exists (re-merges publish onto input a's own path — X.merged.mp4 +
	// next → dest == a, so a lingering lock on a is guaranteed to collide),
	// and os.Remove had already deleted both inputs.  tmp was then deleted
	// too, so the next ffmpeg call failed with "Impossible to open" and the
	// "uploading separately" fallback had nothing left to upload — the
	// dominant merge-failure class in the fleet logs (8 of 11 in 24h).
	// Publishing first means a failed rename leaves tmp + both inputs, and
	// the "keep both" fallback is actually true.
	//
	// Re-merge: dest == a (X.merged.mp4 + next → same stable name).  Windows
	// rename fails onto an existing destination, so side-step the OLD merged
	// file to a scratch name, publish tmp into the vacated slot, then consume
	// the leftovers.  Any failure restores a and keeps tmp + b intact.
	if mergedPath == a {
		oldPath := a + ".consumed"
		_ = os.Remove(oldPath)
		if err := os.Rename(a, oldPath); err != nil {
			return "", fmt.Errorf("stage re-merge input: %w", err)
		}
		if err := os.Rename(tmpPath, mergedPath); err != nil {
			if rerr := os.Rename(oldPath, a); rerr != nil {
				// The restore also failed: a is stranded at oldPath (.consumed)
				// and tmp holds the merged a+b output.  NEVER delete either —
				// deleting tmp here would be the one new data-loss path.  Both
				// are recovered by the orphan scan: tmp is a normal .mp4 main
				// video, and CleanupOrphanedFiles restores .consumed strays.  The
				// caller's "upload both separately" fallback must not be lied to,
				// so both surviving paths are reported.
				return "", fmt.Errorf("publish re-merge: %w (restore of %s also failed: %v — original preserved at %s, merged output preserved at %s for orphan recovery)", err, filepath.Base(a), rerr, filepath.Base(oldPath), filepath.Base(tmpPath))
			}
			// Restored cleanly: both inputs are intact, so tmp (a third copy of
			// the combined content) may be dropped to avoid triple duplication.
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("publish re-merge: %w", err)
		}
		// Merged content is durably published.  Removing leftovers is now
		// cosmetic: a locked input just lingers for the orphan scan, which
		// uploads it separately — duplication at worst, never data loss.
		_ = os.Remove(oldPath)
		if rmErr := os.Remove(b); rmErr != nil && !os.IsNotExist(rmErr) {
			recoveryLogf(filepath.Base(b), "merged file published but consumed input could not be removed (locked?) — leaving it for the orphan scan: %v", rmErr)
		}
		return mergedPath, nil
	}
	if err := os.Rename(tmpPath, mergedPath); err != nil {
		// Windows: the destination may exist (crash leftovers between runs).
		// Never destroy the merged content: drop a stale destination and
		// retry once; if it still fails, PRESERVE tmp and report — callers
		// keep both inputs and fall back to individual uploads.
		if rmErr := os.Remove(mergedPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return "", fmt.Errorf("rename merged output (dest %s unremovable): %w", filepath.Base(mergedPath), rmErr)
		}
		if err2 := os.Rename(tmpPath, mergedPath); err2 != nil {
			return "", fmt.Errorf("rename merged output: %w (merged tmp preserved at %s)", err2, filepath.Base(tmpPath))
		}
	}
	// Inputs are consumed only after the output is durably in place.  A
	// locked input (Windows AV/indexer) is left for the orphan scan rather
	// than treated as a failure — the merge itself succeeded.
	if rmErr := os.Remove(a); rmErr != nil && !os.IsNotExist(rmErr) {
		recoveryLogf(filepath.Base(a), "merged file published but input could not be removed (locked?) — leaving it for the orphan scan: %v", rmErr)
	}
	if rmErr := os.Remove(b); rmErr != nil && !os.IsNotExist(rmErr) {
		recoveryLogf(filepath.Base(b), "merged file published but input could not be removed (locked?) — leaving it for the orphan scan: %v", rmErr)
	}
	return mergedPath, nil
}

// escapeConcatPath quotes a path for ffmpeg's concat demuxer list format and
// normalizes Windows separators so absolute paths resolve correctly.
func escapeConcatPath(p string) string {
	p = filepath.ToSlash(p)
	return strings.ReplaceAll(p, "'", "'\\''")
}
