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

const freeimageAPIURL = "https://freeimage.host/api/1/upload"
const freeimageAPIKey = "6d207e02198a847aa98d0a2a901485a5"

type freeimageResponse struct {
	StatusCode int `json:"status_code"`
	Image      struct {
		URL string `json:"url"`
	} `json:"image"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// FreeimageUploader uploads images to freeimage.host (free, no account, permanent).
type FreeimageUploader struct {
	client *http.Client
}

// NewFreeimageUploader creates a new freeimage.host uploader.
func NewFreeimageUploader() *FreeimageUploader {
	return &FreeimageUploader{
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Upload uploads a file to freeimage.host and returns the direct image URL.
func (u *FreeimageUploader) Upload(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("freeimage: open file: %w", err)
	}
	defer file.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if err := w.WriteField("key", freeimageAPIKey); err != nil {
		return "", fmt.Errorf("freeimage: write key: %w", err)
	}
	if err := w.WriteField("action", "upload"); err != nil {
		return "", fmt.Errorf("freeimage: write action: %w", err)
	}
	if err := w.WriteField("format", "json"); err != nil {
		return "", fmt.Errorf("freeimage: write format: %w", err)
	}

	part, err := w.CreateFormFile("source", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("freeimage: create form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("freeimage: copy file: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("freeimage: close writer: %w", err)
	}

	resp, err := u.client.Post(freeimageAPIURL, w.FormDataContentType(), &buf)
	if err != nil {
		return "", fmt.Errorf("freeimage: post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("freeimage: read response: %w", err)
	}

	var result freeimageResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("freeimage: parse response: %w", err)
	}

	if result.StatusCode != 200 {
		return "", fmt.Errorf("freeimage: error: %s", result.Error.Message)
	}

	if result.Image.URL == "" {
		return "", fmt.Errorf("freeimage: empty image URL in response")
	}

	return result.Image.URL, nil
}
