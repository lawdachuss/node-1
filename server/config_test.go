package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/teacat/chaturbate-dvr/entity"
)

// fakeSupabase is an in-memory app_settings store that mimics the Supabase
// REST endpoints saveJSONSetting / loadJSONSetting rely on (GET/POST/PATCH).
type fakeSupabase struct {
	mu sync.Mutex
	m  map[string]json.RawMessage
}

func newFakeSupabase() *fakeSupabase { return &fakeSupabase{m: map[string]json.RawMessage{}} }

// keyFromQuery extracts the key from "?key=eq.<key>..." (RawQuery form).
func (f *fakeSupabase) keyFromQuery(rawQuery string) string {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	k := q.Get("key")
	if len(k) > 3 && k[:3] == "eq." {
		return k[3:]
	}
	return k
}

func (f *fakeSupabase) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			k := f.keyFromQuery(r.URL.RawQuery)
			if v, ok := f.m[k]; ok {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[{"key":` + mustJSON(k) + `,"value":` + string(v) + `}]`))
				return
			}
			w.Write([]byte(`[]`))
		case http.MethodPost:
			var body struct {
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.m[body.Key] = body.Value
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"key":` + mustJSON(body.Key) + `,"value":` + string(body.Value) + `}]`))
		case http.MethodPatch:
			var body struct {
				Value json.RawMessage `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			k := f.keyFromQuery(r.URL.RawQuery)
			f.m[k] = body.Value
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"key":` + mustJSON(k) + `,"value":` + string(body.Value) + `}]`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestPerNodeCookieSaveLoadRoundTrip verifies the end-to-end split: cookies go
// to the per-node key (dvr_settings:<node_id>), upload creds stay global, and
// LoadSettings restores both from the right keys.
func TestPerNodeCookieSaveLoadRoundTrip(t *testing.T) {
	ts := httptest.NewServer(newFakeSupabase().handler())
	defer ts.Close()

	oldID := os.Getenv("NODE_ID")
	os.Setenv("NODE_ID", "node-99")
	defer os.Setenv("NODE_ID", oldID)

	oldConfig := Config
	Config = &entity.Config{
		SupabaseURL:     ts.URL,
		SupabaseAPIKey:  "test-key",
		Cookies:         "cf_clearance=NODE_COOKIE; csrftoken=tok",
		CfClearance:     "NODE_COOKIE",
		Csrftoken:       "tok",
		UserAgent:       "UA",
		VoeSXAPIKey:     "voe-key",
		StreamtapeKey:   "st-key",
		StreamtapeLogin: "st-login",
		AffiliateWM:     "wm-123",
	}
	defer func() { Config = oldConfig }()

	if err := SaveSettings(); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Per-node key must hold cookies, never upload creds.
	nodeKey := CookieSettingsKey()
	if nodeKey != "dvr_settings:node-99" {
		t.Fatalf("CookieSettingsKey = %q, want dvr_settings:node-99", nodeKey)
	}
	perNode := LoadSettingsFromDBKey(nodeKey)
	if perNode == nil {
		t.Fatalf("per-node key %q not written", nodeKey)
	}
	var node persistedSettings
	if err := json.Unmarshal(perNode, &node); err != nil {
		t.Fatalf("unmarshal per-node blob: %v", err)
	}
	if node.Cookies != "cf_clearance=NODE_COOKIE; csrftoken=tok" {
		t.Errorf("per-node cookies = %q", node.Cookies)
	}
	if node.CfClearance != "NODE_COOKIE" {
		t.Errorf("per-node cf_clearance = %q", node.CfClearance)
	}
	if node.UserAgent != "UA" {
		t.Errorf("per-node user_agent = %q", node.UserAgent)
	}
	if node.VoeSXAPIKey != "" || node.StreamtapeKey != "" || node.AffiliateWM != "" {
		t.Errorf("per-node blob leaked upload creds: %+v", node)
	}

	// Global key must hold creds, never cookies.
	global := LoadSettingsFromDB()
	if global == nil {
		t.Fatalf("global dvr_settings not written")
	}
	var g persistedSettings
	if err := json.Unmarshal(global, &g); err != nil {
		t.Fatalf("unmarshal global blob: %v", err)
	}
	if g.VoeSXAPIKey != "voe-key" || g.StreamtapeKey != "st-key" {
		t.Errorf("global creds = voe:%q st:%q", g.VoeSXAPIKey, g.StreamtapeKey)
	}
	if g.AffiliateWM != "wm-123" {
		t.Errorf("global creds affiliate_wm = %q, want wm-123", g.AffiliateWM)
	}
	if g.Cookies != "" || g.CfClearance != "" {
		t.Errorf("global key leaked cookies: %+v", g)
	}

	// Reset cookies in memory, then LoadSettings from DB and confirm restore.
	Config.Cookies = ""
	Config.CfClearance = ""
	Config.Csrftoken = ""
	Config.VoeSXAPIKey = ""
	Config.StreamtapeKey = ""
	Config.AffiliateWM = ""
	if err := LoadSettings(); err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if Config.Cookies != "cf_clearance=NODE_COOKIE; csrftoken=tok" {
		t.Errorf("after load cookies = %q", Config.Cookies)
	}
	if Config.VoeSXAPIKey != "voe-key" {
		t.Errorf("after load voesx = %q", Config.VoeSXAPIKey)
	}
	if Config.StreamtapeKey != "st-key" {
		t.Errorf("after load streamtape = %q", Config.StreamtapeKey)
	}
	if Config.AffiliateWM != "wm-123" {
		t.Errorf("after load affiliate_wm = %q, want wm-123", Config.AffiliateWM)
	}
}

// TestStaggerSessionDurationDeterministic verifies the offset is stable for a
// given node ID and stays within [0, spread).
func TestStaggerSessionDurationDeterministic(t *testing.T) {
	t.Setenv("NODE_ID", "node-7")
	t.Setenv("GITHUB_RUN_ID", "")
	base := 320 * time.Minute
	a := staggerSessionDuration(base)
	b := staggerSessionDuration(base)
	if a != b {
		t.Fatalf("stagger not deterministic: %s vs %s", a, b)
	}
	if a < base || a >= base+permanentSessionStaggerSpread {
		t.Fatalf("staggered %s out of [%s, %s)", a, base, base+permanentSessionStaggerSpread)
	}
}

// TestStaggerSessionDurationSpreadsNodes verifies distinct node IDs get spread
// across the stagger range instead of all landing on the same offset.
func TestStaggerSessionDurationSpreadsNodes(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "")
	base := 320 * time.Minute
	seen := map[time.Duration]bool{}
	for i := 1; i <= 24; i++ {
		t.Setenv("NODE_ID", fmt.Sprintf("node-%d", i))
		seen[staggerSessionDuration(base)-base] = true
	}
	if len(seen) < 12 {
		t.Errorf("expected >=12 distinct offsets across 24 node IDs, got %d", len(seen))
	}
}

