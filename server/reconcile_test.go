package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/teacat/chaturbate-dvr/database"
	"github.com/teacat/chaturbate-dvr/entity"
)

// noHostRPC is the path suffix of the recordings_without_upload_links() call.
const noHostRPC = "/rpc/recordings_without_upload_links"

// ackWritePathProbe answers the upload-link write-path canary's probe insert and
// its cleanup delete, so a mock that is about the sweep does not have to model the
// probe.  Returns true when it handled the request.
func ackWritePathProbe(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, "/upload_links") || r.Method == "GET" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`[]`))
	return true
}

// filenamesInFilter extracts the values of a `?filename=in.(a,b)` filter.
func filenamesInFilter(r *http.Request) []string {
	inner := r.URL.Query().Get("filename")
	if !strings.HasPrefix(inner, "in.(") || !strings.HasSuffix(inner, ")") {
		return nil
	}
	inner = inner[len("in.(") : len(inner)-1]
	if inner == "" {
		return nil
	}
	return strings.Split(inner, ",")
}

// TestReconcileMissingUploadLinksRepairsOldGaps verifies the sweep is driven by
// the recordings that currently have ZERO upload links rather than by a recent
// journal window: the journal entry it rebuilds from is months old, and the
// queries it issues carry no limit/order filter, so a gap left by a crash weeks
// ago is still repaired on the next sweep.
//
// It also pins the cost shape: the already-linked recording is never asked about,
// so the sweep scales with the (small) broken set, not with the journal.
func TestReconcileMissingUploadLinksRepairsOldGaps(t *testing.T) {
	prevConfig, prevClient := Config, dbClient
	cacheClear()
	defer func() {
		Config, dbClient = prevConfig, prevClient
		cacheClear()
	}()

	const (
		strandedName = "stranded_2026-09-13_10-18-43.mp4"
		linkedName   = "uploaded_2026-09-13_10-18-43.mp4"
		oldLink      = "https://mixdrop.ag/e/old-gap-link"
	)

	var (
		mu             sync.Mutex
		journalQueries []string
		recLookups     [][]string
		posted         []database.UploadLink
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, noHostRPC):
			// Only the stranded recording has zero upload links fleet-wide.
			_ = json.NewEncoder(w).Encode([]map[string]string{{"filename": strandedName}})

		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/upload_journal"):
			mu.Lock()
			journalQueries = append(journalQueries, r.URL.RawQuery)
			mu.Unlock()
			// A success recorded long ago, whose per-host link save never landed.
			_ = json.NewEncoder(w).Encode([]database.UploadJournal{{
				FileHash:  "hash-stranded",
				Filename:  strandedName,
				Host:      "Mixdrop",
				Status:    "success",
				Link:      oldLink,
				CreatedAt: "2026-06-01T00:00:00Z",
				UpdatedAt: "2026-06-01T00:00:00Z",
			}})

		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/recordings"):
			names := filenamesInFilter(r)
			mu.Lock()
			recLookups = append(recLookups, names)
			mu.Unlock()
			rows := make([]map[string]string, 0, len(names))
			for _, name := range names {
				rows = append(rows, map[string]string{"id": "rec-" + name, "filename": name})
			}
			_ = json.NewEncoder(w).Encode(rows)

		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/upload_links"):
			// The stranded recording has no link rows, which is why it is stranded.
			_ = json.NewEncoder(w).Encode([]map[string]string{})

		case r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/upload_links"):
			// The write-path canary removing its probe row.
			_, _ = w.Write([]byte(`[]`))

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/upload_links"):
			var link database.UploadLink
			_ = json.NewDecoder(r.Body).Decode(&link)
			if link.Host == probeUploadLinkHost {
				// The canary's own probe: acknowledged, never counted as a restored
				// link.
				_ = json.NewEncoder(w).Encode([]database.UploadLink{link})
				return
			}
			mu.Lock()
			posted = append(posted, link)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode([]database.UploadLink{link})

		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	Config = &entity.Config{SupabaseURL: srv.URL, SupabaseAPIKey: "test-key"}
	dbClient = nil

	if restored := ReconcileMissingUploadLinks(); restored != 1 {
		t.Fatalf("restored = %d, want 1 (the stranded recording's journal success)", restored)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(posted) != 1 {
		t.Fatalf("expected exactly 1 upload_links upsert, got %d (%v)", len(posted), posted)
	}
	want := database.UploadLink{RecordingID: "rec-" + strandedName, Host: "Mixdrop", URL: oldLink}
	if posted[0].RecordingID != want.RecordingID || posted[0].Host != want.Host || posted[0].URL != want.URL {
		t.Errorf("posted link = %+v, want recording_id=%s host=%s url=%s", posted[0], want.RecordingID, want.Host, want.URL)
	}

	if len(recLookups) != 1 {
		t.Fatalf("resolved recording ids in %d request(s), want 1: %v", len(recLookups), recLookups)
	}
	for _, name := range recLookups[0] {
		if name == linkedName {
			t.Errorf("looked up %s, which already has upload links — the sweep must only walk the zero-link set", name)
		}
	}
	if len(recLookups[0]) != 1 || recLookups[0][0] != strandedName {
		t.Errorf("recording lookups = %v, want exactly [%s]", recLookups, strandedName)
	}

	if len(journalQueries) != 1 {
		t.Fatalf("expected 1 batched journal query, got %d (%v)", len(journalQueries), journalQueries)
	}
	if strings.Contains(journalQueries[0], "limit=") || strings.Contains(journalQueries[0], "order=") {
		t.Errorf("journal query is window-bounded (%s); old gaps would never be repaired", journalQueries[0])
	}
	if !strings.Contains(journalQueries[0], "filename=in.(") {
		t.Errorf("journal query is not scoped to the zero-link filenames: %s", journalQueries[0])
	}
}

// TestReconcileMissingUploadLinksNoopWhenEverythingHasLinks covers the common
// case: nothing to repair, so the sweep must not touch the journal at all.
func TestReconcileMissingUploadLinksNoopWhenEverythingHasLinks(t *testing.T) {
	prevConfig, prevClient := Config, dbClient
	cacheClear()
	defer func() {
		Config, dbClient = prevConfig, prevClient
		cacheClear()
	}()

	var journalHits, recordedQueries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ackWritePathProbe(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, noHostRPC):
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/upload_journal"):
			journalHits++
			_ = json.NewEncoder(w).Encode([]database.UploadJournal{})
		case strings.HasSuffix(r.URL.Path, "/recordings"):
			recordedQueries++
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	Config = &entity.Config{SupabaseURL: srv.URL, SupabaseAPIKey: "test-key"}
	dbClient = nil

	if restored := ReconcileMissingUploadLinks(); restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}
	if journalHits != 0 {
		t.Errorf("journal was queried %d time(s) with no zero-link recordings to repair", journalHits)
	}
	if recordedQueries != 0 {
		t.Errorf("recordings were queried %d time(s) with nothing to repair", recordedQueries)
	}
}

