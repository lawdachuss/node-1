package uploader

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	streamWishAPIBase = "https://api.streamwish.com/api"
)

type StreamWishUploader struct {
	keys         *keyRing
	client       *http.Client
	cachedServer string // cached upload server URL to skip getUploadServer on retries
}

func NewStreamWishUploader(apiKeys []string) *StreamWishUploader {
	return &StreamWishUploader{
		keys:   newKeyRing(apiKeys),
		client: newDirectClient(120 * time.Minute),
	}
}

type streamWishServerResponse struct {
	ServerTime string `json:"server_time"`
	Msg        string `json:"msg"`
	Status     int    `json:"status"`
	Result     string `json:"result"`
}

type streamWishUploadFileEntry struct {
	FileCode string `json:"filecode"`
	Filename string `json:"filename"`
	Status   string `json:"status"`
}

type streamWishUploadResponse struct {
	Msg    string                     `json:"msg"`
	Status int                        `json:"status"`
	Files  []streamWishUploadFileEntry `json:"files"`
}

func (u *StreamWishUploader) Upload(filePath string) (string, error) {
	return u.UploadWithProgress(filePath, nil)
}

// Keys returns the current key ring (for testing/debugging).
func (u *StreamWishUploader) Keys() *keyRing { return u.keys }

func (u *StreamWishUploader) UploadWithProgress(filePath string, progress ProgressFunc) (string, error) {
	if u.keys.count() == 0 {
		return "", fmt.Errorf("StreamWish API key not configured")
	}

	// Try each key at most once; on permanent (quota) error, rotate to next key.
	// For transient errors, retry the same key up to 3 times (standard backoff).
	attempts := u.keys.count()
	maxRetriesPerKey := 3
	var lastErr error

	for ki := 0; ki < attempts; ki++ {
		key := u.keys.current()
		for retry := 1; retry <= maxRetriesPerKey; retry++ {
			if retry > 1 {
				time.Sleep(uploadBackoff(retry-2, lastErr))
			}

			downloadLink, err := u.uploadFile(filePath, progress, key)
			if err != nil {
				lastErr = fmt.Errorf("upload file: %w", err)
				// Permanent error (quota) — rotate to next key
				if IsPermanentError(err) {
					u.keys.rotate()
					break // break inner retry loop, try next key
				}
				if isUploadRateLimited(err) {
					time.Sleep(uploadBackoff(retry, err))
					lastErr = nil
					continue
				}
				if retry < maxRetriesPerKey {
					continue
				}
				// All retries for this key exhausted — try next key
				u.keys.rotate()
				break
			}

			return downloadLink, nil
		}
	}

	return "", lastErr
}

func (u *StreamWishUploader) getUploadServer(apiKey string) (string, error) {
	url := fmt.Sprintf("%s/upload/server?key=%s", streamWishAPIBase, apiKey)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request upload server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get upload server failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var serverResp streamWishServerResponse
	if err := json.NewDecoder(resp.Body).Decode(&serverResp); err != nil {
		return "", fmt.Errorf("decode server response: %w", err)
	}

	if serverResp.Status != 200 {
		return "", fmt.Errorf("server status not ok: %s (result: %s)", serverResp.Msg, serverResp.Result)
	}

	if serverResp.Result == "" {
		return "", fmt.Errorf("no upload server URL in response")
	}

	return serverResp.Result, nil
}

func (u *StreamWishUploader) uploadFile(filePath string, progress ProgressFunc, apiKey string) (string, error) {
	uploadServer := u.cachedServer
	if uploadServer == "" {
		var err error
		uploadServer, err = u.getUploadServer(apiKey)
		if err != nil {
			return "", fmt.Errorf("get upload server: %w", err)
		}
		u.cachedServer = uploadServer
	}

	body, contentLen, contentType, file, err := multipartStreamWithProgress(
		map[string]string{"key": apiKey},
		"file", filePath, "StreamWish", progress,
	)
	if err != nil {
		return "", fmt.Errorf("multipart stream: %w", err)
	}
	defer file.Close()

	req, err := http.NewRequest("POST", uploadServer, body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = contentLen

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	rawBody, _ := io.ReadAll(resp.Body)

	var uploadResp streamWishUploadResponse
	if err := json.Unmarshal(rawBody, &uploadResp); err != nil {
		return "", fmt.Errorf("decode upload response: %w (body: %s)", err, string(rawBody))
	}

	if uploadResp.Status != 200 {
		return "", fmt.Errorf("upload failed: status %d — %s (body: %s)", uploadResp.Status, uploadResp.Msg, string(rawBody))
	}

	if len(uploadResp.Files) == 0 {
		return "", fmt.Errorf("no files in upload response (body: %s)", string(rawBody))
	}

	fileStatus := uploadResp.Files[0].Status
	if fileStatus != "" && !strings.EqualFold(fileStatus, "ok") {
		errMsg := fmt.Errorf("upload rejected: file status %q (body: %s)", fileStatus, string(rawBody))
		// "too many files" is a daily quota limit — retrying is futile
		if strings.Contains(strings.ToLower(fileStatus), "too many") {
			return "", &permanentError{err: errMsg}
		}
		return "", errMsg
	}

	fileCode := uploadResp.Files[0].FileCode
	if fileCode == "" {
		var fallback struct {
			Files []struct {
				FileCode string `json:"file_code"`
			} `json:"files"`
		}
		if err := json.Unmarshal(rawBody, &fallback); err == nil && len(fallback.Files) > 0 && fallback.Files[0].FileCode != "" {
			fileCode = fallback.Files[0].FileCode
		}
	}
	if fileCode == "" {
		var fallback struct {
			Result string `json:"result"`
		}
		if err := json.Unmarshal(rawBody, &fallback); err == nil && fallback.Result != "" && !strings.HasPrefix(fallback.Result, "http") {
			fileCode = fallback.Result
		}
	}
	if fileCode == "" {
		return "", fmt.Errorf("no file code in response (body: %s)", string(rawBody))
	}

	viewURL := fmt.Sprintf("https://masukestin.com/%s", fileCode)
	return viewURL, nil
}
