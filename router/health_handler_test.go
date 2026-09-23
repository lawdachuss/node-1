package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

// callHealth runs HealthAPI against a throwaway gin engine.
func callHealth(t *testing.T) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/health", nil)
	HealthAPI(c)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("health response is not JSON (%v): %s", err, w.Body.String())
	}
	return w.Code, body
}

// TestHealthAPIReflectsUploadLinkWritePath is the regression guard for the reason
// this endpoint exists: on 2026-09-23 the database reachability probe stayed green
// while every upload_links insert was rejected, so an external monitor watching
// only "is Supabase up?" saw a healthy fleet losing a day of link data.
func TestHealthAPIReflectsUploadLinkWritePath(t *testing.T) {
	prevConfig := server.Config
	t.Cleanup(func() {
		server.Config = prevConfig
		server.ResetDBClient()
	})

	// A database that answers reads but rejects upload_links writes — exactly the
	// shape of the incident.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/upload_links") && r.Method == "POST" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"42883","message":"operator does not exist: uuid = text"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	server.Config = &entity.Config{SupabaseURL: srv.URL, SupabaseAPIKey: "test-key"}
	server.ResetDBClient()

	code, body := callHealth(t)
	linkPath, _ := body["link_write_path"].(map[string]any)
	if linkPath == nil {
		t.Fatalf("health response has no link_write_path object: %v", body)
	}
	if linkPath["state"] != "unchecked" {
		t.Errorf("link_write_path.state = %v before any canary run, want \"unchecked\" — an unchecked path must never be reported as healthy", linkPath["state"])
	}
	if code != http.StatusServiceUnavailable {
		t.Errorf("HTTP %d before the first canary run, want 503", code)
	}

	// Now run the canary against that database: it must fail and say so.
	if err := server.CheckUploadLinkWritePath(); err == nil {
		t.Fatal("canary passed against a database that rejects upload_links inserts")
	}
	code, body = callHealth(t)
	linkPath, _ = body["link_write_path"].(map[string]any)
	if linkPath["state"] != "failing" {
		t.Errorf("link_write_path.state = %v after a failed canary, want \"failing\"", linkPath["state"])
	}
	if detail, _ := linkPath["detail"].(string); !strings.Contains(detail, "42883") {
		t.Errorf("link_write_path.detail = %q, want the database's own error", detail)
	}
	if linkPath["checked_at"] == "" || linkPath["checked_at"] == nil {
		t.Error("link_write_path.checked_at is empty; a stale result must be visible")
	}
	if body["ok"] != false || code != http.StatusServiceUnavailable {
		t.Errorf("ok = %v with HTTP %d while links cannot be written, want false/503", body["ok"], code)
	}
	if supabase, _ := body["supabase"].(string); supabase != "ok" {
		t.Errorf("supabase = %q — the read path is reachable, which is exactly why the write-path signal had to be separate", supabase)
	}
}

// TestHealthAPIReportsHealthyDatabase pins the success path so the endpoint is not
// stuck at 503 for everything.
func TestHealthAPIReportsHealthyDatabase(t *testing.T) {
	prevConfig := server.Config
	t.Cleanup(func() {
		server.Config = prevConfig
		server.ResetDBClient()
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	server.Config = &entity.Config{SupabaseURL: srv.URL, SupabaseAPIKey: "test-key"}
	server.ResetDBClient()

	if err := server.CheckUploadLinkWritePath(); err != nil {
		t.Fatalf("canary against a healthy database: %v", err)
	}

	code, body := callHealth(t)
	if code != http.StatusOK || body["ok"] != true {
		t.Errorf("HTTP %d ok=%v with a reachable database and an open write path, want 200/true (%v)", code, body["ok"], body)
	}
	linkPath, _ := body["link_write_path"].(map[string]any)
	if linkPath["state"] != "ok" {
		t.Errorf("link_write_path.state = %v, want \"ok\"", linkPath["state"])
	}
}