// TestReconcileMissingUploadLinksSkipsWhenFuncMissing pins the degradation path
// for a project where the 20260923000000 migration has not been applied: the
// sweep reports and stops instead of treating every journal success as a
// candidate (which would mean paging the whole journal on every startup).
func TestReconcileMissingUploadLinksSkipsWhenFuncMissing(t *testing.T) {
	prevConfig, prevClient := Config, dbClient
	cacheClear()
	defer func() {
		Config, dbClient = prevConfig, prevClient
		cacheClear()
	}()

	journalHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ackWritePathProbe(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, noHostRPC):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"PGRST202","message":"Could not find the function public.recordings_without_upload_links"}`))
		case strings.HasSuffix(r.URL.Path, "/upload_journal"):
			journalHits++
			_ = json.NewEncoder(w).Encode([]database.UploadJournal{})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	Config = &entity.Config{SupabaseURL: srv.URL, SupabaseAPIKey: "test-key"}
	dbClient = nil

	if restored := ReconcileMissingUploadLinks(); restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}
	if journalHits != 0 {
		t.Errorf("journal was queried %d time(s) without the no-host function", journalHits)
	}
}

// TestRecordingUploadLinkIndexSeparatesRowsFromLinks verifies the index the
// orphan list and the admin panel rely on: a filename with a recordings row but
// no upload link must NOT be reported as safe in the cloud, and the (small)
// no-host list is cached with the (large) row list.
func TestRecordingUploadLinkIndexSeparatesRowsFromLinks(t *testing.T) {
	prevConfig, prevClient := Config, dbClient
	cacheClear()
	defer func() {
		Config, dbClient = prevConfig, prevClient
		cacheClear()
	}()

	rpcHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, noHostRPC):
			rpcHits++
			_ = json.NewEncoder(w).Encode([]map[string]string{{"filename": "hostless.mp4"}})
		case strings.HasSuffix(r.URL.Path, "/recordings"):
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"filename": "uploaded.mp4"},
				{"filename": "hostless.mp4"},
			})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	Config = &entity.Config{SupabaseURL: srv.URL, SupabaseAPIKey: "test-key"}
	dbClient = nil

	all, safe, err := RecordingUploadLinkIndex()
	if err != nil {
		t.Fatalf("RecordingUploadLinkIndex: %v", err)
	}
	if len(all) != 2 || !all["uploaded.mp4"] || !all["hostless.mp4"] {
		t.Fatalf("all = %v, want both recordings", all)
	}
	if !safe["uploaded.mp4"] {
		t.Error("uploaded.mp4 has an upload link, want it marked safe in the cloud")
	}
	if safe["hostless.mp4"] {
		t.Error("hostless.mp4 has a recordings row but no upload link — a row is not proof of an upload")
	}
	if len(all)-len(safe) != 1 {
		t.Errorf("no-host count = %d, want 1", len(all)-len(safe))
	}

	// Second call inside the TTL must not hit the database again.
	if _, _, err := RecordingUploadLinkIndex(); err != nil {
		t.Fatalf("RecordingUploadLinkIndex (cached): %v", err)
	}
	if rpcHits != 1 {
		t.Errorf("no-host RPC called %d time(s) for two reads inside the TTL, want 1", rpcHits)
	}
}
