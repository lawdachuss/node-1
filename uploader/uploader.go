package uploader

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// dialWithTuning returns a DialContext function that:
// - Disables Nagle's algorithm (TCP_NODELAY) for immediate sends
// - Sets larger socket send/receive buffers for higher throughput
func dialWithTuning(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.SetNoDelay(true)
			tcp.SetWriteBuffer(262144)
			tcp.SetReadBuffer(262144)
		}
		return conn, nil
	}
}

// newNoProxyClient returns an http.Client that explicitly bypasses any
// environment-configured proxy (ALL_PROXY / HTTP_PROXY / HTTPS_PROXY).
// The Chaturbate DVR proxy setting is only meant for Chaturbate requests;
// image/thumbnail upload services must reach the public internet directly.
func newNoProxyClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, // never use environment proxy
			DialContext:         dialWithTuning(30 * time.Second),
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:       120 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// newDefaultClient returns an http.Client with proper timeouts and
// SOCKS5 proxy support via ALL_PROXY env var.
// When ALL_PROXY is set (e.g. on GitHub Actions nodes), all uploads
// route through the proxy to avoid datacenter IP blocking.
// When ALL_PROXY is unset, connects directly (local development).
//
// Uses golang.org/x/net/proxy.SOCKS5 at the DialContext level instead of
// http.Transport.Proxy because Go's built-in SOCKS5 proxy handling
// (http.ProxyURL with socks5://) has a known issue where DialContext
// timeouts are ignored for the remote host connection through the proxy
// (golang/go#37549). By handling SOCKS5 at the dial layer, we enforce
// a hard 30s deadline on every dial attempt.
func newDefaultClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           dialWithTuning(30 * time.Second),
	}

	if proxyEnv := os.Getenv("ALL_PROXY"); proxyEnv != "" {
		if proxyURL, err := url.Parse(proxyEnv); err == nil {
			socksDialer, err := proxy.SOCKS5("tcp", proxyURL.Host, nil, proxy.Direct)
			if err == nil {
				if ctxDialer, ok := socksDialer.(proxy.ContextDialer); ok {
					directDialer := &net.Dialer{
						Timeout:   15 * time.Second,
						KeepAlive: 30 * time.Second,
					}
					transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
						dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
						defer cancel()
						conn, err := ctxDialer.DialContext(dialCtx, network, addr)
						if err != nil {
							errStr := err.Error()
							if strings.Contains(errStr, "host unreachable") ||
								strings.Contains(errStr, "connection refused") ||
								strings.Contains(errStr, "general SOCKS server failure") {
								direct, directErr := directDialer.DialContext(ctx, network, addr)
								if directErr != nil {
									return nil, err
								}
								if tcp, ok := direct.(*net.TCPConn); ok {
									tcp.SetNoDelay(true)
									tcp.SetWriteBuffer(262144)
									tcp.SetReadBuffer(262144)
								}
								return direct, nil
							}
							return nil, err
						}
						if tcp, ok := conn.(*net.TCPConn); ok {
							tcp.SetNoDelay(true)
							tcp.SetWriteBuffer(262144)
							tcp.SetReadBuffer(262144)
						}
						return conn, nil
					}
				}
			}
		}
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// newDirectClient creates an HTTP client with NO proxy at all. Use for hosts
// whose CDN endpoints are unreachable through the configured proxy (e.g.
// StreamWish, VidHide on Hetzner-only CDNs).
func newDirectClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           dialWithTuning(30 * time.Second),
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// multipartStream builds a multipart request body that streams the file without
// loading it into RAM, while still setting an exact Content-Length so servers
// that reject chunked transfer encoding (Streamtape, Mixdrop) work.
//
// fields is written before the file part (may be nil).
// If host is non-empty the file part is wrapped with a ProgressReader.
// Returns: body reader, content-length, multipart content-type, closer (the opened file), error.
func multipartStream(fields map[string]string, fileField, filePath, host string) (io.Reader, int64, string, io.Closer, error) {
	return multipartStreamWithProgress(fields, fileField, filePath, host, nil)
}

