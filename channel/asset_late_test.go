package channel

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// logRecorder separates the three severities so a test can assert not just that
// something was logged, but at WHICH LEVEL — which is the entire point of this
// change.  A mutex is needed because the trackers log from their own goroutine.
type logRecorder struct {
	mu   sync.Mutex
	info []string
	warn []string
	errs []string
}

func (r *logRecorder) infof(f string, a ...interface{}) { r.add(&r.info, f, a...) }
func (r *logRecorder) warnf(f string, a ...interface{}) { r.add(&r.warn, f, a...) }
func (r *logRecorder) errf(f string, a ...interface{})  { r.add(&r.errs, f, a...) }

func (r *logRecorder) add(dst *[]string, f string, a ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*dst = append(*dst, fmt.Sprintf(f, a...))
}

func (r *logRecorder) at(src []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), src...)
}

func (r *logRecorder) infos() []string { return r.at(r.info) }
func (r *logRecorder) warns() []string { return r.at(r.warn) }
func (r *logRecorder) errors() []string {
	return r.at(r.errs)
}

// waitFor polls until cond holds, so a test can observe a background tracker
// without depending on goroutine scheduling.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestCollectAssetsAbandonIsInformationalNotFailure is the regression test for
// the phantom 6,160: an asset that overruns the collect budget is still being
// uploaded, so abandoning it must NOT be logged as an error — that error line
// was the entire fleet's preview "failure" count while ~98% of those assets were
// delivered seconds later.
func TestCollectAssetsAbandonIsInformationalNotFailure(t *testing.T) {
	budget := 5 * time.Millisecond
	thumb, sprite, preview := make(chan string, 1), make(chan string, 1), make(chan string, 1)
	var thumbURL, spriteURL, previewURL string
	assets := []assetCollect{
		{"thumbnail", &thumbURL, thumb},
		{"sprite", &spriteURL, sprite},
		{"preview", &previewURL, preview},
	}

	rec := &logRecorder{}
	collectAssets("x.mp4", assets, budget, rec.infof, rec.warnf, rec.errf)

	if got := rec.errors(); len(got) != 0 {
		t.Fatalf("abandoning an asset must not log an error, got: %v", got)
	}
	infos := rec.infos()
	if len(infos) != 3 {
		t.Fatalf("each of the 3 assets should report its abandon at info level, got %d: %v", len(infos), infos)
	}
	for _, want := range []string{"still uploading", "not a failure"} {
		found := false
		for _, line := range infos {
			if strings.Contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("abandon line must say %q so operators do not read it as a failure: %v", want, infos)
		}
	}
	// An abandoned asset was never received, so its URL stays empty: the caller
	// must be able to tell "not back yet" from "uploaded".
	if thumbURL+spriteURL+previewURL != "" {
		t.Fatalf("abandoned assets must leave their URLs empty, got %q/%q/%q", thumbURL, spriteURL, previewURL)
	}
}

// TestCollectAssetsTracksLateLandingAfterAbandon covers the case the request is
// about: the collect gives up, the asset lands anyway, and the run records that
// it LANDED — a success — instead of leaving a failure behind.
func TestCollectAssetsTracksLateLandingAfterAbandon(t *testing.T) {
	preview := make(chan string, 1)
	var previewURL string
	assets := []assetCollect{{"preview", &previewURL, preview}}

	rec := &logRecorder{}
	collectAssets("x.mp4", assets, time.Millisecond, rec.infof, rec.warnf, rec.errf)

	// The upload finishes after the collect stopped waiting.
	preview <- "https://catbox.moe/preview.webp"

	waitFor(t, "the late landing to be recorded", func() bool {
		for _, line := range rec.infos() {
			if strings.Contains(line, "landed late") {
				return true
			}
		}
		return false
	})
	if got := rec.errors(); len(got) != 0 {
		t.Fatalf("an asset that landed late is not a failure, got errors: %v", got)
	}
	if got := rec.warns(); len(got) != 0 {
		t.Fatalf("an asset that landed late is not a warning either, got: %v", got)
	}
	for _, line := range rec.infos() {
		if strings.Contains(line, "landed late") && !strings.Contains(line, "not a failure") {
			t.Errorf("late-landing line should say it was not a failure: %q", line)
		}
	}
}

// TestTrackLateAssetEmptyResultIsWarnNotError pins the no-double-count rule. The
// asset goroutine logs its own error before it sends "" ("preview: failed for
// …", "all hosts rejected or saturated"), so an error here would report every
// genuine failure twice and inflate the bucket all over again.
func TestTrackLateAssetEmptyResultIsWarnNotError(t *testing.T) {
	done := make(chan string, 1)
	done <- ""

	rec := &logRecorder{}
	trackLateAsset("preview", "x.mp4", done, 3*time.Minute, rec.infof, rec.warnf, rec.errf)

	if got := rec.errors(); len(got) != 0 {
		t.Fatalf("the asset's own error line already reports this; a second one double-counts it: %v", got)
	}
	warns := rec.warns()
	if len(warns) != 1 {
		t.Fatalf("expected exactly one warn correlating the abandon with the failure, got: %v", warns)
	}
	if !strings.Contains(warns[0], "not counted again") {
		t.Errorf("the warn should point at the failure that was already reported: %q", warns[0])
	}
}

// TestTrackLateAssetStillMissingIsTheOnlyRealFailure is the flip side: if the
// goroutine never finishes at all, nothing else will ever report it (its own
// failure line is never written), so THIS is the one case that must be an error.
func TestTrackLateAssetStillMissingIsTheOnlyRealFailure(t *testing.T) {
	orig := lateAssetTrackBound
	lateAssetTrackBound = 10 * time.Millisecond
	t.Cleanup(func() { lateAssetTrackBound = orig })

	done := make(chan string, 1) // never receives
	rec := &logRecorder{}
	trackLateAsset("preview", "x.mp4", done, 3*time.Minute, rec.infof, rec.warnf, rec.errf)

	errs := rec.errors()
	if len(errs) != 1 {
		t.Fatalf("an asset that never finishes is a real failure and must be reported once, got: %v", rec.infos())
	}
	if !strings.Contains(errs[0], "treating as missing") {
		t.Errorf("expected the stuck case to be named, got: %q", errs[0])
	}
}

// TestLateAssetBoundOutlastsEveryAssetBudget guards the tracker's core
// assumption: it may only conclude "never landed" after the longest internal
// asset budget has expired. Announcing a missing asset while its goroutine is
// still legitimately working is the same category error as the abandon line
// this change removes.
func TestLateAssetBoundOutlastsEveryAssetBudget(t *testing.T) {
	if lateAssetTrackBound <= assetTimeoutCap {
		t.Errorf("lateAssetTrackBound (%v) must exceed the longest asset extraction budget (%v)",
			lateAssetTrackBound, assetTimeoutCap)
	}
}
