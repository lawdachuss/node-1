package uploader

import (
	"bytes"
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
	streamtapeAPIBase = "https://api.streamtape.com"
)

// StreamtapeUploader handles uploading files to Streamtape.com
type StreamtapeUploader struct {
	login  string
	apiKey string
	client *http.Client
}

// NewStreamtapeUploader creates a new Streamtape uploader instance
// login and apiKey can be found in Account Settings -> API Details
func NewStreamtapeUploader(login, apiKey string) *StreamtapeUploader {
	return &StreamtapeUploader{
		login:  login,
		apiKey: apiKey,
		client: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
			},
		},
	}
}

type streamtapeUploadURLResponse struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Result struct {
		URL        string `json:"url"`
		ValidUntil string `json:"valid_until"`
	} `json:"result"`
}

type streamtapeUploadResult struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Result struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		Name string `json:"name"`
		Size string `json:"size"` // Changed from int64 to string
	} `json:"result"`
}

// Upload uploads a file to Streamtape and returns the view link
func (u *StreamtapeUploader) Upload(filePath string) (string, error) {
	if u.login == "" || u.apiKey == "" {
		return "", fmt.Errorf("Streamtape login/API key not configured")
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

// getUploadURL gets the upload URL from Streamtape API
func (u *StreamtapeUploader) getUploadURL() (string, error) {
	url := fmt.Sprintf("%s/file/ul?login=%s&key=%s", streamtapeAPIBase, u.login, u.apiKey)
	
	resp, err := u.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("request upload URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get upload URL failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var urlResp streamtapeUploadURLResponse
	if err := json.NewDecoder(resp.Body).Decode(&urlResp); err != nil {
		return "", fmt.Errorf("decode URL response: %w", err)
	}

	if urlResp.Status != 200 {
		return "", fmt.Errorf("API status not OK: %d - %s", urlResp.Status, urlResp.Msg)
	}

	if urlResp.Result.URL == "" {
		return "", fmt.Errorf("no upload URL in response")
	}

	return urlResp.Result.URL, nil
}

func (u *StreamtapeUploader) uploadFile(filePath string) (string, error) {
	// Step 1: Get upload URL
	uploadURL, err := u.getUploadURL()
	if err != nil {
		return "", fmt.Errorf("get upload URL: %w", err)
	}

	// Step 2: Upload file to the URL
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	// Create multipart form in memory buffer to get exact Content-Length
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Streamtape expects the field name to be "file1"
	part, err := writer.CreateFormFile("file1", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}

	// Copy file content to buffer
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("copy file: %w", err)
	}

	// Close writer to finalize multipart form
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close writer: %w", err)
	}

	// Create request with exact Content-Length
	req, err := http.NewRequest("POST", uploadURL, body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = int64(body.Len()) // Set exact content length

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

	var uploadResp streamtapeUploadResult
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}

	if uploadResp.Status != 200 {
		return "", fmt.Errorf("upload status not OK: %d - %s", uploadResp.Status, uploadResp.Msg)
	}

	if uploadResp.Result.URL == "" {
		return "", fmt.Errorf("no URL in response")
	}

	// Return the video URL
	return uploadResp.Result.URL, nil
}
