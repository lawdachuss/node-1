package database

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// inFilterValues extracts the values of a PostgREST `?key=in.(a,b,c)` filter,
// returning nil when the filter is absent or malformed.  Handlers call it from
// their own goroutine, so it must never fail the test itself.
func inFilterValues(u *url.URL, key string) []string {
	inner := u.Query().Get(key)
	if !strings.HasPrefix(inner, "in.(") || !strings.HasSuffix(inner, ")") {
		return nil
	}
	inner = inner[len("in.(") : len(inner)-1]
	if inner == "" {
		return nil
	}
	return strings.Split(inner, ",")
}

// TestLookupRecordingLinkStateIsForeignKeyFree pins how "does this recording have
// a host?" is asked.  The deployed schema has no foreign key from upload_links to
// recordings, so PostgREST rejects an embedded `upload_links(host)` select with
// PGRST200 — an embed-based check answers "no links" (with an error) for every
// file, which is how host-less recordings counted as uploaded.  The lookup must
// resolve filenames to recording ids first, then ask upload_links for exactly
// those ids, in batches that stay inside the proxy's URL budget.
func TestLookupRecordingLinkStateIsForeignKeyFree(t *testing.T) {
	var (
		recQueries     []*url.URL
		linkQueries    []*url.URL
		embeddedSelect bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/recordings"):
			recQueries = append(recQueries, r.URL)
			if strings.Contains(r.URL.Query().Get("select"), "upload_links") {
				embeddedSelect = true
			}
			var rows []map[string]string
			for _, name := range parseFilenameInFilter(r.URL) {
				if name == "gone.mp4" { // no recordings row at all
					continue
				}
				rows = append(rows, map[string]string{"id": "rec-" + name, "filename": name})
			}
			_ = json.NewEncoder(w).Encode(rows)

		case strings.HasSuffix(r.URL.Path, "/upload_links"):
			linkQueries = append(linkQueries, r.URL)
			var rows []map[string]string
			for _, id := range inFilterValues(r.URL, "recording_id") {
				if id == "rec-linked.mp4" { // only this recording ever reached a host
					rows = append(rows, map[string]string{"recording_id": id})
				}
			}
			_ = json.NewEncoder(w).Encode(rows)

		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	names := []string{"linked.mp4", "no-links.mp4", "gone.mp4"}
	for i := 0; i < 42; i++ {
		names = append(names, fmt.Sprintf("filler%02d.mp4", i))
	}

	c := &Client{URL: srv.URL, APIKey: "test-key", client: srv.Client()}
	ids, hasLinks, err := c.LookupRecordingLinkState(names)
	if err != nil {
		t.Fatalf("LookupRecordingLinkState: %v", err)
	}

	if embeddedSelect {
		t.Error("resolved link state with an embedded upload_links select, which the deployed schema rejects (PGRST200)")
	}
	if len(recQueries) != 2 {
		t.Errorf("expected 45 filenames to be resolved in 2 batches (batch size %d), got %d: %v",
			releaseBatchSize, len(recQueries), recQueries)
	}
	if len(linkQueries) != 2 {
		t.Errorf("expected 45 ids to be looked up in 2 batches, got %d", len(linkQueries))
	}
	for i, u := range recQueries {
		if got := len(parseFilenameInFilter(u)); got > releaseBatchSize {
			t.Errorf("recordings batch %d carries %d filenames, above the %d-per-request URL budget", i, got, releaseBatchSize)
		}
	}
	for i, u := range linkQueries {
		ids := inFilterValues(u, "recording_id")
		if len(ids) == 0 {
			t.Errorf("upload_links batch %d does not filter recording_id=in.(...): %s", i, u.RawQuery)
			continue
		}
		if len(ids) > releaseBatchSize {
			t.Errorf("upload_links batch %d carries %d ids, above the %d-per-request URL budget", i, len(ids), releaseBatchSize)
		}
	}

	if len(ids) != 44 {
		t.Errorf("resolved %d recording ids, want 44 (45 names minus the one with no row)", len(ids))
	}
	if ids["no-links.mp4"] != "rec-no-links.mp4" {
		t.Errorf("ids[no-links.mp4] = %q, want rec-no-links.mp4", ids["no-links.mp4"])
	}
	if _, ok := ids["gone.mp4"]; ok {
		t.Error("reported a recording id for a filename that has no recordings row")
	}

	if !hasLinks["linked.mp4"] {
		t.Error("linked.mp4 has an upload_links row, want it marked as linked")
	}
	if len(hasLinks) != 1 {
		t.Errorf("marked %d recordings as linked, want exactly 1 (%v)", len(hasLinks), hasLinks)
	}
	if hasLinks["no-links.mp4"] {
		t.Error("marked a recording with zero upload_links rows as linked — that is the exact state this lookup exists to detect")
	}
}

// TestHasUploadedLinksUsesLinkTableNotRowExistence guards the disk-cleanup guard:
// a recordings row is written at enqueue time, so only an upload_links row proves
// the content is safe in the cloud.  The old implementation's embedded select
// always failed against the deployed schema, which reported "not uploaded" with an
// error for every file — including files that were uploaded.
func TestHasUploadedLinksUsesLinkTableNotRowExistence(t *testing.T) {
	var embeddedSelect bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/recordings"):
			if strings.Contains(r.URL.Query().Get("select"), "upload_links") {
				embeddedSelect = true
			}
			name := strings.TrimPrefix(r.URL.Query().Get("filename"), "in.(")
			name = strings.TrimSuffix(name, ")")
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "rec-" + name, "filename": name}})
		case strings.HasSuffix(r.URL.Path, "/upload_links"):
			ids := inFilterValues(r.URL, "recording_id")
			var rows []map[string]string
			for _, id := range ids {
				if id == "rec-uploaded.mp4" {
					rows = append(rows, map[string]string{"recording_id": id})
				}
			}
			_ = json.NewEncoder(w).Encode(rows)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, APIKey: "test-key", client: srv.Client()}

	ok, err := c.HasUploadedLinks("uploaded.mp4")
	if err != nil {
		t.Fatalf("HasUploadedLinks(uploaded.mp4): %v", err)
	}
	if !ok {
		t.Error("uploaded.mp4 has an upload_links row, want true")
	}

	ok, err = c.HasUploadedLinks("row-only.mp4")
	if err != nil {
		t.Fatalf("HasUploadedLinks(row-only.mp4): %v", err)
	}
	if ok {
		t.Error("row-only.mp4 has a recordings row but no upload_links row, want false — a row is not proof of an upload")
	}
	if embeddedSelect {
		t.Error("used an embedded upload_links select, which cannot work without the foreign key")
	}
}

