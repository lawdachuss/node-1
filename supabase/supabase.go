package supabase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client handles interactions with Supabase
type Client struct {
	url        string
	apiKey     string
	httpClient *http.Client
}

// UploadRecord represents a video upload record with links from multiple hosts
type UploadRecord struct {
	ID               int       `json:"id,omitempty"`
	StreamerName     string    `json:"streamer_name"`
	Filename         string    `json:"filename,omitempty"`
	GofileLink       string    `json:"gofile_link,omitempty"`
	TurboViPlayLink  string    `json:"turboviplay_link,omitempty"`
	VoeSXLink        string    `json:"voesx_link,omitempty"`
	StreamtapeLink   string    `json:"streamtape_link,omitempty"`
	ThumbnailLink    string    `json:"thumbnail_link,omitempty"`
	UploadDate       time.Time `json:"upload_date"`
}

// NewClient creates a new Supabase client
func NewClient(url, apiKey string) *Client {
	return &Client{
		url:    url,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// InsertUploadRecord stores a new video upload record with links from multiple hosts in Supabase
func (c *Client) InsertUploadRecord(streamerName, gofileLink, thumbnailLink string) error {
	record := UploadRecord{
		StreamerName:  streamerName,
		GofileLink:    gofileLink,
		ThumbnailLink: thumbnailLink,
		UploadDate:    time.Now(),
	}

	jsonData, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}

	req, err := http.NewRequest("POST", c.url+"/rest/v1/video_uploads", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// InsertMultiHostUploadRecord stores a new video upload record with links from all hosts
func (c *Client) InsertMultiHostUploadRecord(streamerName, filename, gofileLink, turboviplayLink, voesxLink, streamtapeLink, thumbnailLink string) error {
	record := UploadRecord{
		StreamerName:    streamerName,
		Filename:        filename,
		GofileLink:      gofileLink,
		TurboViPlayLink: turboviplayLink,
		VoeSXLink:       voesxLink,
		StreamtapeLink:  streamtapeLink,
		ThumbnailLink:   thumbnailLink,
		UploadDate:      time.Now(),
	}

	jsonData, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}

	req, err := http.NewRequest("POST", c.url+"/rest/v1/video_uploads", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// GetAllUploads retrieves all upload records from Supabase, ordered by upload date (newest first)
func (c *Client) GetAllUploads() ([]UploadRecord, error) {
	req, err := http.NewRequest("GET", c.url+"/rest/v1/video_uploads?order=upload_date.desc", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var records []UploadRecord
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return records, nil
}

// GetUploadsByStreamer retrieves all upload records for a specific streamer
func (c *Client) GetUploadsByStreamer(streamerName string) ([]UploadRecord, error) {
	url := fmt.Sprintf("%s/rest/v1/video_uploads?streamer_name=eq.%s&order=upload_date.desc", c.url, streamerName)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var records []UploadRecord
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return records, nil
}

// UploadThumbnail is deprecated - thumbnails are now uploaded to ImgBB instead of Supabase Storage
// This function is kept for backward compatibility but should not be used
func (c *Client) UploadThumbnail(bucket, objectPath, localPath, contentType string) (string, error) {
	return "", fmt.Errorf("UploadThumbnail is deprecated - use uploader.ThumbnailUploader (ImgBB) instead")
}

// ChannelConfig represents a channel configuration stored in Supabase
type ChannelConfig struct {
	ID         int    `json:"id,omitempty"`
	Username   string `json:"username"`
	Site       string `json:"site"`
	IsPaused   bool   `json:"is_paused"`
	Framerate  int    `json:"framerate"`
	Resolution int    `json:"resolution"`
	Pattern    string `json:"pattern"`
	MaxDuration int   `json:"max_duration"`
	MaxFilesize int   `json:"max_filesize"`
	CreatedAt  int64  `json:"created_at"`
	StreamedAt int64  `json:"streamed_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// GetAllChannels retrieves all channel configurations from Supabase
func (c *Client) GetAllChannels() ([]ChannelConfig, error) {
	req, err := http.NewRequest("GET", c.url+"/rest/v1/channels?order=username.asc", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var channels []ChannelConfig
	if err := json.NewDecoder(resp.Body).Decode(&channels); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return channels, nil
}

// GetChannelByUsername retrieves a specific channel by username
func (c *Client) GetChannelByUsername(username string) (*ChannelConfig, error) {
	url := fmt.Sprintf("%s/rest/v1/channels?username=eq.%s", c.url, username)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var channels []ChannelConfig
	if err := json.NewDecoder(resp.Body).Decode(&channels); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(channels) == 0 {
		return nil, fmt.Errorf("channel not found: %s", username)
	}

	return &channels[0], nil
}

// InsertChannel creates a new channel configuration in Supabase
func (c *Client) InsertChannel(channel ChannelConfig) error {
	jsonData, err := json.Marshal(channel)
	if err != nil {
		return fmt.Errorf("marshal channel: %w", err)
	}

	req, err := http.NewRequest("POST", c.url+"/rest/v1/channels", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// UpdateChannel updates an existing channel configuration in Supabase
func (c *Client) UpdateChannel(username string, channel ChannelConfig) error {
	jsonData, err := json.Marshal(channel)
	if err != nil {
		return fmt.Errorf("marshal channel: %w", err)
	}

	url := fmt.Sprintf("%s/rest/v1/channels?username=eq.%s", c.url, username)
	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// DeleteChannel removes a channel configuration from Supabase
func (c *Client) DeleteChannel(username string) error {
	url := fmt.Sprintf("%s/rest/v1/channels?username=eq.%s", c.url, username)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// UpsertChannel inserts or updates a channel configuration in Supabase
func (c *Client) UpsertChannel(channel ChannelConfig) error {
	// Try to get existing channel first
	existing, err := c.GetChannelByUsername(channel.Username)
	
	if err != nil {
		// Channel doesn't exist, insert it
		return c.InsertChannel(channel)
	}
	
	// Channel exists, update it
	return c.UpdateChannel(existing.Username, channel)
}
