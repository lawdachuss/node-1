package uploader

import (
"encoding/json"
"fmt"
"io"
"log"
"mime/multipart"
"net/http"
"os"
"path/filepath"
"strings"
"time"
)

// ThumbnailUploader handles uploading thumbnail images to Pixhost.to
type ThumbnailUploader struct {
apiKey string // Not used for Pixhost, kept for compatibility
client *http.Client
}

// pixhostResponse is the JSON response from the Pixhost.to API
type pixhostResponse struct {
	Name    string `json:"name"`
	ShowURL string `json:"show_url"`
	ThURL   string `json:"th_url"`
	ImgURL  string `json:"img_url"` // Direct image URL
}

// NewThumbnailUploader creates a new Pixhost.to thumbnail uploader.
// apiKey parameter is ignored (Pixhost doesn't require API keys)
func NewThumbnailUploader(apiKey string) *ThumbnailUploader {
return &ThumbnailUploader{
apiKey: apiKey,
client: &http.Client{
Timeout: 2 * time.Minute,
},
}
}

// Upload uploads a thumbnail image to Pixhost.to and returns the direct image URL.
func (t *ThumbnailUploader) Upload(thumbnailPath string) (string, error) {
log.Printf("Uploading thumbnail to Pixhost.to: %s", thumbnailPath)

file, err := os.Open(thumbnailPath)
if err != nil {
return "", fmt.Errorf("open file: %w", err)
}
defer file.Close()

// Build multipart form via pipe to avoid buffering the whole image in memory
pr, pw := io.Pipe()
writer := multipart.NewWriter(pw)

errCh := make(chan error, 1)
go func() {
defer pw.Close()
// Pixhost expects field name "img"
part, err := writer.CreateFormFile("img", filepath.Base(thumbnailPath))
if err != nil {
errCh <- fmt.Errorf("create form file: %w", err)
writer.Close()
return
}
if _, err := io.Copy(part, file); err != nil {
errCh <- fmt.Errorf("copy file: %w", err)
writer.Close()
return
}

// Set content_type: 0 for SFW, 1 for NSFW
// Using 1 (NSFW) since this is for adult content
if err := writer.WriteField("content_type", "1"); err != nil {
errCh <- fmt.Errorf("write content_type field: %w", err)
writer.Close()
return
}

// Optional: set thumbnail size (150-500, default 200)
if err := writer.WriteField("max_th_size", "420"); err != nil {
errCh <- fmt.Errorf("write max_th_size field: %w", err)
writer.Close()
return
}

errCh <- writer.Close()
}()

// Pixhost API endpoint
uploadURL := "https://api.pixhost.to/images"
req, err := http.NewRequest("POST", uploadURL, pr)
if err != nil {
return "", fmt.Errorf("create request: %w", err)
}
req.Header.Set("Content-Type", writer.FormDataContentType())
req.Header.Set("Accept", "application/json")

resp, err := t.client.Do(req)
if err != nil {
	pr.CloseWithError(err) // unblock the writer goroutine
	<-errCh               // drain to avoid goroutine leak
	return "", fmt.Errorf("send request: %w", err)
}
defer resp.Body.Close()

// Wait for the writer goroutine to finish
if werr := <-errCh; werr != nil {
	return "", werr
}

body, err := io.ReadAll(resp.Body)
if err != nil {
return "", fmt.Errorf("read response: %w", err)
}

if resp.StatusCode != http.StatusOK {
return "", fmt.Errorf("Pixhost returned status %d: %s", resp.StatusCode, string(body))
}

var result pixhostResponse
if err := json.Unmarshal(body, &result); err != nil {
return "", fmt.Errorf("decode response: %w", err)
}

// Prefer direct image URL (img_url), fall back to thumbnail URL (th_url)
// Do NOT use show_url as it's an HTML page, not an image
imageURL := strings.TrimSpace(result.ImgURL)
if imageURL == "" {
imageURL = strings.TrimSpace(result.ThURL)
}
if imageURL == "" {
return "", fmt.Errorf("Pixhost returned no image URL (response: %s)", string(body))
}

log.Printf("Thumbnail uploaded to Pixhost: %s", imageURL)
return imageURL, nil
}