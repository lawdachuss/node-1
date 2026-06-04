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
"regexp"
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
        ImgURL  string `json:"img_url"` // Direct image URL (may be empty for NSFW)
}

// NewThumbnailUploader creates a new Pixhost.to thumbnail uploader.
// apiKey parameter is ignored (Pixhost doesn't require API keys)
func NewThumbnailUploader(apiKey string) *ThumbnailUploader {
return &ThumbnailUploader{
apiKey: apiKey,
client: newNoProxyClient(2 * time.Minute),
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

body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10MB limit
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

// For NSFW uploads img_url is always empty. Derive the full-size CDN URL
// from th_url by replacing the thumbnail path with the images path:
//   https://t2.pixhost.to/thumbs/ID/file.jpg
//   → https://img2.pixhost.to/images/ID/file.jpg
// This gives the original full-resolution image without any age-gate.
// th_url is only used as a last resort because it is capped at max_th_size.
imageURL := strings.TrimSpace(result.ImgURL)
if imageURL == "" {
        if th := strings.TrimSpace(result.ThURL); th != "" {
                imageURL = pixhostThumbToFull(th)
        }
}
if imageURL == "" && strings.Contains(result.ShowURL, "/show/") {
        imageURL = strings.Replace(result.ShowURL, "/show/", "/images/", 1)
}
if imageURL == "" {
        return "", fmt.Errorf("Pixhost returned no image URL (response: %s)", string(body))
}
log.Printf("Pixhost response: img_url=%q show_url=%q th_url=%q → using %q",
        result.ImgURL, result.ShowURL, result.ThURL, imageURL)

log.Printf("Thumbnail uploaded to Pixhost: %s", imageURL)
return imageURL, nil
}

// pixhostThToFull re-derives the full-resolution CDN URL from a Pixhost
// thumbnail URL.
//
//      https://t2.pixhost.to/thumbs/8020/file.jpg
//      → https://img2.pixhost.to/images/8020/file.jpg
//
// If the URL doesn't match the expected pattern, it is returned unchanged so
// we always have something to store rather than an empty string.
var pixhostThRe = regexp.MustCompile(`^https?://t(\d+)\.pixhost\.to/thumbs/`)

func pixhostThumbToFull(thURL string) string {
        loc := pixhostThRe.FindStringIndex(thURL)
        if loc == nil {
                return thURL
        }
        // Extract the server number from the match (group 1)
        sub := pixhostThRe.FindStringSubmatch(thURL)
        if len(sub) < 2 {
                return thURL
        }
        n := sub[1]
        full := "https://img" + n + ".pixhost.to/images/" + thURL[loc[1]:]
        return full
}