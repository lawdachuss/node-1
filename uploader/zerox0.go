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

// ZeroX0Uploader uploads files to 0x0.st (no account required, permanent hosting).
type ZeroX0Uploader struct {
	client *http.Client
}

// NewZeroX0Uploader creates a new 0x0.st uploader.
func NewZeroX0Uploader() *ZeroX0Uploader {
	return &ZeroX0Uploader{
		client: newNoProxyClient(60 * time.Second),
	}
}

// Upload uploads a file to 0x0.st and returns the direct URL.
func (u *ZeroX0Uploader) Upload(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("0x0: open file: %w", err)
	}
	defer file.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("0x0: create form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("0x0: copy file: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("0x0: close writer: %w", err)
	}

	resp, err := u.client.Post("https://0x0.st", w.FormDataContentType(), &buf)
	if err != nil {
		return "", fmt.Errorf("0x0: post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("0x0: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("0x0: status %d: %s", resp.StatusCode, string(body))
	}

	url := string(bytes.TrimSpace(body))
	if url == "" {
		return "", fmt.Errorf("0x0: empty response")
	}
	return url, nil
}
