package recovery

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOneTicketFileNotFoundIsPermanent(t *testing.T) {
	var calls int32
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if strings.Contains(r.URL.Path, "dlticket") {
			w.Write([]byte(`{"status":200,"msg":"OK","result":{"ticket":"abc123~u~~1~tok","wait_time":0,"valid_until":"2026-09-08"}}`))
			return
		}
		// dl endpoint -> permanent 404 "file not found"
		w.Write([]byte(`{"status":404,"msg":"file not found","result":null}`))
	}))
	defer dlSrv.Close()
	defer func(old string) { streamAPI = old }(streamAPI)
	streamAPI = dlSrv.URL

	_, err := oneTicket("somefilecode", "login", "key")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, errStreamFileNotFound) {
		t.Fatalf("expected errStreamFileNotFound, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 1 {
		t.Fatalf("expected dlticket/dl server to be hit, got %d calls", got)
	}
}

func TestFreshDLURLBailsImmediatelyOnFileNotFound(t *testing.T) {
	var dlCalls int32
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dlCalls, 1)
		if strings.Contains(r.URL.Path, "dlticket") {
			w.Write([]byte(`{"status":200,"msg":"OK","result":{"ticket":"abc123~u~~1~tok","wait_time":0,"valid_until":"2026-09-08"}}`))
			return
		}
		w.Write([]byte(`{"status":404,"msg":"file not found","result":null}`))
	}))
	defer dlSrv.Close()
	defer func(old string) { streamAPI = old }(streamAPI)
	streamAPI = dlSrv.URL

	start := time.Now()
	_, err := freshDLURL("somefilecode", "login", "key")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, errStreamFileNotFound) {
		t.Fatalf("expected errStreamFileNotFound, got %v", err)
	}
	// freshDLURL used to loop 4x with a 3s sleep; the permanent-404 path must
	// not retry, so the whole call should return fast.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected fail-fast, took %v", elapsed)
	}
	// dlticket called once (1), dl once (1) => 2 hits. No retry loop.
	if got := atomic.LoadInt32(&dlCalls); got != 2 {
		t.Fatalf("expected 2 server hits (no retries), got %d", got)
	}
}
