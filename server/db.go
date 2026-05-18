package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

// InitDB initialises the PostgreSQL connection using DATABASE_URL (from env,
// not Config — it is not a CLI flag). Falls back to Supabase REST API if
// DATABASE_URL is not set but SUPABASE_URL/SUPABASE_API_KEY are. Falls back
// to local files if neither direct DB nor Supabase REST is available.
func InitDB() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if supabaseRestURL() != "" && supabaseRestAPIKey() != "" {
			fmt.Println(" INFO [db] DATABASE_URL not set — using Supabase REST API as fallback")
			// Verify the app_settings table is accessible via REST
			if err := checkAppSettingsTable(); err != nil {
				fmt.Printf(" WARN [db] app_settings table not reachable via REST API: %v\n", err)
				fmt.Println(" WARN [db] ensure you have run the Supabase SQL migration (see supabase/migrations/)")
			}
		} else {
			fmt.Println(" INFO [db] DATABASE_URL not set — using local files as fallback")
		}
		return nil
	}

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	conn.SetMaxOpenConns(10)
	conn.SetMaxIdleConns(5)
	conn.SetConnMaxLifetime(5 * time.Minute)

	if err := conn.Ping(); err != nil {
		conn.Close()
		return fmt.Errorf("ping db: %w", err)
	}

	db = conn

	// Ensure schema exists
	if err := migrate(db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	fmt.Println(" INFO [db] connected to PostgreSQL — all data will be persisted remotely")
	return nil
}

