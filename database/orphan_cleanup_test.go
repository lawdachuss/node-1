package database

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestDeleteOrphanedRecordingsSkipsOnlineNodes verifies that cleanup only
// deletes candidates whose owning instance is NOT currently online: a row
// created by a node that is heartbeating may be mid-pipeline (thumbnail queue
// backlog > 30 min), and deleting it makes every subsequent per-host link
// save fail with a 23503 FK violation.  Rows from dead/unknown instances are
// still cleaned.
func TestDeleteOrphanedRecordingsSkipsOnlineNodes(t *testing.T) {
	var deletedIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == "DELETE" && strings.HasSuffix(path, "/recordings"):
			deletedIDs = parseIDInFilter(r.URL)
			w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/nodes"):
			json.NewEncoder(w).Encode([]Node{
				{NodeID: "node-1", Status: "online"},
				{NodeID: "node-9", Status: "draining"},
				{NodeID: "node-gone", Status: "offline"},
			})
		case r.Method == "GET" && strings.HasSuffix(path, "/recordings"):
			// Candidate scan: three rows older than the cutoff, none with
			// thumbnail/embed.  node-1/node-9 are alive; node-gone is not.
			q := r.URL.Query()
			if q.Get("thumbnail_url") == "is.null" {
				json.NewEncoder(w).Encode([]Recording{
					{ID: "rec-alive-1", InstanceID: "node-1", CreatedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)},
					{ID: "rec-alive-9", InstanceID: "node-9", CreatedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)},
					{ID: "rec-dead", InstanceID: "node-gone", CreatedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)},
				})
				return
			}
			w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/upload_links"):
			// No links for any candidate → all pass the zero-links check.
			w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+path, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, APIKey: "test-key", client: srv.Client()}
	deleted, err := c.DeleteOrphanedRecordings(30 * time.Minute)
	if err != nil {
		t.Fatalf("DeleteOrphanedRecordings: %v", err)
	}

	if deleted != 1 {
		t.Fatalf("expected exactly 1 row deleted (the dead-node one), got %d (%v)", deleted, deletedIDs)
	}
	if len(deletedIDs) != 1 || deletedIDs[0] != "rec-dead" {
		t.Fatalf("expected only rec-dead to be deleted, got %v", deletedIDs)
	}
}

// parseIDInFilter extracts ids from an ?id=in.(...) DELETE filter.
func parseIDInFilter(u *url.URL) []string {
	inner := u.Query().Get("id")
	if !strings.HasPrefix(inner, "in.(") || !strings.HasSuffix(inner, ")") {
		return nil
	}
	inner = inner[len("in.(") : len(inner)-1]
	if inner == "" {
		return nil
	}
	var out []string
	for _, id := range strings.Split(inner, ",") {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}
