package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestIsFFmpegSpawnFailureTextOnProductionMessages pins the classifier against
// the exact messages the fleet actually emitted.  The two entries that must stay
// FALSE matter most: 0xffffffea is a genuine media verdict (ffmpeg's documented
// EINVAL for header-only fMP4) and a host-chain failure is not a node fault —
// classifying either as "node broken" would retry a file forever instead of
// finalizing it.
func TestIsFFmpegSpawnFailureTextOnProductionMessages(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "thumb fast seek, node-18 burst",
			msg:  "thumb: fast seek failed for Mr_Genghis_Khan_2026-09-22_00-50-16.mp4: exit status 0xc000026b, retrying with slow seek",
			want: true,
		},
		{
			name: "sprite tile, same burst",
			msg:  "sprite: tile 2 fast seek failed for Mr_Genghis_Khan_2026-09-22_00-50-16.mp4: exit status 0xc000026b — retrying with slow seek",
			want: true,
		},
		{
			name: "preview failed, same burst",
			msg:  "preview: failed for Mr_Genghis_Khan_2026-09-22_00-50-16.mp4: exit status 0xc000026b",
			want: true,
		},
		{
			name: "duration probe failed with the co-occurring status",
			msg:  "thumb: duration probe failed for x.mp4: exit status 0xbebbb1b7 — continuing with unknown duration",
			want: true,
		},
		{
			name: "DLL not found",
			msg:  "thumb: failed for x.mp4: exit status 0xc0000135",
			want: true,
		},
		{
			name: "binary missing",
			msg:  "thumb: failed for x.mp4: exec: \"ffmpeg\": executable file not found in %PATH%",
			want: true,
		},
		{
			name: "media verdict: EINVAL on header-only fMP4 must NOT be a node fault",
			msg:  "thumb: fast seek failed for x.mp4: exit status 0xffffffea, retrying with slow seek",
			want: false,
		},
		{
			name: "unreadable keyframe is a media problem",
			msg:  "sprite: tile 9 at 6076s skipped (both seeks failed): exit status 1",
			want: false,
		},
		{
			name: "host chain failure is not a node-tool failure",
			msg:  "thumb: upload failed for x.mp4 — all hosts rejected or saturated",
			want: false,
		},
		{
			name: "ordinary stage failure",
			msg:  "thumbnail generation/upload failed for x.mp4 — will retry later",
			want: false,
		},
		{name: "empty", msg: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsFFmpegSpawnFailureText(tc.msg); got != tc.want {
				t.Errorf("IsFFmpegSpawnFailureText(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestIsFFmpegUnavailable covers both routes into the class: the sentinel (so
// stage code can classify without re-parsing text) and the raw status text.
func TestIsFFmpegUnavailable(t *testing.T) {
	if IsFFmpegUnavailable(nil) {
		t.Fatal("nil error must not be a node-tool failure")
	}
	wrapped := fmt.Errorf("pipeline: %w", nodeToolFailureError("x.mp4"))
	if !IsFFmpegUnavailable(wrapped) {
		t.Fatal("a wrapped sentinel must classify as unavailable")
	}
	if IsFFmpegUnavailable(errors.New("exit status 0xffffffea")) {
		t.Fatal("a media verdict must not classify as unavailable")
	}
}

// TestThumbnailNodeToolUnavailable pins that BOTH conditions are required, which
// is what keeps this from stealing the ordinary unthumbnailable verdict.
func TestThumbnailNodeToolUnavailable(t *testing.T) {
	cases := []struct {
		name          string
		thumbURL      string
		spawnFailures int32
		want          bool
	}{
		{name: "nothing produced + tools would not start (the observed outage)", thumbURL: "", spawnFailures: 49, want: true},
		{name: "nothing produced but no spawn failure (unthumbnailable file)", thumbURL: "", spawnFailures: 0, want: false},
		{name: "thumbnail produced, so the tools clearly ran", thumbURL: "https://host/x.jpg", spawnFailures: 3, want: false},
		{name: "clean run", thumbURL: "https://host/x.jpg", spawnFailures: 0, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := thumbnailNodeToolUnavailable(tc.thumbURL, tc.spawnFailures); got != tc.want {
				t.Errorf("thumbnailNodeToolUnavailable(%q, %d) = %v, want %v", tc.thumbURL, tc.spawnFailures, got, tc.want)
			}
		})
	}
}

// TestPipelineRetryDelay is the behavioural half of the fix: three retries on the
// normal 30s/60s/120s ramp all land inside a multi-minute node outage, so a
// node-tool failure must back off far enough for the window to close.  Ordinary
// failures keep the exponential ramp.
func TestPipelineRetryDelay(t *testing.T) {
	// Built through the real constructor (not a copied string) so the retry path
	// cannot drift away from the sentinel wording it classifies on.
	nodeFault := nodeToolFailureError("x.mp4").Error()
	if !IsFFmpegSpawnFailureText(nodeFault) {
		t.Fatalf("the stage error must classify by text, since only its text survives a retry: %q", nodeFault)
	}
	for _, retries := range []int{1, 2, 3} {
		if got := pipelineRetryDelay(retries, nodeFault); got != ffmpegUnavailableRetryDelay {
			t.Errorf("node-tool failure at retry %d waited %v, want the flat %v", retries, got, ffmpegUnavailableRetryDelay)
		}
	}
	if ffmpegUnavailableRetryDelay <= 2*time.Minute {
		t.Errorf("ffmpegUnavailableRetryDelay = %v; it must outlast the 30s/60s/120s ramp it replaces", ffmpegUnavailableRetryDelay)
	}

	normal := []struct {
		retries int
		want    time.Duration
	}{
		{retries: 1, want: 30 * time.Second},
		{retries: 2, want: 60 * time.Second},
		{retries: 3, want: 120 * time.Second},
		{retries: 4, want: 240 * time.Second},
		{retries: 0, want: 30 * time.Second}, // clamped, never a zero/negative shift
		{retries: 99, want: 10 * time.Minute},
	}
	for _, tc := range normal {
		if got := pipelineRetryDelay(tc.retries, "some other failure"); got != tc.want {
			t.Errorf("pipelineRetryDelay(%d, ordinary) = %v, want %v", tc.retries, got, tc.want)
		}
	}
}

// TestFFmpegSlotWaitErrorIsBothSkippableAndRetryable pins the acquire-timeout
// wrapper.  It has to satisfy two contracts at once: IsFFmpegSlotStarved, so the
// seek-fallback paths stop retrying into the same saturated pool, and
// IsFFmpegUnavailable, so the stage treats it as "retry after the node recovers"
// rather than finalizing without a thumbnail.
func TestFFmpegSlotWaitErrorIsBothSkippableAndRetryable(t *testing.T) {
	err := ffmpegSlotWaitError(30 * time.Second)
	if !IsFFmpegSlotStarved(err) {
		t.Fatalf("must be recognisable as pool starvation: %v", err)
	}
	if !IsFFmpegUnavailable(err) {
		t.Fatalf("must also classify as a node-tool fault so the retry is delayed: %v", err)
	}
	if errors.Is(err, ErrFFmpegUnavailable) == false {
		t.Fatalf("errors.Is must reach the umbrella sentinel: %v", err)
	}
	// The message names the wait and the pool — never a seek, which is the whole
	// point of the fix (905 of 1,330 "fast seek failed" lines were this).
	msg := err.Error()
	if !strings.Contains(msg, "ffmpeg pool saturated") || !strings.Contains(msg, "30s") {
		t.Errorf("message should name the saturated pool and the wait: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "seek") {
		t.Errorf("a pool wait must not be described as a seek failure: %q", msg)
	}

	// Not confused with the other class, and nil-safe.
	if IsFFmpegSlotStarved(nil) {
		t.Fatal("nil must not be pool starvation")
	}
	if IsFFmpegSlotStarved(errors.New("exit status 0xc000026b")) {
		t.Fatal("a spawn failure is not pool starvation")
	}
}

// TestRunFFmpegFreshLabelsPoolStarvation guards the production wiring, not just
// the error constructor: the pool wait returning a bare ctx.Err() ("context
// deadline exceeded") must be converted on the way out, because that is exactly
// the value all three callers used to log as "fast seek failed … retrying with
// slow seek".  Stubbing the acquire seam means ffmpeg is never invoked, so a
// pass also proves the function bails out BEFORE spawning a process it has no
// slot for.
func TestRunFFmpegFreshLabelsPoolStarvation(t *testing.T) {
	orig := acquireFFmpegSlot
	t.Cleanup(func() { acquireFFmpegSlot = orig })
	// What the real AcquireFFmpegFor returns when its budget expires.
	acquireFFmpegSlot = func(time.Duration) error { return context.DeadlineExceeded }

	err := runFFmpegFresh(time.Second, "-version")
	if err == nil {
		t.Fatal("a starved pool must be an error, not a silent no-op")
	}
	if !IsFFmpegSlotStarved(err) {
		t.Fatalf("callers gate their fallbacks on IsFFmpegSlotStarved, so it must hold: %v", err)
	}
	if !IsFFmpegUnavailable(err) {
		t.Fatalf("it must stay in the node-tool class so the pipeline retries after recovery: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "ffmpeg pool saturated") || strings.Contains(strings.ToLower(msg), "seek") {
		t.Errorf("the message must name the pool and never a seek: %q", msg)
	}
}

// TestNodeToolFailureErrorNamesTheNodeAndClassifies checks the error is both
// machine-classifiable and readable — this string is what an operator sees.
func TestNodeToolFailureErrorNamesTheNodeAndClassifies(t *testing.T) {
	err := nodeToolFailureError("Mr_Genghis_Khan_2026-09-22_00-50-16.mp4")
	if !errors.Is(err, ErrFFmpegUnavailable) {
		t.Fatalf("error must wrap ErrFFmpegUnavailable so stages can classify it: %v", err)
	}
	if !IsFFmpegUnavailable(err) {
		t.Fatalf("IsFFmpegUnavailable must accept it: %v", err)
	}
	if !IsFFmpegSpawnFailureText(err.Error()) {
		t.Fatalf("the message must itself classify, since only its text survives a retry: %v", err)
	}
}
