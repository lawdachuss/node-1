package channel

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrFFmpegUnavailable marks a failure where ffmpeg could not RUN at all, as
// opposed to ffmpeg running and rejecting the media.
//
// The distinction matters because the two need opposite handling.  A rejected
// file will be rejected forever: retrying is wasted work and the recording is
// best finalized without the asset.  A node where the process cannot start is
// temporarily broken; the SAME file thumbnails fine once it recovers, so
// finalizing without the asset throws away a thumbnail that was seconds away,
// and re-running immediately just burns the retry budget inside the broken
// window.
//
// Live evidence this exists for (7 days, fleet): every recording with an empty
// thumbnail_url also had no sprite and no preview, and the per-file log shows
// the thumbnail, all 16 sprite tiles, the preview AND the ffprobe duration probe
// failing with one Windows status inside a single second —
// `Mr_Genghis_Khan_2026-09-22_00-50-16.mp4` on node-18:
//
//	03:26:49  preview: duration unknown …      (the probe failed, silently)
//	03:26:49  sprite:  duration unknown …
//	03:26:49  thumb:   fast seek failed: exit status 0xc000026b
//	03:26:50  thumb:   failed for …: exit status 0xc000026b
//	03:26:50  preview: failed for …: exit status 0xc000026b
//
// Nothing in that log is about the file, yet the pipeline treated it as an
// unthumbnailable recording: it burned all 3 retries ~30s/60s/120s apart (all
// inside the same outage) and gave up with the row still missing its thumbnail.
var ErrFFmpegUnavailable = errors.New(ffmpegUnavailablePhrase)

// ffmpegUnavailablePhrase is BOTH the sentinel's wording and one of the
// classifier signals below, and the two must agree.  The retry path only has the
// persisted error TEXT to work with (the in-memory spawn-failure counter does not
// survive a restart), so a sentinel that the text matcher does not recognise is
// silently invisible to pipelineRetryDelay — which would quietly restore the
// 30s/60s/120s ramp this exists to avoid.
// TestNodeToolFailureErrorNamesTheNodeAndClassifies asserts the agreement.
//
// Worded for every cause in the class, not just a failed spawn: the pool
// being saturated means no ffmpeg ever ran either, and that must read the same
// way to an operator.
const ffmpegUnavailablePhrase = "this node could not run ffmpeg"

// errFFmpegSlotStarved is the specific reason when the shared lightweight ffmpeg
// pool had no free slot within the wait budget — a capacity problem on this
// node, not a problem with the file or with the seek.
//
// It used to surface as a bare "context deadline exceeded" (that is literally
// what config.AcquireFFmpegFor returns: `return ctx.Err()`), which the callers
// then reported as "fast seek failed … retrying with slow seek".  Live evidence:
// 905 of the fleet's 1,330 "fast seek failed" messages were this, concentrated
// on the two most loaded nodes (node-7 1,007, node-13 570).  The mislabelling
// mattered — it sent this investigation hunting codec bugs — and so did the
// retry: the slow-seek fallback needs the same slot, so it could only time out
// again, after which a blank frame was burned one more slot ahead of the
// contact-sheet assembly.
var errFFmpegSlotStarved = errors.New("ffmpeg pool saturated — no free slot within the wait budget")

// ffmpegSpawnFailureSignals are the error fragments that mean the process never
// got to run.  Matched case-insensitively against the formatted error text,
// because that is all the callers have (the failures surface as `exec.ExitError`
// wrapped into a message, and the asset goroutines log rather than return).
//
// Deliberately NOT included: `0xffffffea` (-22 EINVAL).  That one is a genuine
// media verdict — generateThumbnailForFile's own header comment documents it as
// the code ffmpeg returns for header-only fMP4 from failed streams — so it must
// keep finalizing the recording instead of retrying forever.
var ffmpegSpawnFailureSignals = []string{
	ffmpegUnavailablePhrase, // keeps this in sync with ErrFFmpegUnavailable
	"0xc000026b",            // STATUS_DLL_INIT_FAILED_LOGOFF — 133 fleet-wide in 7 days
	"0xc0000135",            // STATUS_DLL_NOT_FOUND
	"0xc0000142",            // STATUS_DLL_INIT_FAILED
	"0xc000007b",            // STATUS_INVALID_IMAGE_FORMAT
	// Seen ONLY alongside the statuses above, in the same per-node bursts
	// (node-18: 49 + 94, node-7: 48 + 70, node-8: 0 + 70), never as a lone
	// media verdict.  Classifying it here is also the safe direction: a false
	// positive costs a delayed retry, a false negative costs the thumbnail.
	"0xbebbb1b7",
	"executable file not found",
	"cannot find the file specified",
	"cannot find the path specified",
	"access is denied",
	"not enough memory resources", // CreateProcess() failure, also not about the file
}

