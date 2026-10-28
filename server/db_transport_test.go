package server

import (
	"net/http"
	"testing"
	"time"
)

func TestSupabaseRESTClientsSharePooledTransport(t *testing.T) {
	if supabaseHTTPClient.Transport != supabaseRESTTransport {
		t.Fatal("supabaseHTTPClient does not use the shared transport")
	}
	if fastHTTPClient.Transport != supabaseRESTTransport {
		t.Fatal("fastHTTPClient does not share the Supabase transport")
	}

	transport, ok := supabaseRESTTransport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", supabaseRESTTransport)
	}
	if transport.MaxIdleConnsPerHost < 16 {
		t.Errorf("MaxIdleConnsPerHost = %d, want at least 16", transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", transport.IdleConnTimeout)
	}
}
