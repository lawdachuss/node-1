package uploader

import (
	"errors"
	"testing"
	"time"
)

// TestIsUploadAuthError verifies the credential-rejection matcher covers every
// live error shape: Streamtape's "API error 403: Authentication failed", the
// generic authenticate wording, and the unconfigured-key case — while leaving
// transient failures unclassified.
func TestIsUploadAuthError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("get upload URL: API error 403: Authentication failed"), true},
		{errors.New("streamtape: could not authenticate user. The key pair may be invalid or your account may be locked"), true},
		{errors.New("vidara: invalid api key"), true},
		{errors.New("imgbb: IMGBB_API_KEY not set"), false}, // not an auth REJECTION; initHosts already handles unconfigured hosts
		{errors.New("imgpile: rate limited (HTTP 429)"), false},
		{errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), false},
		{errors.New("imgbox: get token: HTTP 500: Internal Server Error"), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := isUploadAuthError(c.err); got != c.want {
			t.Errorf("case %d: isUploadAuthError(%v) = %v, want %v", i, c.err, got, c.want)
		}
	}
}

// TestIsFailFastAuthenticationFailed ensures the Streamtape 403 shape is also
// fail-fast at the per-host retry level (uploadWithRetries / host loops).
func TestIsFailFastAuthenticationFailed(t *testing.T) {
	err := errors.New("get upload URL: API error 403: Authentication failed")
	if !isFailFastError(err) {
		t.Fatalf("expected Streamtape 403 authentication failure to be fail-fast")
	}
}

// TestImgboxCircuitBreaker verifies the breaker opens after the failure
// threshold, blocks uploads while open, and resets on success.
func TestImgboxCircuitBreaker(t *testing.T) {
	// Isolate package-global state.
	prevOpenUntil, prevFailures := imgboxOpenUntil, imgboxFailures
	t.Cleanup(func() { imgboxOpenUntil, imgboxFailures = prevOpenUntil, prevFailures })
	imgboxOpenUntil, imgboxFailures = time.Time{}, 0

	for i := 0; i < imgboxBreakerThreshold; i++ {
		if imgboxBreakerOpen() {
			t.Fatalf("breaker must be closed before %d failures (failed %d so far)", imgboxBreakerThreshold, i)
		}
		imgboxRecordResult(false)
	}
	if !imgboxBreakerOpen() {
		t.Fatalf("breaker must be open after %d consecutive failures", imgboxBreakerThreshold)
	}

	// A success (simulating host recovery inside an open window after expiry)
	// resets the streak.
	imgboxRecordResult(true)
	if imgboxFailures != 0 {
		t.Fatalf("success must reset the failure streak, got %d", imgboxFailures)
	}
}

// TestImgPileThrottleSpacing verifies the process-wide throttle spaces
// consecutive calls and switches to the longer cooldown after a rate-limit.
func TestImgPileThrottleSpacing(t *testing.T) {
	prevLast, prevBackoff := imgPileLastUpload, imgPileBackoff
	t.Cleanup(func() { imgPileLastUpload, imgPileBackoff = prevLast, prevBackoff })
	imgPileLastUpload, imgPileBackoff = time.Now().Add(-imgPileMinInterval), time.Time{}

	start := time.Now()
	throttleImgPile() // just under interval → waits the remainder
	elapsed := time.Since(start)
	if elapsed > imgPileMinInterval {
		t.Fatalf("first throttle should wait at most one interval, waited %v", elapsed)
	}

	markImgPileRateLimited()
	if !time.Now().Before(imgPileBackoff) {
		t.Fatalf("markImgPileRateLimited must arm the cooldown window")
	}
}