// IsFFmpegSpawnFailureText reports whether an error message means the ffmpeg
// process could not be started (as opposed to running and failing).
func IsFFmpegSpawnFailureText(errText string) bool {
	if errText == "" {
		return false
	}
	msg := strings.ToLower(errText)
	for _, sig := range ffmpegSpawnFailureSignals {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// IsFFmpegUnavailable reports whether err is the node-tool-unavailable class,
// either via the sentinel (errors.Is) or because the wrapped text carries a
// spawn-failure status.
func IsFFmpegUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrFFmpegUnavailable) {
		return true
	}
	return IsFFmpegSpawnFailureText(err.Error())
}

// IsFFmpegSlotStarved reports whether err means no ffmpeg ran because the
// shared pool had no free slot.  Callers use it to skip the fallbacks that
// exist for BROKEN SEEKS — the slow-seek retry and the blank-frame placeholder
// both need a slot too, so they cannot succeed here and only add wait time.
func IsFFmpegSlotStarved(err error) bool {
	return errors.Is(err, errFFmpegSlotStarved)
}

// ffmpegSlotWaitError builds the error runFFmpegFresh returns when the shared
// pool had no free slot within the wait budget.  It is a named constructor so
// the tests exercise the exact chain production builds — the wrapping is what
// makes the failure both skippable (IsFFmpegSlotStarved) and retry-later
// (IsFFmpegUnavailable), and a drift between the two would silently undo the
// mislabelling fix.
func ffmpegSlotWaitError(waited time.Duration) error {
	return fmt.Errorf("%w (waited %s): %w", errFFmpegSlotStarved, waited, ErrFFmpegUnavailable)
}

// thumbnailNodeToolUnavailable decides whether a generation run that produced no
// thumbnail failed because the node's tools could not run.
//
// Both conditions are required.  A spawn-failure signal alone is not enough: if
// the thumbnail came out, the tools obviously ran, so the signal was incidental
// (a transient failure on a later asset).  And an empty thumbnail alone is not
// enough either: that is the ordinary unthumbnailable-file verdict this must not
// steal.  Together they describe exactly the observed signature — nothing
// produced, and a process that would not start.
func thumbnailNodeToolUnavailable(thumbURL string, spawnFailures int32) bool {
	return thumbURL == "" && spawnFailures > 0
}

// ffmpegUnavailableRetryDelay replaces the normal 30s/60s/120s retry ramp when
// the failure is a node-tool outage.  The normal ramp retries three times inside
// ~3.5 minutes, which lands every attempt back inside the same outage that
// spans minutes (the node-18 burst above covers a whole pipeline's worth of
// files), so the retries are spent learning nothing.  Five minutes gives the
// window a chance to close first — and the file is kept on disk either way, so a
// longer wait risks nothing but latency.
const ffmpegUnavailableRetryDelay = 5 * time.Minute

// pipelineRetryDelay returns the delay before re-queuing a failed pipeline.
// The exponential ramp is deliberate for ordinary failures (a dead host, a
// transient disk error); node-tool outages get a flat, longer delay instead.
func pipelineRetryDelay(retries int, lastErr string) time.Duration {
	if IsFFmpegSpawnFailureText(lastErr) {
		return ffmpegUnavailableRetryDelay
	}
	if retries < 1 {
		retries = 1
	}
	delay := 30 * time.Second << uint(min(retries-1, 5))
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	return delay
}

// nodeToolFailureError builds the stage-level error for a node-tool outage, so
// callers can classify it with IsFFmpegUnavailable and operators see the node —
// not the file — named as the problem.  The failed-invocation count is logged at
// detection time by generateThumbnailForFile; it is not part of this error
// because the same error is rebuilt when routing a retry.
func nodeToolFailureError(filename string) error {
	return fmt.Errorf("%w during thumbnail generation for %s — the file is fine, keeping it and retrying after the node recovers",
		ErrFFmpegUnavailable, filename)
}
