package uploader

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func tempVideoFile(t *testing.T) string {
	t.Helper()
	const mp4Header = "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\x00\x00\x08moov"
	f, err := os.CreateTemp("", "vidmoly-test-*.mp4")
	if err != nil {
		t.Fatalf("create temp video: %v", err)
	}
	if _, err := f.WriteString(mp4Header); err != nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatalf("write temp video: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestVidMolyUploader_Success(t *testing.T) {
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	uploadURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/upload/server":
			if r.Method != "GET" {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if r.URL.Query().Get("key") != "test-key" {
				t.Errorf("server: expected key=test-key, got %q", r.URL.Query().Get("key"))
			}
			resp := vidmolyServerResponse{
				Status: 200,
				Msg:    "OK",
				Result: uploadURL,
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case "/upload/01":
			if r.Method != "POST" {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("server: parse multipart: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			if apiKey := r.FormValue("api_key"); apiKey != "test-key" {
				t.Errorf("server: expected api_key=test-key, got %q", apiKey)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("server: missing file field: %v", err)
				http.Error(w, "no file", http.StatusBadRequest)
				return
			}
			io.Copy(io.Discard, file)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<Form name='F1'><textarea name="op">upload_result</textarea><textarea name="fn">yhap17mcn59m</textarea><textarea name="st">OK</textarea></Form>`)
		default:
			http.NotFound(w, r)
		}
	}))
	uploadURL = srv.URL + "/upload/01"
	defer srv.Close()

	vidmolyAPIBase = srv.URL

	u := NewVidMolyUploader("test-key")
	link, err := u.Upload(tempVideoFile(t))
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	expected := "https://vidmoly.me/yhap17mcn59m"
	if link != expected {
		t.Fatalf("expected link %q, got %q", expected, link)
	}
}

func TestVidMolyUploader_AuthErrorRotatesKey(t *testing.T) {
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	callCount := 0
	uploadURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch r.URL.Path {
		case "/api/upload/server":
			key := r.URL.Query().Get("key")
			resp := vidmolyServerResponse{}
			if key == "bad-key" {
				resp.Status = 403
				resp.Msg = "invalid api key"
			} else if key == "good-key" {
				resp.Status = 200
				resp.Msg = "OK"
				resp.Result = uploadURL
			} else {
				http.Error(w, "unknown key", http.StatusBadRequest)
				return
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case "/upload/01":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<Form name='F1'><textarea name="op">upload_result</textarea><textarea name="fn">abc123xyz</textarea><textarea name="st">OK</textarea></Form>`)
		default:
			http.NotFound(w, r)
		}
	}))
	uploadURL = srv.URL + "/upload/01"
	defer srv.Close()

	vidmolyAPIBase = srv.URL

	u := NewVidMolyUploader("bad-key,good-key")
	link, err := u.Upload(tempVideoFile(t))
	if err != nil {
		t.Fatalf("upload should succeed on second key: %v", err)
	}
	if link != "https://vidmoly.me/abc123xyz" {
		t.Fatalf("expected link from second key, got %q", link)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 calls (bad then good), got %d", callCount)
	}
}

func TestVidMolyUploader_DailyLimitDetectedAndTimedDisabled(t *testing.T) {
	defer clearTimedDisablesForTest()
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	uploadURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/upload/server":
			resp := vidmolyServerResponse{
				Status: 200,
				Msg:    "OK",
				Result: uploadURL,
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case "/upload/01":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<Form name='F1'><textarea name="op">upload_result</textarea><textarea name="fn">ignored</textarea><textarea name="st">Daily upload limit reached</textarea></Form>`)
		default:
			http.NotFound(w, r)
		}
	}))
	uploadURL = srv.URL + "/upload/01"
	defer srv.Close()

	vidmolyAPIBase = srv.URL

	// Use MultiHostUploader to exercise the timed-disable path (which lives there)
	m := NewMultiHostUploader("", "", "", "", "", "", "test-key", &nilLogger{})
	results := m.UploadSelected(tempVideoFile(t), []string{"VidMoly"})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Error == nil {
		t.Fatal("expected error for daily limit")
	}
	errMsg := results[0].Error.Error()
	if !strings.Contains(errMsg, "Daily upload limit") && !strings.Contains(errMsg, "daily upload limit") {
		t.Errorf("error should mention daily limit, got: %v", errMsg)
	}

	// The timed disable must be active now
	if !isHostTimedOut("VidMoly") {
		t.Fatal("VidMoly should be timed-disabled after daily limit error")
	}
	until := hostTimedOutUntil("VidMoly")
	if until.IsZero() {
		t.Fatal("hostTimedOutUntil should return non-zero expiry")
	}
	if time.Until(until) < 23*time.Hour {
		t.Errorf("timed disable should be ~24h, got %s", time.Until(until))
	}
}

func TestVidMolyUploader_DailyLimitSkipsOnFreshUploader(t *testing.T) {
	defer clearTimedDisablesForTest()

	disableHostFor("VidMoly", 24*time.Hour)
	defer clearTimedDisablesForTest()

	var callCount int
	failFn := func(filePath string, progress ProgressFunc) (string, error) {
		callCount++
		return "", fmt.Errorf("Daily upload limit reached")
	}

	m := &MultiHostUploader{
		hosts: map[string]uploaderFunc{
			"VidMoly": failFn,
		},
		log: &nilLogger{},
	}

	_ = m.UploadSelected("test.mp4", []string{"VidMoly"})

	if callCount != 0 {
		t.Fatalf("VidMoly upload func should never be called when timed-disabled, got %d calls", callCount)
	}
}

func TestVidMolyUploader_NonVideoReject_FailFast(t *testing.T) {
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	uploadURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/upload/server":
			resp := vidmolyServerResponse{
				Status: 200,
				Msg:    "OK",
				Result: uploadURL,
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case "/upload/01":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<Form name='F1'><textarea name="op">upload_result</textarea><textarea name="fn">ignored</textarea><textarea name="st">Not video file format</textarea></Form>`)
		default:
			http.NotFound(w, r)
		}
	}))
	uploadURL = srv.URL + "/upload/01"
	defer srv.Close()

	vidmolyAPIBase = srv.URL

	u := NewVidMolyUploader("test-key")
	_, err := u.Upload(tempVideoFile(t))
	if err == nil {
		t.Fatal("expected error for non-video file")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not video") {
		t.Errorf("error should mention non-video, got: %v", err)
	}
}

