// Package recovery extracts a thumbnail for a recording whose local video file
// is no longer on disk, by pulling a recoverable asset from one of the upload
// hosts that still holds it.
//
// Two sources are supported:
//   - Streamtape: the official API returns a Range-capable download ticket, so we
//     grab only the first few MB of the (moov-at-front, faststart-remuxed) file,
//     render a frame with ffmpeg, and return that local JPEG. No need to pull the
//     whole file.
//   - Vidara: the host renders its own per-video thumbnail and exposes it as the
//     embed page's <meta property="og:image">, which we fetch with a plain HTTP
//     GET and return as an image path.
//
// This package deliberately does NOT talk to Supabase or the image uploaders;
// callers (the standalone backfill commands and the automated thumbnail sweep)
// upload the returned image and persist the resulting URLs themselves. Keeping
// that boundary free of DB/uploader imports avoids an import cycle while letting
// every caller share the same fetch logic.
package recovery

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/config"
)

const (
	thumbWidth   = 1280
	thumbHeight  = 720
	downloadMax  = 512 * 1024 * 1024 // safety cap for a Range response
	browserUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
	downloadMaxB = 600 * time.Second
)

// streamAPI is the Streamtape API base URL. A var (not const) so tests can point
// it at an httptest server.
var streamAPI = "https://api.streamtape.com"

// cloudflareIPs are known-good Anycast addresses for api.streamtape.com.
// Some ISP resolvers return a stale origin IP that no longer responds; the
// Cloudflare-fronted addresses are the durable path.
var cloudflareIPs = []string{"104.21.96.46", "172.67.173.3"}

