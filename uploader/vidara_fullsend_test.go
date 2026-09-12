package uploader

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullSendFakeHost returns a host upload func that reports the file at 100%
// through the progress callback and then fails, counting how many times the
// func is actually invoked.
func fullSendFakeHost(count *int, failWith error) uploaderFunc {
	return func(_ string, progress ProgressFunc) (string, error) {
		*count++
		if progress != nil {
			progress("TestHost", 1024, 1024)
		}
		return "", failWith
	}
}

// TestFullSendSkipsReUploadOnSubsequentAttempt: a host that fully transmitted
// the body but lost the response must NOT have the file re-streamed on the next
// UploadSelectedWithCallback call (stageUploadVideos re-runs it once per
// DoWithRetry attempt, reusing the same MultiHostUploader).
func TestFullSendSkipsReUploadOnSubsequentAttempt(t *testing.T) {
	var count int
	boom := errors.New("upload failed: unexpected response from host")
	u := &MultiHostUploader{
		log: &nilLogger{},
		hosts: map[string]uploaderFunc{
			"TestHost": fullSendFakeHost(&count, boom),
		},
	}

	first := u.UploadSelected("f.mp4", []string{"TestHost"})
	if len(first) != 1 || first[0].Error == nil {
		t.Fatalf("first attempt should fail with a result")
	}
	if count != 1 {
		t.Fatalf("first attempt invoked host %d times, want 1", count)
	}
	if !u.hasFullSend("TestHost") {
		t.Fatal("full-send marker not recorded after progress reached 100%")
	}

	// Simulate the next DoWithRetry attempt for the same file.
	second := u.UploadSelected("f.mp4", []string{"TestHost"})
	if len(second) != 1 || second[0].Error == nil {
		t.Fatalf("skipped attempt must still yield a failure result")
	}
	if count != 1 {
		t.Fatalf("re-upload must be skipped after full send; host invoked %d times, want 1", count)
	}
	if !strings.Contains(second[0].Error.Error(), "already fully transmitted") {
		t.Fatalf("skipped attempt error = %v, want errBodyFullySent wording", second[0].Error)
	}
}

// TestFullSendClearedOnSuccess: once the host succeeds, the full-send marker is
// cleared so a later (hypothetical) attempt is not skipped.
func TestFullSendClearedOnSuccess(t *testing.T) {
	var count int
	u := &MultiHostUploader{
		log: &nilLogger{},
		hosts: map[string]uploaderFunc{
			"TestHost": func(_ string, progress ProgressFunc) (string, error) {
				count++
				if progress != nil {
					progress("TestHost", 1024, 1024)
				}
				return "https://example.test/video", nil
			},
		},
	}

	results := u.UploadSelected("f.mp4", []string{"TestHost"})
	if len(results) != 1 || results[0].Error != nil {
		t.Fatalf("expected success, got %#v", results)
	}
	if u.hasFullSend("TestHost") {
		t.Fatal("full-send marker must be cleared on success")
	}
}

// TestVidaraStopsRetryingAfterFullBodySent: within a single
// UploadWithProgress call, when the entire body has been transmitted and the
// host then returns a 5xx, the file must NOT be re-streamed for the remaining
// in-call retry attempts.
func TestVidaraStopsRetryingAfterFullBodySent(t *testing.T) {
	var uploads int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/upload/server") {
			fmt.Fprintf(w, `{"msg":"OK","status":200,"result":{"upload_server":%q}}`, srv.URL)
			return
		}
		uploads++
		// Consume the ENTIRE multipart body (this is what makes the client's
		// progress reach 100%), then reject the upload with a 500.
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "read err", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"status":500,"msg":"boom"}`)
	}))
	defer srv.Close()

	oldBase := vidaraAPIBase
	vidaraAPIBase = srv.URL
	defer func() { vidaraAPIBase = oldBase }()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.mp4")
	if err := os.WriteFile(filePath, []byte("fake video bytes"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	u := NewVidaraUploader("test-key")
	if _, err := u.Upload(filePath); err == nil {
		t.Fatal("expected an error from a 500 response")
	}
	if uploads != 1 {
		t.Fatalf("file re-streamed %d times after a fully-sent body, want 1", uploads)
	}
}

// TestVidaraAuthErrorNarrowing pins isVidaraAuthError to explicit
// credential-rejection wording.  Broad matchers ("403", "forbidden", bare
// "api key") previously mislabeled nginx/Cloudflare block pages as dead keys,
// producing "all keys exhausted" for every file while the key was valid.
func TestVidaraAuthErrorNarrowing(t *testing.T) {
	rejections := []string{
		`get upload server failed with status 401: {"msg":"invalid api_key","status":401,"result":null}`,
		"get upload server failed with status 403: invalid api key",
		"auth failed: invalid_api_key",
		"upload failed: unauthorized",
		"could not authenticate user: authentication failed",
		"api key is invalid",
	}
	for _, m := range rejections {
		if !isVidaraAuthError(errors.New(m)) {
			t.Errorf("isVidaraAuthError(%q) = false, want true", m)
		}
	}

	notAuth := []string{
		"get upload server failed with status 403: 403 Forbidden", // nginx/Cloudflare block page
		"upload failed with status 403: <html>Forbidden</html>",
		"get upload server failed with status 403",
		"upload failed with status 500: internal server error",
		"dial tcp 1.2.3.4:443: connectex: connection refused",
		"Vidara API key not configured",
	}
	for _, m := range notAuth {
		if isVidaraAuthError(errors.New(m)) {
			t.Errorf("isVidaraAuthError(%q) = true, want false", m)
		}
	}
}