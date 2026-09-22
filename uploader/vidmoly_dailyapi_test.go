package uploader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// vidmolyDailyAPIResponse is the exact body VidMoly returns once the key's
// 50-requests/day API budget is gone (captured live: used_today 2416, limit 50).
const vidmolyDailyAPIResponse = `{"status":429,"msg":"Daily API limit reached (50).","limit":50,"used_today":2416,"remaining_today":0,"server_time":"2026-09-22 05:32:51"}`

// TestVidMolyUploader_DailyAPILimitIsNotRetried is a regression test for the
// masking bug: VidMoly reports its API cap as {"status":429,...}, and the
// generic isUploadRateLimited matcher (which matches a bare "429") used to
// swallow that signal.  The retry loop then burned all attempts, reported the
// misleading "all keys exhausted", and never disabled the host — so every file
// re-walked the dead key and the fleet hit used_today 2416 against a limit of
// 50.  The limit must now be recognized immediately, with no retry.
func TestVidMolyUploader_DailyAPILimitIsNotRetried(t *testing.T) {
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	lookups := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vidmolyDailyAPIResponse))
	}))
	defer srv.Close()
	vidmolyAPIBase = srv.URL

	u := NewVidMolyUploader("test-key")
	_, err := u.Upload(tempVideoFile(t))
	if err == nil {
		t.Fatal("expected an error when the daily API limit is reached")
	}
	if strings.Contains(err.Error(), "all keys exhausted") {
		t.Errorf("daily API limit was reported as the misleading 'all keys exhausted': %v", err)
	}
	if !strings.Contains(err.Error(), "Daily API limit") {
		t.Errorf("error should preserve the host's daily-limit wording, got: %v", err)
	}
	if lookups != 1 {
		t.Fatalf("a daily API limit must not be retried: %d upload-server lookups, want 1", lookups)
	}
}

// TestVidMolyUploader_DailyAPILimitTimedDisables verifies the multi-host layer
// reacts to the surfaced error by skipping VidMoly for ~24h (the limit is per
// calendar day and lifts on its own), so the rest of the file queue never
// touches the dead key.
func TestVidMolyUploader_DailyAPILimitTimedDisables(t *testing.T) {
	defer clearTimedDisablesForTest()
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vidmolyDailyAPIResponse))
	}))
	defer srv.Close()
	vidmolyAPIBase = srv.URL

	m := NewMultiHostUploader("", "", "", "", "", "", "test-key", &nilLogger{})
	results := m.UploadSelected(tempVideoFile(t), []string{"VidMoly"})
	if len(results) != 1 || results[0].Error == nil {
		t.Fatalf("expected one failed result for the daily limit, got %+v", results)
	}
	if !isHostTimedOut("VidMoly") {
		t.Fatal("VidMoly should be timed-disabled after hitting its daily API limit")
	}
	if remaining := time.Until(hostTimedOutUntil("VidMoly")); remaining < 23*time.Hour {
		t.Errorf("timed disable should be ~24h, got %s", remaining)
	}
}

// TestVidMolyUploader_DailyAPILimitRotatesToNextKey confirms a multi-key ring
// still gets value from its remaining keys: only the exhausted key is skipped,
// not the host.
func TestVidMolyUploader_DailyAPILimitRotatesToNextKey(t *testing.T) {
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload/server"):
			key := r.URL.Query().Get("key")
			keys = append(keys, key)
			w.Header().Set("Content-Type", "application/json")
			if key == "spent-key" {
				_, _ = w.Write([]byte(vidmolyDailyAPIResponse))
				return
			}
			b, _ := json.Marshal(vidmolyServerResponse{Status: 200, Msg: "OK", Result: "http://" + r.Host + "/upload/01"})
			w.Write(b)
		case r.URL.Path == "/upload/01":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<textarea name="st">OK</textarea><textarea name="fn">goodcode123</textarea>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	vidmolyAPIBase = srv.URL

	u := NewVidMolyUploader("spent-key,fresh-key")
	link, err := u.Upload(tempVideoFile(t))
	if err != nil {
		t.Fatalf("expected the fresh key to succeed, got %v", err)
	}
	if link != "https://vidmoly.me/goodcode123" {
		t.Fatalf("unexpected link %q", link)
	}
	if len(keys) != 2 || keys[0] != "spent-key" || keys[1] != "fresh-key" {
		t.Fatalf("expected rotation to the fresh key, lookups were %v", keys)
	}
}