func TestVidMolyUploader_CapacityError_FailFast(t *testing.T) {
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	uploadURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/upload/server":
			resp := vidmolyServerResponse{
				Status: 200,
				Msg:    "OK",
				Result: uploadURL,
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case "/upload/01":
			http.Error(w, "no healthy upload server", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	uploadURL = srv.URL + "/upload/01"
	defer srv.Close()

	vidmolyAPIBase = srv.URL

	u := NewVidMolyUploader("test-key")
	_, err := u.Upload(tempVideoFile(t))
	if err == nil {
		t.Fatal("expected error for capacity failure")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "no healthy upload server") && !strings.Contains(strings.ToLower(err.Error()), "status 503") {
		t.Errorf("error should mention capacity failure, got: %v", err)
	}
}

func TestVidMolyUploader_RateLimitBackoff(t *testing.T) {
	defer func(old string) { vidmolyAPIBase = old }(vidmolyAPIBase)

	attempt := 0
	uploadURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		switch r.URL.Path {
		case "/api/upload/server":
			resp := vidmolyServerResponse{
				Status: 200,
				Msg:    "OK",
				Result: uploadURL,
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		case "/upload/01":
			if attempt == 1 {
				http.Error(w, "rate limit: too many requests", http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<Form name='F1'><textarea name="op">upload_result</textarea><textarea name="fn">rate123</textarea><textarea name="st">OK</textarea></Form>`)
		default:
			http.NotFound(w, r)
		}
	}))
	uploadURL = srv.URL + "/upload/01"
	defer srv.Close()

	vidmolyAPIBase = srv.URL

	u := NewVidMolyUploader("test-key")
	link, err := u.Upload(tempVideoFile(t))
	if err != nil {
		t.Fatalf("upload should succeed on retry: %v", err)
	}
	if link != "https://vidmoly.me/rate123" {
		t.Errorf("expected retry link, got %q", link)
	}
	if attempt != 2 {
		t.Errorf("expected 2 attempts (1 rate limit + 1 success), got %d", attempt)
	}
}

func TestVidMolyDailyLimitClassifier(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect bool
	}{
		{"daily upload limit reached", fmt.Errorf("Daily upload limit reached"), true},
		{"daily limit exceeded", fmt.Errorf("Daily limit exceeded"), true},
		{"limit reached for today", fmt.Errorf("Limit reached for today"), true},
		{"per day limit", fmt.Errorf("Upload limit: 50 per day"), true},
		{"today's upload limit", fmt.Errorf("Today's upload limit exceeded"), true},
		{"rate limit (not daily)", fmt.Errorf("rate limit: too many requests"), false},
		{"file too large", fmt.Errorf("file too large: exceeds 10GB limit"), false},
		{"storage full (VOE)", fmt.Errorf("VOE.sx storage full"), false},
		{"auth failure", fmt.Errorf("invalid api key"), false},
		{"nil error", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if isVidMolyDailyLimit(tc.err) != tc.expect {
				t.Errorf("isVidMolyDailyLimit(%q) = %v, want %v", tc.err, !tc.expect, tc.expect)
			}
		})
	}
}