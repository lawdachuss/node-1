package uploader

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	turboViPlayAPIBase = "https://api.turboviplay.com"
)

// TurboViPlayUploader handles uploading files to TurboViPlay.com
type TurboViPlayUploader struct {
	apiKey string
	client *http.Client
}

// NewTurboViPlayUploader creates a new TurboViPlay uploader instance
func NewTurboViPlayUploader(apiKey string) *TurboViPlayUploader {
	return &TurboViPlayUploader{
		apiKey: apiKey,
		client: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // Skip SSL verification
			},
		},
	}
}

type turboViPlayServerResponse struct {
	Msg        string `json:"msg"`
	ServerTime string `json:"server_time"`
	Status     int    `json:"status"`
	Result     string `json:"result"`
}

type turboViPlayUploadResponse struct {
	VideoID interface{} `json:"videoID"` // Changed to interface{} to handle both string and object
	Title   string      `json:"title"`
}

// Upload uploads a file to TurboViPlay and returns the view link
func (u *TurboViPlayUploader) Upload(filePath string) (string, error) {
	if u.apiKey == "" {
		return "", fmt.Errorf("TurboViPlay API key not configured")
	}

	var lastErr error
	
	// Retry with exponential backoff
	maxAttempts := 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration((1<<uint(attempt-2))*5) * time.Second
			time.Sleep(backoff)
		}
		
		downloadLink, err := u.uploadFile(filePath)
		if err != nil {
			lastErr = fmt.Errorf("upload file: %w", err)
			if attempt < maxAttempts {
				continue
			}
			return "", lastErr
		}
		
		return downloadLink, nil
	}
	
	return "", lastErr
}

// getUploadServer gets the upload server URL from TurboViPlay API
func (u *TurboViPlayUploader) getUploadServer() (string, error) {
	url := fmt.Sprintf("%s/uploadserver?keyApi=%s", turboViPlayAPIBase, u.apiKey)
	
	resp, err := u.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("request upload server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get upload server failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var serverResp turboViPlayServerResponse
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		return "", fmt.Errorf("decode server response: %w", err)
	}

	if serverResp.Status != 200 || serverResp.Msg != "ok" {
		return "", fmt.Errorf("server status not ok: %s (status: %d)", serverResp.Msg, serverResp.Status)
	}

	return serverResp.Result, nil
}

func (u *TurboViPlayUploader) uploadFile(filePath string) (string, error) {
	// Step 1: Get upload server
	uploadServer, err := u.getUploadServer()
	if err != nil {
		return "", fmt.Errorf("get upload server: %w", err)
	}

	// Step 2: Upload file to the server
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	// Create multipart form
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add API key
	if err := writer.WriteField("keyapi", u.apiKey); err != nil {
		return "", fmt.Errorf("write api key field: %w", err)
	}

	// Add file
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}

	// Copy file content
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("copy file: %w", err)
	}

	// Close writer to finalize multipart form
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close writer: %w", err)
	}

	// Create request
	req, err := http.NewRequest("POST", uploadServer, body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Send request
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var uploadResp turboViPlayUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}

	// Extract slug from videoID (can be string or object)
	var slug string
	switch v := uploadResp.VideoID.(type) {
	case string:
		slug = v
	case map[string]interface{}:
		if s, ok := v["slug"].(string); ok {
			slug = s
		}
	}
	
	if slug == "" {
		return "", fmt.Errorf("no slug in response")
	}

	// TurboViPlay video URL format: https://emturbovid.com/t/{slug}
	viewURL := fmt.Sprintf("https://emturbovid.com/t/%s", slug)
	return viewURL, nil
}