// TestGetRecordingFilenamesPagesWholeTable verifies the keep set used by the
// preview_images prune is complete: a single limit=N request would silently stop
// at the server's max_rows (1000) and the prune would then delete the preview rows
// of every recording beyond the first page.
func TestGetRecordingFilenamesPagesWholeTable(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/recordings") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		pages++
		offset := r.URL.Query().Get("offset")
		if offset == "0" {
			rows := make([]map[string]string, 0, 1000)
			for i := 0; i < 1000; i++ {
				rows = append(rows, map[string]string{"filename": fmt.Sprintf("file-%04d.mp4", i)})
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"filename": "last.mp4"},
			{"filename": ""}, // malformed row must not add an empty keep entry
		})
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, APIKey: "test-key", client: srv.Client()}
	filenames, err := c.GetRecordingFilenames()
	if err != nil {
		t.Fatalf("GetRecordingFilenames: %v", err)
	}
	if pages != 2 {
		t.Errorf("paged %d time(s), want 2 (a full 1000-row page then the tail)", pages)
	}
	if len(filenames) != 1001 {
		t.Errorf("collected %d filenames, want 1001", len(filenames))
	}
	if !filenames["file-0999.mp4"] || !filenames["last.mp4"] {
		t.Error("keep set is missing rows from the second page")
	}
	if filenames[""] {
		t.Error("keep set contains an empty filename")
	}
}

// TestGetJournalSuccessWithLinksForChunksFilenames verifies the reconciliation
// lookup asks for exactly the filenames it needs, in chunks small enough for the
// proxy's URL limit, and that it is NOT window-bounded (no limit/order filter):
// scoping by filename is what lets an old gap still be repaired.
func TestGetJournalSuccessWithLinksForChunksFilenames(t *testing.T) {
	var rawQueries []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/upload_journal") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		rawQueries = append(rawQueries, r.URL.RawQuery)

		var entries []UploadJournal
		for _, name := range parseFilenameInFilter(r.URL) {
			entries = append(entries, UploadJournal{
				FileHash: "hash-" + name,
				Filename: name,
				Host:     "Mixdrop",
				Status:   "success",
				Link:     "https://mixdrop.ag/e/" + name,
			})
		}
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer srv.Close()

	names := make([]string, 0, 45)
	for i := 0; i < 45; i++ {
		names = append(names, fmt.Sprintf("stranded_%02d_2026-09-13_10-18-43.mp4", i))
	}

	c := &Client{URL: srv.URL, APIKey: "test-key", client: srv.Client()}
	entries, err := c.GetJournalSuccessWithLinksFor(names)
	if err != nil {
		t.Fatalf("GetJournalSuccessWithLinksFor: %v", err)
	}

	if len(entries) != len(names) {
		t.Fatalf("expected %d journal entries (one per requested file), got %d", len(names), len(entries))
	}
	if len(rawQueries) != 2 {
		t.Fatalf("expected 45 filenames to be chunked into 2 requests (batch size %d), got %d: %v", releaseBatchSize, len(rawQueries), rawQueries)
	}
	for i, raw := range rawQueries {
		if !strings.Contains(raw, "status=eq.success") || !strings.Contains(raw, "link=like.*") {
			t.Errorf("query %d must only select success entries that carry a link: %s", i, raw)
		}
		if strings.Contains(raw, "limit=") || strings.Contains(raw, "order=") {
			t.Errorf("query %d is window-bounded (%s); old gaps would never be repaired", i, raw)
		}
		u, err := url.Parse("?" + raw)
		if err != nil {
			t.Fatalf("query %d is not a parseable filter: %v", i, err)
		}
		if got := len(parseFilenameInFilter(u)); got > releaseBatchSize {
			t.Errorf("query %d carries %d filenames, above the %d-per-request URL budget: %s", i, got, releaseBatchSize, raw)
		}
	}
}