// TestStaggerSessionDurationCI verifies CI runners' staggered duration stays
// at or BELOW the base duration. Runners are hard-killed 6h after start no
// matter what the session says, so the offset is subtracted (deadlines spread
// while the shortest-drain node keeps the full kill headroom). The staggered
// value must never cross the 348m self-cancel buffer for the 335m fallback.
func TestStaggerSessionDurationCI(t *testing.T) {
	t.Setenv("NODE_ID", "node-7")
	t.Setenv("GITHUB_RUN_ID", "12345")
	base := 335 * time.Minute
	d := staggerSessionDuration(base)
	if d > base {
		t.Fatalf("CI staggered duration %s exceeds base %s — subtracting offset must never extend the session toward the hard kill", d, base)
	}
	if d < base-ciSessionStaggerSpread {
		t.Fatalf("CI staggered duration %s below base-spread %s", d, base-ciSessionStaggerSpread)
	}
	if d > 348*time.Minute {
		t.Fatalf("CI staggered duration %s crosses the 348m self-cancel buffer", d)
	}
}

// TestStaggerSessionDurationCISpreadsDeadlines is the regression test for the
// fleet-wide migration wave: with the old 10-minute ADD spread, all 18 CI
// deadlines fit inside the single 15-minute migration window, so every node
// went imminent together and deadline migration had no healthy target
// ("10 node(s) entering migration" in one log line, 126 channel stops). The
// 30-minute SUBTRACT spread must distribute 18 nodes across enough distinct
// offsets that no 15-minute window can contain them all.
func TestStaggerSessionDurationCISpreadsDeadlines(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "12345")
	base := 335 * time.Minute
	seen := map[time.Duration]bool{}
	for i := 1; i <= 18; i++ {
		t.Setenv("NODE_ID", fmt.Sprintf("node-%d", i))
		d := staggerSessionDuration(base)
		if d > base || d < base-ciSessionStaggerSpread {
			t.Fatalf("node-%d staggered %s outside [%s, %s]", i, d, base-ciSessionStaggerSpread, base)
		}
		seen[d] = true
	}
	// A 15-minute migration window covers half the 30-minute spread. With
	// offsets clustered in fewer than ~6 distinct values, a single window
	// could still monopolize the fleet; require healthy distribution.
	if len(seen) < 10 {
		t.Errorf("expected >=10 distinct CI deadlines across 18 nodes (30m spread), got %d: %v", len(seen), seen)
	}
}