func multipartStreamWithProgress(fields map[string]string, fileField, filePath, host string, progress ProgressFunc) (io.Reader, int64, string, io.Closer, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, 0, "", nil, fmt.Errorf("stat: %w", err)
	}

	// Build the preamble (all multipart headers, but NOT the file bytes).
	var preamble bytes.Buffer
	mw := multipart.NewWriter(&preamble)

	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, 0, "", nil, fmt.Errorf("write field %s: %w", k, err)
		}
	}

	// CreateFormFile writes the part header into preamble; we do NOT write file
	// bytes through this writer — they come from the file directly.
	if _, err := mw.CreateFormFile(fileField, filepath.Base(filePath)); err != nil {
		return nil, 0, "", nil, fmt.Errorf("create form file: %w", err)
	}

	// Closing boundary that would normally be written by mw.Close().
	closing := fmt.Sprintf("\r\n--%s--\r\n", mw.Boundary())
	contentType := mw.FormDataContentType()
	totalLen := int64(preamble.Len()) + fi.Size() + int64(len(closing))

	file, err := os.Open(filePath)
	if err != nil {
		return nil, 0, "", nil, fmt.Errorf("open: %w", err)
	}

	// Buffered reader with 512KB buffer for fewer syscalls during upload
	bufFile := bufio.NewReaderSize(file, 512*1024)
	var fileReader io.Reader = bufFile
	if host != "" {
		fileReader = NewProgressReaderWithCallback(bufFile, fi.Size(), host, progress)
	}

	body := io.MultiReader(&preamble, fileReader, bytes.NewReader([]byte(closing)))
	return body, totalLen, contentType, file, nil
}

// keyRing manages multiple API keys and rotates through them on permanent
// (quota) errors.  Keys are provided as a slice, typically from a
// comma-separated config value.
type keyRing struct {
	mu    sync.Mutex
	keys  []string
	index int
}

func newKeyRing(keys []string) *keyRing {
	var cleaned []string
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			cleaned = append(cleaned, k)
		}
	}
	return &keyRing{keys: cleaned}
}

func (kr *keyRing) current() string {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if len(kr.keys) == 0 {
		return ""
	}
	return kr.keys[kr.index]
}

func (kr *keyRing) rotate() {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if len(kr.keys) > 1 {
		kr.index = (kr.index + 1) % len(kr.keys)
	}
}

func (kr *keyRing) count() int {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	return len(kr.keys)
}

// Logger is the interface for logging upload events.
// The channel package implements this with ch.Info/ch.Error.
type Logger interface {
	Info(format string, a ...any)
	Error(format string, a ...any)
}

// UploadResult contains the result of an upload to a specific host
type UploadResult struct {
	Host         string
	DownloadLink string
	Error        error
	PosterURL    string // auto-generated poster (SeekStreaming)
	PreviewURL   string // auto-generated preview (SeekStreaming)
}

// MultiHostUploader handles uploading to multiple hosts simultaneously
type MultiHostUploader struct {
	gofile        *GoFileUploader
	voesx         *VoeSXUploader
	streamtape    *StreamtapeUploader
	mixdrop       *MixdropUploader
	seekstreaming *SeekStreamingUploader
	vidhide       *VidHideUploader
	streamwish    *StreamWishUploader
	log           Logger
	hostInitOnce  sync.Once
	hosts         map[string]uploaderFunc // host name -> upload function, lazy-init
	progress      ProgressFunc
}

type uploaderFunc func(string, ProgressFunc) (string, error)

func (m *MultiHostUploader) initHosts() {
	m.hostInitOnce.Do(func() {
		if m.hosts != nil {
			return
		}
		m.hosts = map[string]uploaderFunc{}
		m.hosts["GoFile"] = m.gofile.UploadWithProgress
		if m.voesx != nil && m.voesx.apiKey != "" {
			m.hosts["VOE.sx"] = m.voesx.UploadWithProgress
		}
		if m.streamtape != nil && m.streamtape.login != "" && m.streamtape.key != "" {
			m.hosts["Streamtape"] = m.streamtape.UploadWithProgress
		}
		if m.mixdrop != nil && m.mixdrop.email != "" && m.mixdrop.token != "" {
			m.hosts["Mixdrop"] = m.mixdrop.UploadWithProgress
		}
		if m.seekstreaming != nil && m.seekstreaming.key != "" {
			m.hosts["SeekStreaming"] = m.seekstreaming.UploadWithProgress
		}
		if m.vidhide != nil && m.vidhide.keys.count() > 0 {
			m.hosts["VidHide"] = m.vidhide.UploadWithProgress
		}
		if m.streamwish != nil && m.streamwish.keys.count() > 0 {
			m.hosts["StreamWish"] = m.streamwish.UploadWithProgress
		}
	})
}

