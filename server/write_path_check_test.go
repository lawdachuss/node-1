package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teacat/chaturbate-dvr/database"
	"github.com/teacat/chaturbate-dvr/entity"
)

// writePathFake simulates an upload_links endpoint that either accepts inserts or
// rejects them the way a broken AFTER INSERT trigger does (HTTP 404 carrying the
// Postgres error, which is how PostgREST surfaces 42883).
type writePathFake struct {
	rejectInsert bool
	inserts      []database.UploadLink
	deletes      []string
	otherHits    []string
}

func (f *writePathFake) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !strings.HasSuffix(r.URL.Path, "/upload_links") {
		f.otherHits = append(f.otherHits, r.Method+" "+r.URL.Path)
		_ = json.NewEncoder(w).Encode([]map[string]string{})
		return
	}

	switch r.Method {
	case "POST":
		body, _ := io.ReadAll(r.Body)
		var link database.UploadLink
		_ = json.Unmarshal(body, &link)
		f.inserts = append(f.inserts, link)
		if f.rejectInsert {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"42883","message":"operator does not exist: uuid = text"}`))
			return
		}
		_ = json.NewEncoder(w).Encode([]database.UploadLink{link})

	case "DELETE":
		f.deletes = append(f.deletes, r.URL.Query().Get("host"))
		_, _ = w.Write([]byte(`[]`))

	default:
		http.Error(w, "unexpected "+r.Method, http.StatusInternalServerError)
	}
}

func newWritePathClient(t *testing.T, f *writePathFake) {
	t.Helper()
	prevConfig, prevClient := Config, dbClient
	t.Cleanup(func() {
		Config, dbClient = prevConfig, prevClient
		writePathErr, writePathCheckedAt = nil, time.Time{}
		LinkPathWriteAlert, LinkPathWriteRecovered = nil, nil
	})
	Config = &entity.Config{SupabaseURL: httptest.NewServer(http.HandlerFunc(f.handler)).URL, SupabaseAPIKey: "test-key"}
	dbClient = nil
}

// TestVerifyUploadLinkWritePathWritesAndRemovesProbe pins what the canary does to
// the database: exactly one insert and one delete, the insert carrying the
// reserved host and a recording id that cannot collide with a real recording.
func TestVerifyUploadLinkWritePathWritesAndRemovesProbe(t *testing.T) {
	fake := &writePathFake{}
	newWritePathClient(t, fake)

	if err := VerifyUploadLinkWritePath(); err != nil {
		t.Fatalf("VerifyUploadLinkWritePath: %v", err)
	}

	if len(fake.inserts) != 1 {
		t.Fatalf("issued %d insert(s), want exactly 1", len(fake.inserts))
	}
	probe := fake.inserts[0]
	if probe.Host != probeUploadLinkHost {
		t.Errorf("probe host = %q, want the reserved %q so real links can never be confused with it", probe.Host, probeUploadLinkHost)
	}
	if probe.RecordingID != probeUploadLinkRecordingID {
		t.Errorf("probe recording_id = %q, want the all-zeroes sentinel", probe.RecordingID)
	}
	if len(probe.RecordingID) != 36 || strings.Trim(probe.RecordingID, "0-") != "" {
		t.Errorf("probe recording_id %q is not the all-zeroes uuid (a real recording must never match it)", probe.RecordingID)
	}

	if len(fake.deletes) != 1 || fake.deletes[0] != "eq."+probeUploadLinkHost {
		t.Errorf("deletes = %v, want one host=eq.%s filter (BY HOST, so a probe row left behind by a killed run is cleared too)", fake.deletes, probeUploadLinkHost)
	}
}

// TestVerifyUploadLinkWritePathReportsRejectedInsert is the regression guard for
// the 2026-09-23 incident: the insert is rejected by a trigger that cannot be
// planned, while reads and the schema cache stay green.  The canary must fail
// loudly — and must not try to delete a row it never created.
func TestVerifyUploadLinkWritePathReportsRejectedInsert(t *testing.T) {
	fake := &writePathFake{rejectInsert: true}
	newWritePathClient(t, fake)

	err := VerifyUploadLinkWritePath()
	if err == nil {
		t.Fatal("VerifyUploadLinkWritePath returned nil while every insert was rejected")
	}
	if !strings.Contains(err.Error(), "insert rejected") {
		t.Errorf("error %q does not say the insert was rejected", err)
	}
	if !strings.Contains(err.Error(), "42883") {
		t.Errorf("error %q dropped the database's explanation (42883), which is what makes it diagnosable", err)
	}
	if len(fake.deletes) != 0 {
		t.Errorf("issued %d delete(s) after a failed insert", len(fake.deletes))
	}
}

// TestCheckUploadLinkWritePathAlertsOnceAndClearsOnRecovery verifies the alerting
// contract: a closed write path raises a keyed alert and marks the panel status,
// and a later success clears the cooldown so a recurrence is reported at once.
func TestCheckUploadLinkWritePathAlertsOnceAndClearsOnRecovery(t *testing.T) {
	fake := &writePathFake{rejectInsert: true}
	newWritePathClient(t, fake)

	var (
		mu        sync.Mutex
		alerts    [][3]string
		recovered []string
	)
	LinkPathWriteAlert = func(key, title, message string) {
		mu.Lock()
		alerts = append(alerts, [3]string{key, title, message})
		mu.Unlock()
	}
	LinkPathWriteRecovered = func(key string) {
		mu.Lock()
		recovered = append(recovered, key)
		mu.Unlock()
	}

	if err := CheckUploadLinkWritePath(); err == nil {
		t.Fatal("CheckUploadLinkWritePath returned nil while inserts were rejected")
	}

	mu.Lock()
	if len(alerts) != 1 {
		t.Fatalf("raised %d alert(s), want 1 (%v)", len(alerts), alerts)
	}
	if alerts[0][0] != keyUploadLinkWriteFailure {
		t.Errorf("alert key = %q, want %q (the notifier cooldowns per key)", alerts[0][0], keyUploadLinkWriteFailure)
	}
	if !strings.Contains(alerts[0][2], "no host") {
		t.Errorf("alert message %q does not explain the consequence (recordings stored with no host)", alerts[0][2])
	}
	mu.Unlock()

	st := UploadLinkWritePathStatus()
	if !st.Checked || st.OK {
		t.Errorf("status after a failed check = %+v, want Checked=true OK=false", st)
	}
	if !strings.Contains(st.Detail, "42883") {
		t.Errorf("status detail %q does not carry the underlying error", st.Detail)
	}

	fake.rejectInsert = false
	if err := CheckUploadLinkWritePath(); err != nil {
		t.Fatalf("CheckUploadLinkWritePath after the path recovered: %v", err)
	}
	if st := UploadLinkWritePathStatus(); !st.Checked || !st.OK {
		t.Errorf("status after recovery = %+v, want Checked=true OK=true", st)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recovered) != 1 || recovered[0] != keyUploadLinkWriteFailure {
		t.Errorf("recovered hooks = %v, want exactly [%s] so the next failure is reported immediately", recovered, keyUploadLinkWriteFailure)
	}
	if len(alerts) != 1 {
		t.Errorf("raised %d alert(s) after recovery, want no further alert", len(alerts))
	}
}

// TestReconcileMissingUploadLinksAbortsWhenWritePathBroken pins the preflight: a
// sweep that cannot write must not walk the journal and log hundreds of doomed
// inserts, and must report the broken path rather than "restored 0".
func TestReconcileMissingUploadLinksAbortsWhenWritePathBroken(t *testing.T) {
	fake := &writePathFake{rejectInsert: true}
	prevConfig, prevClient := Config, dbClient
	cacheClear()
	defer func() {
		Config, dbClient = prevConfig, prevClient
		writePathErr, writePathCheckedAt = nil, time.Time{}
		LinkPathWriteAlert = nil
		cacheClear()
	}()

	noHostHits, journalHits := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload_links"):
			fake.handler(w, r)
		case strings.HasSuffix(r.URL.Path, noHostRPC):
			noHostHits++
			_ = json.NewEncoder(w).Encode([]map[string]string{{"filename": "stranded_2026-09-13_10-18-43.mp4"}})
		case strings.HasSuffix(r.URL.Path, "/upload_journal"):
			journalHits++
			_ = json.NewEncoder(w).Encode([]database.UploadJournal{})
		default:
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		}
	}))
	defer srv.Close()

	alerted := 0
	LinkPathWriteAlert = func(key, title, message string) { alerted++ }

	Config = &entity.Config{SupabaseURL: srv.URL, SupabaseAPIKey: "test-key"}
	dbClient = nil

	if restored := ReconcileMissingUploadLinks(); restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}
	if alerted != 1 {
		t.Errorf("alerts = %d, want 1 for a write path that rejects every link", alerted)
	}
	if journalHits != 0 {
		t.Errorf("journal was queried %d time(s) even though nothing could be written back", journalHits)
	}
	if noHostHits != 0 {
		t.Errorf("the no-host lookup ran %d time(s) before the write path was known to be broken", noHostHits)
	}
	if len(fake.inserts) != 1 {
		t.Errorf("inserts = %d, want exactly the 1 canary probe", len(fake.inserts))
	}
}