// ogImageRe matches <meta property="og:image" content="..."/> on Vidara's
// embed page. The thumbnail is video-specific (pointing at the CDN path).
var ogImageRe = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:image["'][^>]+content=["']([^"']+)["']`)

// errStreamFileNotFound is returned when Streamtape's dl endpoint reports that
// the file has gone / is not downloadable under the configured account. It is a
// permanent condition — retrying only wastes time — so the download loop treats
// it as a hard stop (fail fast) rather than re-issuing tickets and range grabs
// that can never succeed.
var errStreamFileNotFound = errors.New("streamtape: file not found")

// ExtractStreamtapeCode returns the filecode from a Streamtape embed/share URL,
// or "" if it cannot be identified. Accepts e/ and v/ style URLs as well as the
// bare host path and query-stripped codes.
func ExtractStreamtapeCode(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.TrimSpace(u)
	if len(u) < 6 {
		return ""
	}
	for _, r := range u {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return u
}

// VidaraCode extracts the trailing file code from a Vidara share/embed URL.
func VidaraCode(u string) string {
	return ExtractStreamtapeCode(u)
}

// StreamtapeThumb downloads only the head of a Streamtape-hosted video and
// renders a 1280x720 JPEG frame from it into the given work directory. Returns
// the local path of the rendered JPEG. login/key are the Streamtape API
// credentials. partialMB is how many MB to pull from the head (moov-at-front);
// tailMB (>0) enables a tail-range fallback for moov-at-end files; seek is the
// seconds offset for the frame.
func StreamtapeThumb(filecode, workDir string, login, key string, partialMB int, seek float64, tailMB int) (string, error) {
	if partialMB <= 0 {
		partialMB = 4
	}
	if seek < 0 {
		seek = 0
	}
	safe := sanitizeName(filecode)
	headPath := filepath.Join(workDir, "head_"+safe+"_"+fmt.Sprintf("%d", time.Now().UnixNano())+".mp4")
	tailPath := filepath.Join(workDir, "tail_"+safe+".mp4")
	thumbPath := filepath.Join(workDir, "thumb_"+safe+".jpg")

	headBytes := int64(partialMB) * 1024 * 1024
	if err := streamGrab(filecode, headPath, 0, headBytes, login, key); err != nil {
		return "", fmt.Errorf("download head: %w", err)
	}

	videoPath := headPath
	if err := extractThumb(videoPath, thumbPath, seek); err != nil {
		// Fall back to the tail (moov-at-end or sparse head).
		if tailMB <= 0 {
			return "", fmt.Errorf("frame extraction failed on head (corrupt file?) and tail fallback disabled: %w", err)
		}
		tailBytes := int64(tailMB) * 1024 * 1024
		if err := streamGrabTail(filecode, tailPath, tailBytes, login, key); err != nil {
			return "", fmt.Errorf("download tail: %w", err)
		}
		videoPath = tailPath
		if err := extractThumb(videoPath, thumbPath, seek); err != nil {
			return "", fmt.Errorf("frame extraction failed on head and tail (corrupt file?): %w", err)
		}
	}
	if _, err := os.Stat(thumbPath); err != nil {
		return "", fmt.Errorf("thumbnail file not produced: %w", err)
	}
	if fi, err := os.Stat(thumbPath); err == nil && fi.Size() < 5000 {
		return "", fmt.Errorf("thumbnail too small (%d bytes)", fi.Size())
	}
	// Clean up the partial video downloads so they never accumulate.
	_ = os.Remove(headPath)
	_ = os.Remove(tailPath)
	return thumbPath, nil
}

// ResolveVidaraImageURL fetches the Vidara embed page for a file code and
// returns the video-specific og:image thumbnail URL, or ("", nil) if the page
// has no og:image. client may be nil for a default HTTP client.
func ResolveVidaraImageURL(code string, client *http.Client) (string, error) {
	if code == "" {
		return "", fmt.Errorf("bad Vidara file code")
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	pageURL := "https://vidarae.live/e/" + code
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("page status %d", resp.StatusCode)
	}
	m := ogImageRe.FindSubmatch(body)
	if len(m) < 2 || len(m[1]) == 0 {
		return "", nil
	}
	u := string(m[1])
	u = strings.TrimPrefix(u, "//")
	if !strings.HasPrefix(u, "http") {
		u = "https://" + u
	}
	return u, nil
}

// VidaraThumb resolves the video-specific thumbnail for a Vidara embed URL by
// scraping its og:image, downloads it, and returns the local image path. If the
// page has no og:image it returns ("", nil).
func VidaraThumb(embedURL, workDir string, client *http.Client) (string, error) {
	code := VidaraCode(embedURL)
	if code == "" {
		return "", fmt.Errorf("bad Vidara URL %q", embedURL)
	}
	u, err := ResolveVidaraImageURL(code, client)
	if err != nil {
		return "", err
	}
	if u == "" {
		return "", nil
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	// Download the thumbnail.
	dreq, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	dreq.Header.Set("User-Agent", browserUA)
	dreq.Header.Set("Accept", "image/webp,image/apng,image/*,*/*;q=0.8")
	dresp, err := client.Do(dreq)
	if err != nil {
		return "", err
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download status %d", dresp.StatusCode)
	}
	ext := ".jpg"
	ct := dresp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "webp") {
		ext = ".webp"
	}
	dst := filepath.Join(workDir, "vidara_"+sanitizeName(code)+ext)
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, dresp.Body); err != nil {
		f.Close()
		_ = os.Remove(dst)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// streamGrab downloads bytes [start, start+length) of the file to dstPath,
// resuming across fresh download tickets when a CDN link dies mid-transfer.
func streamGrab(filecode, dstPath string, start, length int64, login, key string) error {
	f, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer f.Close()

	client := newHTTPClient(downloadMaxB)
	got := int64(0)
	lastProgress := time.Now()
	for tries := 0; tries < 8; tries++ {
		if got >= length {
			return nil
		}
		dlURL, err := freshDLURL(filecode, login, key)
		if err != nil {
			if errors.Is(err, errStreamFileNotFound) {
				// Permanent — the file is gone; stop immediately.
				return err
			}
			if tries == 7 {
				return err
			}
			time.Sleep(3 * time.Second)
			continue
		}
		req, err := http.NewRequest("GET", dlURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start+got, start+length-1))
		resp, err := client.Do(req)
		if err != nil {
			if tries == 7 {
				return err
			}
			time.Sleep(3 * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			resp.Body.Close()
			if got > 0 {
				return nil
			}
			return fmt.Errorf("file is empty")
		}
		n, copyErr := io.CopyN(f, io.LimitReader(resp.Body, length-got), length-got)
		resp.Body.Close()
		if n > 0 {
			got += n
			lastProgress = time.Now()
		}
		if copyErr == nil && got >= length {
			return nil
		}
		if n == 0 && copyErr == nil {
			return fmt.Errorf("server returned no data for range (file may be smaller than requested)")
		}
		if time.Since(lastProgress) > 140*time.Second && tries == 7 {
			return fmt.Errorf("download stalled after %d bytes", got)
		}
		if tries == 7 {
			return fmt.Errorf("gave up after 8 attempts at offset %d", got)
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

// streamGrabTail downloads the last `length` bytes of the file (moov-at-end
// fallback). The total size is discovered with a 1-byte range request.
func streamGrabTail(filecode, dstPath string, length int64, login, key string) error {
	client := newHTTPClient(60 * time.Second)
	dlURL, err := freshDLURL(filecode, login, key)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", dlURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	var total int64
	if cr := resp.Header.Get("Content-Range"); cr != "" && strings.Contains(cr, "/") {
		totalStr := cr[strings.LastIndex(cr, "/")+1:]
		fmt.Sscanf(totalStr, "%d", &total)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if total <= 0 {
		return fmt.Errorf("could not determine file size")
	}
	return streamGrab(filecode, dstPath, total-length, length, login, key)
}

// freshDLURL performs the dlticket -> (wait) -> dl dance and returns a direct
// download URL for the filecode. The Cloudflare edge intermittently resets the
// TLS handshake, so the whole sequence is retried a few times.
func freshDLURL(filecode, login, key string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		url, err := oneTicket(filecode, login, key)
		if err == nil {
			return url, nil
		}
		if errors.Is(err, errStreamFileNotFound) {
			// Permanent — no point re-issuing tickets.
			return "", err
		}
		lastErr = err
		time.Sleep(3 * time.Second)
	}
	return "", lastErr
}

func oneTicket(filecode, login, key string) (string, error) {
	client := newHTTPClient(40 * time.Second)

	ticketURL := fmt.Sprintf("%s/file/dlticket?file=%s&login=%s&key=%s", streamAPI, filecode, login, key)
	resp, err := client.Get(ticketURL)
	if err != nil {
		return "", fmt.Errorf("dlticket: %w", err)
	}
	var tr struct {
		Status int `json:"status"`
		Result struct {
			Ticket   string `json:"ticket"`
			WaitTime int    `json:"wait_time"`
		} `json:"result"`
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("dlticket decode: %w", err)
	}
	if tr.Status != 200 || tr.Result.Ticket == "" {
		return "", fmt.Errorf("dlticket failed: %s", strings.TrimSpace(string(body)))
	}
	time.Sleep(time.Duration(tr.Result.WaitTime+1) * time.Second)

	dlURL := fmt.Sprintf("%s/file/dl?file=%s&ticket=%s", streamAPI, filecode, tr.Result.Ticket)
	resp, err = client.Get(dlURL)
	if err != nil {
		return "", fmt.Errorf("dl: %w", err)
	}
	var dr struct {
		Status int `json:"status"`
		Result struct {
			URL string `json:"url"`
		} `json:"result"`
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(body, &dr); err != nil {
		return "", fmt.Errorf("dl decode: %w", err)
	}
	if dr.Status == 404 || strings.Contains(strings.ToLower(string(body)), "file not found") {
		// Permanent: the file is gone or not under this account. Do not retry.
		return "", errStreamFileNotFound
	}
	if dr.Status != 200 || dr.Result.URL == "" {
		return "", fmt.Errorf("dl failed: %s", strings.TrimSpace(string(body)))
	}
	return dr.Result.URL, nil
}

// newHTTPClient builds an HTTP client whose transport dials api.streamtape.com
// through the Cloudflare edge (bypassing the ISP resolver's stale origin IP)
// and caps TLS at 1.2 — Cloudflare resets the TLS 1.3 handshake for this host.
func newHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if host == "api.streamtape.com" {
					var lastErr error
					for _, ip := range cloudflareIPs {
						c, e := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip, port))
						if e == nil {
							return c, nil
						}
						lastErr = e
					}
					return nil, fmt.Errorf("all cloudflare IPs unreachable: %w", lastErr)
				}
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12},
		},
	}
}

// extractThumb renders a 1280x720 padded JPEG from an early frame.
func extractThumb(videoPath, thumbPath string, seek float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := []string{
		"-y", "-hide_banner", "-loglevel", "error",
		"-ss", fmt.Sprintf("%g", seek),
		"-i", videoPath,
		"-vf", fmt.Sprintf(
			"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2",
			thumbWidth, thumbHeight, thumbWidth, thumbHeight),
		"-frames:v", "1",
		"-c:v", "mjpeg",
		"-q:v", "5",
		thumbPath,
	}
	if err := config.FFmpegCommandContext(ctx, args...).Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	if fi, err := os.Stat(thumbPath); err != nil || fi.Size() < 5000 {
		return fmt.Errorf("thumbnail too small or missing")
	}
	return nil
}

// sanitizeName makes a string safe for use as a filename component.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