// NewMultiHostUploader creates a new multi-host uploader.
// vidHideAPIKeys and streamWishAPIKeys support comma-separated multiple keys
// for automatic key rotation on daily quota limits.
func NewMultiHostUploader(voeSXAPIKey, streamtapeLogin, streamtapeKey, mixdropEmail, mixdropToken, seekStreamingKey string, vidHideAPIKeys, streamWishAPIKeys []string, log Logger) *MultiHostUploader {
	if log == nil {
		log = &nilLogger{}
	}
	return &MultiHostUploader{
		gofile:        NewGoFileUploader(),
		voesx:         NewVoeSXUploader(voeSXAPIKey),
		streamtape:    NewStreamtapeUploader(streamtapeLogin, streamtapeKey),
		mixdrop:       NewMixdropUploader(mixdropEmail, mixdropToken),
		seekstreaming: NewSeekStreamingUploader(seekStreamingKey),
		vidhide:       NewVidHideUploader(vidHideAPIKeys),
		streamwish:    NewStreamWishUploader(streamWishAPIKeys),
		log:           log,
	}
}

// SetProgressCallback sets an upload-local progress callback for this uploader.
func (m *MultiHostUploader) SetProgressCallback(fn ProgressFunc) {
	m.progress = fn
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"

// rateLimitError wraps an error to explicitly mark it as rate-limit related.
// Uploaders return this when an API response indicates a quota/rate limit
// even though the HTTP status was 200 OK.
type rateLimitError struct {
	err error
}

func (e *rateLimitError) Error() string { return e.err.Error() }
func (e *rateLimitError) Unwrap() error { return e.err }

// permanentError wraps an error to signal that retrying is futile
// (e.g. daily quota exhausted). Outer retry loops should skip this host.
type permanentError struct {
	err error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// IsPermanentError returns true if the error (or any wrapped error) is a permanentError.
// Exported so outer retry loops (channel package) can detect hard failures.
func IsPermanentError(err error) bool {
	if err == nil {
		return false
	}
	var pe *permanentError
	return errors.As(err, &pe)
}

// IsProxyError returns true if the error indicates a proxy connectivity issue
// (connection refused, SOCKS failure, closed network). Outer retry loops should
// skip retrying hosts that fail with proxy errors since the proxy is likely down.
func IsProxyError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "actively refused") ||
		strings.Contains(errStr, "SOCKS") ||
		strings.Contains(errStr, "socks") ||
		strings.Contains(errStr, "use of closed network connection") ||
		(strings.Contains(errStr, "wsasend") &&
			strings.Contains(errStr, "forcibly closed"))
}

// isUploadRateLimited returns true if the error indicates a rate-limit hit
// (429 Too Many Requests or similar). Uses a different name than imgbb.go's
// isRateLimitError to avoid redeclaration.
func isUploadRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var rle *rateLimitError
	if errors.As(err, &rle) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "429") ||
		strings.Contains(msg, "too many requests")
}

// uploadBackoff returns the appropriate backoff duration based on whether
// the error was a rate-limit hit. Rate limits get a longer 30s+10s/attempt,
// while other errors use standard exponential delay.
func uploadBackoff(attempt int, err error) time.Duration {
	if isUploadRateLimited(err) {
		// Long backoff for rate limits — wait 30s + 10s per retry
		return 30*time.Second + time.Duration(attempt)*10*time.Second
	}
	// Standard exponential backoff: 5s, 10s, 20s, 40s...
	return time.Duration((1<<uint(attempt))*5) * time.Second
}

// nilLogger discards all log messages when no logger is provided.
type nilLogger struct{}