// TestStaggerSessionDurationCIHeadroomNeverShrinks locks the kill-safety
// invariant across the whole fleet: for every node ID and every plausible
// base duration, the staggered duration must not exceed the base, so the
// graceful-drain window before GitHub's 6h hard kill only ever GROWS.
func TestStaggerSessionDurationCIHeadroomNeverShrinks(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "12345")
	for _, base := range []time.Duration{335 * time.Minute, 320 * time.Minute, 300 * time.Minute} {
		for i := 1; i <= 24; i++ {
			t.Setenv("NODE_ID", fmt.Sprintf("node-%d", i))
			if d := staggerSessionDuration(base); d > base {
				t.Fatalf("base %s: node-%d staggered to %s — kill headroom shrank", base, i, d)
			}
		}
	}
}

// TestStaggerNodeKeyPreferHostnameOverRepoSameAcrossRunners locks the CI
// clustering edge case: without NODE_ID, detectNodeID() falls back to
// GITHUB_REPOSITORY — identical across every runner of the same repo, so
// hashing it would give the whole fleet the SAME offset (the exact
// synchronized-deadline wave the stagger exists to break).  staggerNodeKey must
// prefer the runner-unique hostname over detectNodeID(), and NODE_ID over both.
func TestStaggerNodeKeyPreferHostnameOverRepoSameAcrossRunners(t *testing.T) {
	host, _ := os.Hostname()

	t.Setenv("GITHUB_REPOSITORY", "owner/chaturbate-dvr")
	t.Setenv("NODE_ID", "")
	if key := staggerNodeKey(); key != host {
		t.Errorf("without NODE_ID, staggerNodeKey = %q, want hostname %q (GITHUB_REPOSITORY is identical fleet-wide and must never drive the offset)", key, host)
	}

	t.Setenv("NODE_ID", "node-42")
	if key := staggerNodeKey(); key != "node-42" {
		t.Errorf("with NODE_ID set, staggerNodeKey = %q, want NODE_ID", key)
	}
}

// TestStaggerSessionDurationCIWithoutNodeID verifies the CI hours: a runner
// with no NODE_ID (hostname-keyed offset) still stays inside [d-spread, d] and
// is deterministic across calls — the hostname is stable for a given runner, so
// the persisted deadline does not jitter.
func TestStaggerSessionDurationCIWithoutNodeID(t *testing.T) {
	t.Setenv("NODE_ID", "")
	t.Setenv("GITHUB_RUN_ID", "12345")
	base := 335 * time.Minute
	a := staggerSessionDuration(base)
	b := staggerSessionDuration(base)
	if a != b {
		t.Fatalf("hostname-keyed stagger not deterministic: %s vs %s", a, b)
	}
	if a > base || a < base-ciSessionStaggerSpread {
		t.Fatalf("hostname-keyed CI duration %s outside [%s, %s]", a, base-ciSessionStaggerSpread, base)
	}
}

// TestApplyCentralSessionDurationStaggers verifies the resolved duration (from
// env, central, or CI fallback) gets staggered and the string field stays
// consistent with the parsed value.
func TestApplyCentralSessionDurationStaggers(t *testing.T) {
	t.Setenv("NODE_ID", "node-3")
	t.Setenv("GITHUB_RUN_ID", "")
	oldConfig := Config
	defer func() { Config = oldConfig }()
	Config = &entity.Config{SessionDuration: "5h20m", SessionDurationParsed: 320 * time.Minute}

	ApplyCentralSessionDuration()

	if Config.SessionDurationParsed <= 320*time.Minute {
		t.Fatalf("expected staggered duration > 5h20m, got %s", Config.SessionDurationParsed)
	}
	if Config.SessionDurationParsed >= 320*time.Minute+permanentSessionStaggerSpread {
		t.Fatalf("staggered duration %s exceeds base+spread", Config.SessionDurationParsed)
	}
	if got, _ := time.ParseDuration(Config.SessionDuration); got != Config.SessionDurationParsed {
		t.Fatalf("SessionDuration string %q != parsed %s", Config.SessionDuration, Config.SessionDurationParsed)
	}
}

// TestApplyCentralSessionDurationContinuous verifies a node with no session
// duration stays continuous (0) — never staggered into a deadline.
func TestApplyCentralSessionDurationContinuous(t *testing.T) {
	t.Setenv("NODE_ID", "node-3")
	t.Setenv("GITHUB_RUN_ID", "")
	oldConfig := Config
	defer func() { Config = oldConfig }()
	Config = &entity.Config{}

	ApplyCentralSessionDuration()

	if Config.SessionDurationParsed != 0 {
		t.Fatalf("no-duration node got %s, want 0 (continuous)", Config.SessionDurationParsed)
	}
}

