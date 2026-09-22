package uploader

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hijackClose drains the request body (so the client's progress reader reaches
// 100% of the file and flags the body as fully sent) and then closes the TCP
// connection without sending a response — the exact "body fully transmitted but
// the response was lost" failure that used to make VOE.sx re-stream the file.
func hijackClose(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("response writer does not support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	conn.Close()
}

// TestVoeSXUploader_LostResponseAfterFullBodyIsNotRetried is the idempotency
// regression test: when the whole body reached VOE.sx but the response was
// lost, the uploader must fail that attempt WITHOUT re-uploading the file.
// Before the bodyFullySent guard the inner retry loop re-streamed the same
// video up to maxAttempts times, creating duplicate uploads and double-counting
// the file against the account's allowance.
func TestVoeSXUploader_LostResponseAfterFullBodyIsNotRetried(t *testing.T) {
	defer func(old string) { voeSXAPIBase = old }(voeSXAPIBase)

	var uploadAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload/server"):
			resp := voeSXServerResponse{Status: 200, Success: true, Msg: "OK", Result: "http://" + r.Host + "/upload/01"}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case r.URL.Path == "/upload/01":
			uploadAttempts++
			hijackClose(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	voeSXAPIBase = srv.URL + "/api"

	u := NewVoeSXUploader("test-key")
	link, err := u.Upload(tempVideoFile(t))
	if err == nil {
		t.Fatalf("expected an error when the response is lost, got link %q", link)
	}
	if uploadAttempts != 1 {
		t.Fatalf("file body was re-streamed after a lost response: %d upload attempts, want 1", uploadAttempts)
	}
}

// TestVoeSXUploader_ExplicitRejectionAfterFullBodyStillRetries guards the flip
// side of the idempotency guard: when the server answered with an error status
// it clearly did NOT accept the file, so a retry is safe and must still happen.
// A blanket "body fully sent -> never retry" check would have broken this.
func TestVoeSXUploader_ExplicitRejectionAfterFullBodyStillRetries(t *testing.T) {
	defer func(old string) { voeSXAPIBase = old }(voeSXAPIBase)

	var uploadAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload/server"):
			resp := voeSXServerResponse{Status: 200, Success: true, Msg: "OK", Result: "http://" + r.Host + "/upload/01"}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case r.URL.Path == "/upload/01":
			uploadAttempts++
			// Consume the whole body first, so the bodyFullySent flag is set,
			// then reject with an explicit status.
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			if uploadAttempts == 1 {
				http.Error(w, "server hiccup", http.StatusInternalServerError)
				return
			}
			resp := voeSXUploadResponse{Success: true}
			resp.File.FileCode = "abc123xyz"
			b, _ := json.Marshal(resp)
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	voeSXAPIBase = srv.URL + "/api"

	u := NewVoeSXUploader("test-key")
	link, err := u.Upload(tempVideoFile(t))
	if err != nil {
		t.Fatalf("expected the retry to recover, got %v", err)
	}
	if !strings.HasSuffix(link, "/abc123xyz") {
		t.Fatalf("unexpected link %q", link)
	}
	if uploadAttempts != 2 {
		t.Fatalf("an explicit 500 rejection must be retried exactly once more, got %d attempt(s)", uploadAttempts)
	}
}