// migrate creates all required tables if they don't already exist.
func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS channels (
			id SERIAL PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			site TEXT NOT NULL DEFAULT 'chaturbate',
			is_paused BOOLEAN NOT NULL DEFAULT false,
			framerate INTEGER NOT NULL DEFAULT 30,
			resolution INTEGER NOT NULL DEFAULT 1080,
			pattern TEXT NOT NULL DEFAULT '',
			max_duration INTEGER NOT NULL DEFAULT 30,
			max_filesize INTEGER NOT NULL DEFAULT 0,
			compress BOOLEAN NOT NULL DEFAULT false,
			created_at BIGINT NOT NULL DEFAULT 0,
			streamed_at BIGINT,
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS app_settings (
			key TEXT PRIMARY KEY,
			value JSONB NOT NULL DEFAULT '{}',
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS video_uploads (
			id SERIAL PRIMARY KEY,
			streamer_name TEXT NOT NULL,
			filename TEXT,
			gofile_link TEXT,
			turboviplay_link TEXT,
			voesx_link TEXT,
			streamtape_link TEXT,
			byse_link TEXT,
			sendcm_link TEXT,
			thumbnail_link TEXT,
			sprite_link TEXT,
			embed_url TEXT,
			filesize BIGINT,
			room_title TEXT,
			tags JSONB DEFAULT '[]',
			viewers INTEGER DEFAULT 0,
			resolution TEXT,
			framerate INTEGER DEFAULT 0,
			recorded_at TIMESTAMPTZ,
			upload_date TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS preview_links (
			filename TEXT PRIMARY KEY,
			thumbnail_url TEXT,
			sprite_url TEXT,
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// ─── Channels ────────────────────────────────────────────────────────────────

type channelConfig struct {
	IsPaused    bool   `json:"is_paused"`
	Username    string `json:"username"`
	Framerate   int    `json:"framerate"`
	Resolution  int    `json:"resolution"`
	Pattern     string `json:"pattern"`
	MaxDuration int    `json:"max_duration"`
	MaxFilesize int    `json:"max_filesize"`
	Compress    bool   `json:"compress"`
	CreatedAt   int64  `json:"created_at"`
}

// supabaseRestURL returns the Supabase REST API base URL.
func supabaseRestURL() string {
	if Config == nil || Config.SupabaseURL == "" {
		return ""
	}
	return Config.SupabaseURL + "/rest/v1"
}

// supabaseRestAPIKey returns the Supabase anon/key.
func supabaseRestAPIKey() string {
	if Config == nil {
		return ""
	}
	return Config.SupabaseAPIKey
}

// checkAppSettingsTable verifies the app_settings table is reachable via the
// Supabase REST API. Returns nil on success, an error describing the problem
// if the table cannot be queried.
func checkAppSettingsTable() error {
	baseURL := supabaseRestURL()
	apiKey := supabaseRestAPIKey()
	if baseURL == "" || apiKey == "" {
		return fmt.Errorf("Supabase not configured")
	}

	req, err := http.NewRequest("GET",
		baseURL+"/app_settings?key=eq.__healthcheck__&select=key&limit=1", nil)
	if err != nil {
		return fmt.Errorf("create health check request: %w", err)
	}
	req.Header.Set("apikey", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health check request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		// 404 from /rest/v1/app_settings means the table does not exist
		return fmt.Errorf("app_settings table not found (HTTP 404)")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("authentication failed (HTTP %d) — check SUPABASE_API_KEY and RLS policies", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected response (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// saveChannelsViaRestAPI stores the channels JSON blob in the app_settings
// table via the Supabase REST API. Used as a fallback when direct PostgreSQL
// is not available.
func saveChannelsViaRestAPI(data []byte) error {
	baseURL := supabaseRestURL()
	apiKey := supabaseRestAPIKey()
	if baseURL == "" || apiKey == "" {
		return fmt.Errorf("SUPABASE_URL or SUPABASE_API_KEY not set")
	}

	// The value must be a JSON array wrapped as a JSONB value.
	// Supabase REST API expects the raw JSON for a JSONB column.
	var rawJSON json.RawMessage
	if err := json.Unmarshal(data, &rawJSON); err != nil {
		return fmt.Errorf("parse channels json for rest: %w", err)
	}

	body := map[string]interface{}{
		"key":   "dvr_channels",
		"value": rawJSON,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal rest body: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/app_settings", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create rest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Prefer", "resolution=merge-duplicates")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("rest request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rest api returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// loadChannelsViaRestAPI loads the channels JSON blob from the app_settings
// table via the Supabase REST API.
func loadChannelsViaRestAPI() []byte {
	baseURL := supabaseRestURL()
	apiKey := supabaseRestAPIKey()
	if baseURL == "" || apiKey == "" {
		return nil
	}

	req, err := http.NewRequest("GET", baseURL+"/app_settings?key=eq.dvr_channels&select=value", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("apikey", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	// Response is an array like [{"value": [...]}]
	var entries []struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil
	}
	if len(entries) == 0 {
		return nil
	}

	// entries[0].Value contains the channels JSON array as raw JSON bytes
	return []byte(string(entries[0].Value))
}

// SaveChannelsToDB upserts the channel list to PostgreSQL and writes to local file.
func SaveChannelsToDB(data []byte) error {
	var configs []channelConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return fmt.Errorf("parse channels json: %w", err)
	}

	// Always write to local file as fallback
	if localErr := WriteConfFile("channels.json", data); localErr != nil {
		fmt.Printf("[WARN] could not save channels to local file: %v\n", localErr)
	}

	if db == nil {
		// Try Supabase REST API as fallback
		if restErr := saveChannelsViaRestAPI(data); restErr != nil {
			fmt.Printf("[WARN] could not save channels via Supabase REST API: %v\n", restErr)
		}
		return nil
	}

	if len(configs) == 0 {
		_, err := db.Exec(`DELETE FROM channels WHERE id >= 0`)
		return err
	}

	for _, c := range configs {
		_, err := db.Exec(`
			INSERT INTO channels (username, site, is_paused, framerate, resolution, pattern, max_duration, max_filesize, compress, created_at, updated_at)
			VALUES ($1, 'chaturbate', $2, $3, $4, $5, $6, $7, $8, $9, NOW())
			ON CONFLICT (username) DO UPDATE SET
				is_paused    = EXCLUDED.is_paused,
				framerate    = EXCLUDED.framerate,
				resolution   = EXCLUDED.resolution,
				pattern      = EXCLUDED.pattern,
				max_duration = EXCLUDED.max_duration,
				max_filesize = EXCLUDED.max_filesize,
				compress     = EXCLUDED.compress,
				updated_at   = NOW()`,
			c.Username, c.IsPaused, c.Framerate, c.Resolution, c.Pattern,
			c.MaxDuration, c.MaxFilesize, c.Compress, c.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("upsert channel %s: %w", c.Username, err)
		}
	}
	return nil
}

// LoadChannelsFromDB fetches channels from PostgreSQL first,
// then falls back to the local channels.json file.
func LoadChannelsFromDB() []byte {
	// Try Supabase first
	if db != nil {
		rows, err := db.Query(`
			SELECT username, is_paused, framerate, resolution, pattern, max_duration, max_filesize, compress, created_at
			FROM channels ORDER BY created_at ASC`)
		if err == nil {
			defer rows.Close()

			var configs []channelConfig
			for rows.Next() {
				var c channelConfig
				if err := rows.Scan(&c.Username, &c.IsPaused, &c.Framerate, &c.Resolution,
					&c.Pattern, &c.MaxDuration, &c.MaxFilesize, &c.Compress, &c.CreatedAt); err != nil {
					fmt.Printf("[WARN] [db] scan channel: %v\n", err)
					continue
				}
				configs = append(configs, c)
			}
			if err := rows.Err(); err != nil {
				fmt.Printf("[WARN] [db] rows iteration: %v\n", err)
			} else if len(configs) > 0 {
				b, err := json.Marshal(configs)
				if err == nil {
					return b
				}
			}
		}
	}

	// Try Supabase REST API as fallback (when DATABASE_URL is not set)
	if restData := loadChannelsViaRestAPI(); restData != nil {
		return restData
	}

	// Fall back to local file
	if local := ReadConfFile("channels.json"); local != nil {
		return local
	}

	return nil
}

// ─── Settings ────────────────────────────────────────────────────────────────

// SaveSettingsToDB upserts a settings JSON blob into app_settings.
func SaveSettingsToDB(data []byte) error {
	if db == nil {
		// Try Supabase REST API as fallback
		if restErr := saveJSONSettingViaRestAPI("dvr_settings", data); restErr != nil {
			fmt.Printf("[WARN] could not save settings via Supabase REST API: %v\n", restErr)
		}
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO app_settings (key, value, updated_at)
		VALUES ('dvr_settings', $1, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		data,
	)
	if err != nil {
		fmt.Printf("[WARN] [db] save settings: %v\n", err)
	}
	return nil
}

// LoadSettingsFromDB fetches the settings blob from app_settings.
func LoadSettingsFromDB() []byte {
	if db != nil {
		var raw []byte
		err := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'dvr_settings'`).Scan(&raw)
		if err == nil {
			return raw
		}
	}
	// Try Supabase REST API as fallback
	return loadJSONSettingViaRestAPI("dvr_settings")
}

// ─── Recordings ──────────────────────────────────────────────────────────────

type recDBShape struct {
	Version  int                      `json:"version"`
	Channels map[string]*recChanShape `json:"channels"`
}

type recChanShape struct {
	Gender     string          `json:"gender"`
	Recordings []recEntryShape `json:"recordings"`
}

type recEntryShape struct {
	Filename     string            `json:"filename"`
	Timestamp    string            `json:"timestamp"`
	RoomTitle    string            `json:"room_title"`
	Tags         []string          `json:"tags"`
	Viewers      int               `json:"viewers"`
	Resolution   string            `json:"resolution"`
	Framerate    int               `json:"framerate"`
	Links        map[string]string `json:"links"`
	ThumbnailURL string            `json:"thumbnail_url"`
	SpriteURL    string            `json:"sprite_url"`
	EmbedURL     string            `json:"embed_url"`
	Filesize     int64             `json:"filesize"`
}

func saveJSONSettingViaRestAPI(key string, data []byte) error {
	baseURL := supabaseRestURL()
	apiKey := supabaseRestAPIKey()
	if baseURL == "" || apiKey == "" {
		return fmt.Errorf("SUPABASE_URL or SUPABASE_API_KEY not set")
	}

	var rawJSON json.RawMessage
	if err := json.Unmarshal(data, &rawJSON); err != nil {
		return fmt.Errorf("parse json for rest: %w", err)
	}

	body := map[string]interface{}{
		"key":   key,
		"value": rawJSON,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal rest body: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/app_settings", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create rest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Prefer", "resolution=merge-duplicates")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("rest request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rest api returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

func loadJSONSettingViaRestAPI(key string) []byte {
	baseURL := supabaseRestURL()
	apiKey := supabaseRestAPIKey()
	if baseURL == "" || apiKey == "" {
		return nil
	}

	req, err := http.NewRequest("GET", baseURL+"/app_settings?key=eq."+key+"&select=value", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("apikey", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var entries []struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	return []byte(string(entries[0].Value))
}

func SaveRecordingsJSONToDB(data []byte) error {
	if db == nil {
		// Try Supabase REST API as fallback
		if restErr := saveJSONSettingViaRestAPI("recordings_db", data); restErr != nil {
			fmt.Printf("[WARN] could not save recordings via Supabase REST API: %v\n", restErr)
		}
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO app_settings (key, value, updated_at)
		VALUES ('recordings_db', $1, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		data,
	)
	if err != nil {
		return fmt.Errorf("save recordings json: %w", err)
	}
	return nil
}

// SaveRecordingsToDB syncs the recordings JSON blob to PostgreSQL.
func SaveRecordingsToDB(data []byte) error {
	return SaveRecordingsJSONToDB(data)
}

func LoadRecordingsJSONFromDB() []byte {
	if db != nil {
		var raw []byte
		err := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'recordings_db'`).Scan(&raw)
		if err == nil {
			return raw
		}
	}
	// Try Supabase REST API as fallback
	return loadJSONSettingViaRestAPI("recordings_db")
}

// LoadRecordingsFromDB fetches the recordings JSON blob from PostgreSQL.
func LoadRecordingsFromDB() []byte {
	return LoadRecordingsJSONFromDB()
}

// ─── Tunnels ──────────────────────────────────────────────────────────────────

func SaveTunnelToDB(tunnelURL string, runID int) error {
	// Always write to local file as fallback
	tunnelData := map[string]interface{}{
		"url":    tunnelURL,
		"run_id": runID,
	}
	if data, err := json.MarshalIndent(tunnelData, "", "  "); err == nil {
		if err := WriteDataFile("tunnel.json", data); err != nil {
			fmt.Printf("[WARN] could not save tunnel to local file: %v\n", err)
		}
	}

	tunnelJSON := fmt.Sprintf(`"%s"`, tunnelURL)
	if db != nil {
		_, err := db.Exec(`
			INSERT INTO app_settings (key, value, updated_at)
			VALUES ('tunnel_url', $1, NOW())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
			tunnelJSON,
		)
		return err
	}

	// Try Supabase REST API as fallback
	if restErr := saveJSONSettingViaRestAPI("tunnel_url", []byte(tunnelJSON)); restErr != nil {
		fmt.Printf("[WARN] could not save tunnel via Supabase REST API: %v\n", restErr)
	}
	return nil
}

// LoadCurrentTunnel reads the tunnel URL from Supabase first,
// then falls back to the local tunnel.json file.
func LoadCurrentTunnel() (string, error) {
	if db != nil {
		var raw []byte
		err := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'tunnel_url'`).Scan(&raw)
		if err == nil {
			var tunnelURL string
			if err := json.Unmarshal(raw, &tunnelURL); err == nil && tunnelURL != "" {
				return tunnelURL, nil
			}
		}
	}

	// Try Supabase REST API as fallback
	if restData := loadJSONSettingViaRestAPI("tunnel_url"); restData != nil {
		var tunnelURL string
		if err := json.Unmarshal(restData, &tunnelURL); err == nil && tunnelURL != "" {
			return tunnelURL, nil
		}
	}

	// Fall back to local file
	if local := ReadDataFile("tunnel.json"); local != nil {
		var tunnelData map[string]interface{}
		if err := json.Unmarshal(local, &tunnelData); err == nil {
			if url, ok := tunnelData["url"].(string); ok && url != "" {
				return url, nil
			}
		}
	}

	return "", nil
}

// ─── Preview Links ───────────────────────────────────────────────────────────

// SavePreviewLinks stores thumbnail and sprite URLs in the preview_links table.
func SavePreviewLinks(filename, thumbnailURL, spriteURL string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO preview_links (filename, thumbnail_url, sprite_url, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (filename) DO UPDATE SET
			thumbnail_url = EXCLUDED.thumbnail_url,
			sprite_url = EXCLUDED.sprite_url,
			updated_at = NOW()`,
		filename, thumbnailURL, spriteURL,
	)
	if err != nil {
		return fmt.Errorf("save preview links: %w", err)
	}
	return nil
}

// LoadPreviewLinks fetches thumbnail and sprite URLs from the preview_links table.
func LoadPreviewLinks(filename string) (thumbnailURL, spriteURL string) {
	if db == nil {
		return "", ""
	}
	var thumb, sprite []byte
	err := db.QueryRow(`SELECT thumbnail_url, sprite_url FROM preview_links WHERE filename = $1`, filename).Scan(&thumb, &sprite)
	if err != nil {
		return "", ""
	}
	return string(thumb), string(sprite)
}
