package database

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLeaseDB is a stand-in for PostgREST's conditional PATCH: it records the
// requests and returns the rows the filter matched, exactly like
// Prefer: return=representation does in production.
type fakeLeaseDB struct {
	mu       sync.Mutex
	requests []reqRecord
	// matched is the representation returned for each PATCH in order; an entry
	// of false simulates a PATCH whose conditional filter matched no row
	// (another node won the race).
	matched []bool
	patches int
}

func (f *fakeLeaseDB) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, reqRecord{method: r.Method, path: r.URL.RequestURI(), body: string(body)})
	idx := f.patches
	f.patches++
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r.Method != "PATCH" {
		fmt.Fprint(w, `[]`)
		return
	}
	if idx < len(f.matched) && f.matched[idx] {
		fmt.Fprint(w, `[{"id":"rec-1"}]`)
		return
	}
	fmt.Fprint(w, `[]`)
}

func newLeaseTestClient(t *testing.T, fake *fakeLeaseDB) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, APIKey: "test-key", client: srv.Client()}
}

// TestClaimRecordingThumbAttemptWinsThenLosesFleetRace pins the fleet-wide
// lease: the first node's conditional PATCH matches the row (claim granted) and
// the next node's PATCH matches nothing (claim refused), so only one node
// downloads a given recording per cooldown window.
func TestClaimRecordingThumbAttemptWinsThenLosesFleetRace(t *testing.T) {
	fake := &fakeLeaseDB{matched: []bool{true, false}}
	c := newLeaseTestClient(t, fake)

	claimed, err := c.ClaimRecordingThumbAttempt("alice_2026-09-22_04-14-16.mp4", 3*time.Hour)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed {
		t.Fatal("first node should win the lease")
	}

	claimed, err = c.ClaimRecordingThumbAttempt("alice_2026-09-22_04-14-16.mp4", 3*time.Hour)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("second node must not win a lease already held by the first")
	}
}

// TestClaimRecordingThumbAttemptFilterAndStamp pins the wire shape: the PATCH
// must carry the cooldown window in its filter (so a lease inside the window is
// never stolen) and must stamp the attempt time BEFORE the download starts.
func TestClaimRecordingThumbAttemptFilterAndStamp(t *testing.T) {
	fake := &fakeLeaseDB{matched: []bool{true}}
	c := newLeaseTestClient(t, fake)

	if _, err := c.ClaimRecordingThumbAttempt("weird name+chars.mp4", 3*time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(fake.requests))
	}
	rec := fake.requests[0]
	if rec.method != "PATCH" {
		t.Fatalf("method = %s, want PATCH", rec.method)
	}
	for _, want := range []string{
		"/rest/v1/recordings?",
		"filename=eq.weird+name%2Bchars.mp4",
		"thumbnail_url=is.null",
		"or=(thumb_attempt_at.is.null,thumb_attempt_at.lt.",
		"select=id",
	} {
		if !strings.Contains(rec.path, want) {
			t.Errorf("claim path missing %q: %s", want, rec.path)
		}
	}
	if !strings.Contains(rec.body, `"thumb_attempt_at":`) {
		t.Errorf("claim body should stamp thumb_attempt_at, got: %s", rec.body)
	}
}

// TestClaimRecordingThumbAttemptEmptyFilenameIsNoop: no filename means nothing
// to lease, and no request must be issued.
func TestClaimRecordingThumbAttemptEmptyFilenameIsNoop(t *testing.T) {
	fake := &fakeLeaseDB{}
	c := newLeaseTestClient(t, fake)

	claimed, err := c.ClaimRecordingThumbAttempt("", 3*time.Hour)
	if err != nil {
		t.Fatalf("claim(\"\"): %v", err)
	}
	if claimed {
		t.Fatal("an empty filename must not be claimed")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Fatalf("request count = %d, want 0", len(fake.requests))
	}
}

// TestRecordingThumbAttemptColumnMissing pins the graceful-degradation
// classifier: only a PostgREST/Postgres "column not found" error mentioning the
// lease column counts, so a genuinely durable schema error still surfaces
// instead of silently disabling the fleet coordination.
func TestRecordingThumbAttemptColumnMissing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "PGRST204 schema cache miss",
			err:  fmt.Errorf("HTTP 400: {\"code\":\"PGRST204\",\"message\":\"Could not find the 'thumb_attempt_at' column of 'recordings' in the schema cache\"}"),
			want: true,
		},
		{
			name: "postgres undefined_column",
			err:  fmt.Errorf("HTTP 400: {\"code\":\"42703\",\"message\":\"column \\\"thumb_attempt_at\\\" does not exist\"}"),
			want: true,
		},
		{
			name: "PGRST204 for an unrelated column",
			err:  fmt.Errorf("HTTP 400: {\"code\":\"PGRST204\",\"message\":\"Could not find the 'sprite_url' column\"}"),
			want: false,
		},
		{
			name: "durable failure",
			err:  fmt.Errorf("HTTP 401: unauthorized"),
			want: false,
		},
		{name: "nil", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecordingThumbAttemptColumnMissing(tc.err); got != tc.want {
				t.Errorf("RecordingThumbAttemptColumnMissing(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