// TestLoadSettingsLocalCredentialsWinOverCentral is the regression test for
// secret rotation: the fleet previously let the central dvr_settings blob win
// unconditionally, so rotated .env/GitHub-secret credentials were clobbered at
// startup and SaveSettings re-persisted the stale keys forever (Streamtape
// 403 "Authentication failed" across the fleet). A non-empty local value must
// win; the central blob only seeds empty local fields.
func TestLoadSettingsLocalCredentialsWinOverCentral(t *testing.T) {
	ts := httptest.NewServer(newFakeSupabase().handler())
	defer ts.Close()

	oldID := os.Getenv("NODE_ID")
	os.Setenv("NODE_ID", "node-70")
	defer os.Setenv("NODE_ID", oldID)

	oldConfig := Config
	defer func() { Config = oldConfig }()

	// Central blob holds the OLD (stale) rotated-away credentials.
	seed := []byte(`{"voesx_api_key":"old-voe","streamtape_login":"old-st-login","streamtape_key":"old-st-key","vidara_key":"old-vidara","affiliate_wm":"wm-1"}`)
	Config = &entity.Config{SupabaseURL: ts.URL, SupabaseAPIKey: "test-key"}
	if err := SaveSettingsToDBForKey("dvr_settings", seed); err != nil {
		t.Fatalf("seed central blob: %v", err)
	}

	// Local config (env/flag path) holds the NEW rotated credentials. Vidara is
	// intentionally left empty locally — the central value must seed it.
	Config.VoeSXAPIKey = "new-voe"
	Config.StreamtapeLogin = "new-st-login"
	Config.StreamtapeKey = "new-st-key"

	if err := LoadSettings(); err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if Config.VoeSXAPIKey != "new-voe" {
		t.Errorf("voesx = %q, want new-voe (local must win)", Config.VoeSXAPIKey)
	}
	if Config.StreamtapeLogin != "new-st-login" {
		t.Errorf("streamtape_login = %q, want new-st-login (local must win)", Config.StreamtapeLogin)
	}
	if Config.StreamtapeKey != "new-st-key" {
		t.Errorf("streamtape_key = %q, want new-st-key (local must win)", Config.StreamtapeKey)
	}
	if Config.VidaraKey != "old-vidara" {
		t.Errorf("vidara = %q, want old-vidara (central seeds empty local field)", Config.VidaraKey)
	}

	// SaveSettings must converge the fleet onto the new values.
	if err := SaveSettings(); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	var g persistedSettings
	if err := json.Unmarshal(LoadSettingsFromDB(), &g); err != nil {
		t.Fatalf("unmarshal global blob: %v", err)
	}
	if g.StreamtapeKey != "new-st-key" || g.VoeSXAPIKey != "new-voe" {
		t.Errorf("after save central blob = st:%q voe:%q, want rotated values", g.StreamtapeKey, g.VoeSXAPIKey)
	}

	// A node with NO local values must still pick up the (now rotated) central ones.
	Config = &entity.Config{SupabaseURL: ts.URL, SupabaseAPIKey: "test-key"}
	if err := LoadSettings(); err != nil {
		t.Fatalf("LoadSettings (no local): %v", err)
	}
	if Config.StreamtapeKey != "new-st-key" || Config.VidaraKey != "old-vidara" {
		t.Errorf("seed-only node = st:%q vidara:%q, want central values", Config.StreamtapeKey, Config.VidaraKey)
	}
}

// TestSyncNodeEnvironmentAfterDotenv proves NodeID()/CookieSettingsKey() pick
// up NODE_ID even though package init() runs before .env is loaded.
func TestSyncNodeEnvironmentAfterEnv(t *testing.T) {
	oldID := os.Getenv("NODE_ID")
	oldRepo := os.Getenv("GITHUB_REPOSITORY")

	os.Setenv("NODE_ID", "")
	os.Setenv("GITHUB_REPOSITORY", "lawdachuss/node-12")
	syncNodeEnvironment()
	if got := NodeID(); got != "12" {
		t.Errorf("NodeID after .env-sync = %q, want 12", got)
	}
	if got := CookieSettingsKey(); got != "dvr_settings:12" {
		t.Errorf("CookieSettingsKey = %q, want dvr_settings:12", got)
	}

	os.Setenv("NODE_ID", oldID)
	os.Setenv("GITHUB_REPOSITORY", oldRepo)
	syncNodeEnvironment()
}
