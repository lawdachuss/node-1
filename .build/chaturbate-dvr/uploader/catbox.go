package uploader

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const catboxURL = "https://catbox.moe/user/api.php"

const (
	maxUploadAttempts = 3
	uploadBaseBackoff = 2 * time.Second
)

// CatboxUploader uploads files to catbox.moe (no API key required, files never expire).
type CatboxUploader struct {
	client *http.Client
}

// NewCatboxUploader creates a new Catbox uploader.
func NewCatboxUploader() *CatboxUploader {
	return &CatboxUploader{
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Upload uploads a file to Catbox.moe and returns the direct URL.
// Retries up to maxUploadAttempts with exponential backoff.
func (c *CatboxUploader) Upload(filePath string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxUploadAttempts; attempt++ {
		if attempt > 1 {
			backoff := uploadBaseBackoff * (1 << (attempt - 2))
			time.Sleep(backoff)
		}

		url, err := c.uploadOnce(filePath)
		if err == nil {
			return url, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("catbox: all %d attempts failed; last error: %w", maxUploadAttempts, lastErr)
}

func (c *CatboxUploader) uploadOnce(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("catbox: open file: %w", err)
	}
	defer file.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("reqtype", "fileupload"); err != nil {
		return "", fmt.Errorf("catbox: write reqtype: %w", err)
	}
	part, err := w.CreateFormFile("fileToUpload", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("catbox: create form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("catbox: copy file: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("catbox: close writer: %w", err)
	}

	resp, err := c.client.Post(catboxURL, w.FormDataContentType(), &buf)
	if err != nil {
		return "", fmt.Errorf("catbox: post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("catbox: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("catbox: status %d: %s", resp.StatusCode, string(body))
	}

	url := string(bytes.TrimSpace(body))
	if url == "" {
		return "", fmt.Errorf("catbox: empty response")
	}
	return url, nil
}