func (n *nilLogger) Info(format string, a ...any)  {}
func (n *nilLogger) Error(format string, a ...any) {}

// UploadToAll uploads a file to all configured hosts in parallel.
// Returns a slice of results, one for each host.
func (m *MultiHostUploader) UploadToAll(filePath string) []UploadResult {
	m.initHosts()
	hosts := make([]string, 0, len(m.hosts))
	for name := range m.hosts {
		hosts = append(hosts, name)
	}
	return m.UploadSelected(filePath, hosts)
}

// UploadSelected uploads a file to the specified hosts in parallel.
// Host names that are not configured are silently skipped.
func (m *MultiHostUploader) UploadSelected(filePath string, hosts []string) []UploadResult {
	m.initHosts()
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := []UploadResult{}

	progressFn := m.progress
	for _, name := range hosts {
		uploadFn, ok := m.hosts[name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(host string, fn uploaderFunc) {
			defer wg.Done()
			m.log.Info("upload: starting %s upload for %s", host, filePath)
			link, err := fn(filePath, progressFn)
			result := UploadResult{
				Host:         host,
				DownloadLink: link,
				Error:        err,
			}
			if err == nil && host == "SeekStreaming" && m.seekstreaming != nil {
				result.PosterURL = m.seekstreaming.LastPosterURL()
				result.PreviewURL = m.seekstreaming.LastPreviewURL()
			}
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
			if err != nil {
				m.log.Error("upload: %s failed for %s: %v", host, filePath, err)
			} else {
				m.log.Info("upload: %s successful for %s: %s", host, filePath, link)
			}
		}(name, uploadFn)
	}

	wg.Wait()
	return results
}

// UploadSelectedPriority uploads to the priority host first (sequentially),
// then to remaining hosts in parallel. This ensures the priority host gets
// full bandwidth during shutdown when time is limited.
func (m *MultiHostUploader) UploadSelectedPriority(filePath string, hosts []string, priorityHost string) []UploadResult {
	m.initHosts()

	var priorityHosts []string
	var otherHosts []string
	for _, host := range hosts {
		if host == priorityHost {
			priorityHosts = append(priorityHosts, host)
		} else {
			otherHosts = append(otherHosts, host)
		}
	}

	var results []UploadResult
	progressFn := m.progress

	for _, host := range priorityHosts {
		fn, ok := m.hosts[host]
		if !ok {
			continue
		}
		m.log.Info("upload: priority upload to %s for %s", host, filePath)
		link, err := fn(filePath, progressFn)
		results = append(results, UploadResult{Host: host, DownloadLink: link, Error: err})
		if err != nil {
			m.log.Error("upload: %s (priority) failed for %s: %v", host, filePath, err)
		} else {
			m.log.Info("upload: %s (priority) successful for %s: %s", host, filePath, link)
		}
	}

	if len(otherHosts) > 0 {
		otherResults := m.UploadSelected(filePath, otherHosts)
		results = append(results, otherResults...)
	}

	return results
}

// AvailableHosts returns the names of all configured upload hosts.
func (m *MultiHostUploader) AvailableHosts() []string {
	m.initHosts()
	hosts := make([]string, 0, len(m.hosts))
	for name := range m.hosts {
		hosts = append(hosts, name)
	}
	return hosts
}

// GetSuccessfulUploads returns only the successful upload results
func GetSuccessfulUploads(results []UploadResult) []UploadResult {
	var successful []UploadResult
	for _, result := range results {
		if result.Error == nil && result.DownloadLink != "" {
			successful = append(successful, result)
		}
	}
	return successful
}

// FormatResults formats upload results into a readable string
func FormatResults(results []UploadResult) string {
	var output string
	successCount := 0

	for _, result := range results {
		if result.Error == nil && result.DownloadLink != "" {
			output += fmt.Sprintf("✓ %s: %s\n", result.Host, result.DownloadLink)
			successCount++
		} else {
			output += fmt.Sprintf("✗ %s: %v\n", result.Host, result.Error)
		}
	}

	output = fmt.Sprintf("Upload completed: %d/%d successful\n%s", successCount, len(results), output)
	return output
}
