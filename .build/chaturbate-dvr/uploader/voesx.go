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
	voeSXAPIBase = "https://voe.sx/api"
)

// VoeSXUploader handles uploading files to VOE.sx
type VoeSXUploader struct {
	apiKey string
	client *http.Client
}

// NewVoeSXUploader creates a new VOE.sx uploader instance
func NewVoeSXUploader(apiKey string) *VoeSXUploader {
	return &VoeSXUploader{
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

type voeSXServerResponse struct {
	ServerTime string `json:"server_time"`
	Msg        string `json:"msg"`
	Message    string `json:"message"`
	Status     int    `json:"status"`
	Success    bool   `json:"success"`
	Result     string `json:"result"`
}

type voeSXUploadResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	File    struct {
		ID                 int    `json:"id"`
		FileCode           string `json:"file_code"`
		FileTitle          string `json:"file_title"`
		EncodingNecessary  bool   `json:"encoding_necessary"`
	} `json:"file"`
}

// Upload uploads a file to VOE.sx and returns the view link
func (u *VoeSXUploader) Upload(filePath string) (string, error) {
	if u.apiKey == "" {
		return "", fmt.Errorf("VOE.sx API key not configured")
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

// getUploadServer gets the upload server URL from VOE.sx API
func (u *VoeSXUploader) getUploadServer() (string, error) {
	url := fmt.Sprintf("%s/upload/server?key=%s", voeSXAPIBase, u.apiKey)
	
	resp, err := u.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("request upload server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get upload server failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var serverResp voeSXServerResponse
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		return "", fmt.Errorf("decode server response: %w", err)
	}

	if !serverResp.Success || serverResp.Status != 200 {
		return "", fmt.Errorf("server status not ok: %s (msg: %s)", serverResp.Msg, serverResp.Message)
	}

	if serverResp.Result == "" {
		return "", fmt.Errorf("no upload server URL in response")
	}

	return serverResp.Result, nil
}

func (u *VoeSXUploader) uploadFile(filePath string) (string, error) {
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
	if err := writer.WriteField("key", u.apiKey); err != nil {
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

	var uploadResp voeSXUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}

	if !uploadResp.Success {
		return "", fmt.Errorf("upload failed: %s", uploadResp.Message)
	}

	if uploadResp.File.FileCode == "" {
		return "", fmt.Errorf("no file code in response")
	}

	// Build the view URL from the file code
	// VOE.sx video URL format: https://voe.sx/{file_code}
	viewURL := fmt.Sprintf("https://voe.sx/%s", uploadResp.File.FileCode)
	return viewURL, nil
}
