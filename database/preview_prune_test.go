package database

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// previewFake serves a fixed, id-sorted preview_images table and records the
// queries it received, so a test can assert how the prune scans and deletes.
type previewFake struct {
	rows    []map[string]string
	gets    []string
	deletes []string
}

func newPreviewFake(rows int) *previewFake {
	f := &previewFake{}
	for i := 0; i < rows; i++ {
		f.rows = append(f.rows, map[string]string{
			"id":       fmt.Sprintf("id-%04d", i),
			"filename": fmt.Sprintf("file-%04d.mp4", i),
		})
	}
	return f
}

func (f *previewFake) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/preview_images"):
		f.gets = append(f.gets, r.URL.RawQuery)
		q := r.URL.Query()
		limit := 500
		if v := q.Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		cursor := strings.TrimPrefix(q.Get("id"), "gt.")
		out := make([]map[string]string, 0, limit)
		for _, row := range f.rows {
			if cursor != "" && row["id"] <= cursor {
				continue
			}
			out = append(out, row)
			if len(out) == limit {
				break
			}
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/preview_images"):
		f.deletes = append(f.deletes, r.URL.Query().Get("id"))
		_, _ = w.Write([]byte(`[]`))

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
	}
}

func (f *previewFake) deletedIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	for _, filter := range f.deletes {
		if !strings.HasPrefix(filter, "in.(") || !strings.HasSuffix(filter, ")") {
			t.Fatalf("delete filter is not an in.(...) list: %q", filter)
		}
		inner := filter[len("in.(") : len(filter)-1]
		if inner == "" {
			continue
		}
		batch := strings.Split(inner, ",")
		if len(batch) > 50 {
			t.Errorf("delete batch carries %d ids; the URL budget is 50", len(batch))
		}
		ids = append(ids, batch...)
	}
	return ids
}

func newPreviewClient(t *testing.T, f *previewFake) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, APIKey: "test-key", client: srv.Client()}
}

// TestPreviewPruneSkipsKeptRows pins the whole contract: the scan pages past
// 500-row pages with a cursor, kept filenames are never deleted, every other row
// is, deletes stay inside the in.(...) URL budget, and a dry run issues no
// DELETE while reporting the same number the real run removes.
func TestPreviewPruneSkipsKeptRows(t *testing.T) {
	fake := newPreviewFake(501) // two pages: 500 + 1
	client := newPreviewClient(t, fake)
	cutoff := time.Now().UTC().AddDate(0, 0, -7)
	keep := map[string]bool{"file-0000.mp4": true}

	count, err := client.CountOrphanedPreviewImages(cutoff, keep)
	if err != nil {
		t.Fatalf("CountOrphanedPreviewImages: %v", err)
	}
	if count != 500 {
		t.Fatalf("dry run counted %d rows, want 500 (501 minus the kept one)", count)
	}
	if len(fake.deletes) != 0 {
		t.Fatalf("dry run issued %d DELETE(s); it must not modify anything", len(fake.deletes))
	}
	if len(fake.gets) < 2 {
		t.Fatalf("expected the scan to page past 500 rows, got %d GET(s)", len(fake.gets))
	}
	for _, q := range fake.gets {
		if !strings.Contains(q, "uploaded_at=lt.") {
			t.Errorf("scan query is missing the age cutoff: %s", q)
		}
	}
	if !strings.Contains(fake.gets[1], "id=gt.") {
		t.Errorf("second page is not cursor-based (%s); an offset scan would revisit kept rows forever", fake.gets[1])
	}

	fake.gets, fake.deletes = nil, nil

	deleted, err := client.DeleteOrphanedPreviewImages(cutoff, keep)
	if err != nil {
		t.Fatalf("DeleteOrphanedPreviewImages: %v", err)
	}
	if deleted != count {
		t.Errorf("deleted %d rows, dry run said %d", deleted, count)
	}

	ids := fake.deletedIDs(t)
	if len(ids) != 500 {
		t.Fatalf("deleted %d ids, want 500", len(ids))
	}
	for _, id := range ids {
		if id == "id-0000" {
			t.Error("deleted the preview row of a kept recording (id-0000)")
		}
	}
}

// TestPreviewPruneTerminatesWhenEverythingIsKept is the regression guard for the
// scan shape: rows that stay in the table must not be re-scanned forever, so a
// table where nothing is deletable still finishes after one page and one
// end-of-pages check.
func TestPreviewPruneTerminatesWhenEverythingIsKept(t *testing.T) {
	fake := newPreviewFake(600)
	client := newPreviewClient(t, fake)

	keep := map[string]bool{}
	for _, row := range fake.rows {
		keep[row["filename"]] = true
	}

	done := make(chan int, 1)
	go func() {
		n, err := client.DeleteOrphanedPreviewImages(time.Now().UTC(), keep)
		if err != nil {
			t.Errorf("DeleteOrphanedPreviewImages: %v", err)
		}
		done <- n
	}()

	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("deleted %d rows, want 0 (all filenames are kept)", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("prune did not terminate with every row kept — the scan is revisiting rows it kept")
	}
	if len(fake.deletes) != 0 {
		t.Errorf("issued %d DELETE(s) with nothing to delete", len(fake.deletes))
	}
}

// TestPreviewPruneEncodesCutoff ensures the age boundary is sent as an RFC3339
// timestamp filter that PostgREST can compare against uploaded_at.
func TestPreviewPruneEncodesCutoff(t *testing.T) {
	fake := newPreviewFake(0)
	client := newPreviewClient(t, fake)
	cutoff := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	if _, err := client.CountOrphanedPreviewImages(cutoff, nil); err != nil {
		t.Fatalf("CountOrphanedPreviewImages: %v", err)
	}
	if len(fake.gets) == 0 {
		t.Fatal("no scan query issued")
	}
	want := "uploaded_at=lt." + url.QueryEscape("2026-09-16T12:00:00Z")
	if !strings.Contains(fake.gets[0], want) {
		t.Errorf("scan query %s does not carry the cutoff filter %s", fake.gets[0], want)
	}
}
