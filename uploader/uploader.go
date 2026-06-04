package uploader

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// newNoProxyClient returns an http.Client that explicitly bypasses any
// environment-configured proxy (ALL_PROXY / HTTP_PROXY / HTTPS_PROXY).
// The Chaturbate DVR proxy setting is only meant for Chaturbate requests;
// image/thumbnail upload services must reach the public internet directly.
func newNoProxyClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, // never use environment proxy
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// multipartStream builds a multipart request body that streams the file without
// loading it into RAM, while still setting an exact Content-Length so servers
// that reject chunked transfer encoding (Streamtape, Mixdrop, Pixeldrain) work.
//
// fields is written before the file part (may be nil).
// Returns: body reader, content-length, multipart content-type, closer (the opened file), error.
func multipartStream(fields map[string]string, fileField, filePath string) (io.Reader, int64, string, io.Closer, error) {
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

        body := io.MultiReader(&preamble, file, bytes.NewReader([]byte(closing)))
        return body, totalLen, contentType, file, nil
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
}

// MultiHostUploader handles uploading to multiple hosts simultaneously
type MultiHostUploader struct {
	gofile     *GoFileUploader
	voesx      *VoeSXUploader
	streamtape *StreamtapeUploader
	mixdrop    *MixdropUploader
	pixeldrain *PixeldrainUploader
	log        Logger
	hosts      map[string]uploaderFunc // host name -> upload function, lazy-init
}

type uploaderFunc func(string) (string, error)

func (m *MultiHostUploader) initHosts() {
	if m.hosts != nil {
		return
	}
	m.hosts = map[string]uploaderFunc{}
	m.hosts["GoFile"] = m.gofile.Upload
	if m.voesx != nil && m.voesx.apiKey != "" {
		m.hosts["VOE.sx"] = m.voesx.Upload
	}
	if m.streamtape != nil && m.streamtape.login != "" && m.streamtape.key != "" {
		m.hosts["Streamtape"] = m.streamtape.Upload
	}
	if m.mixdrop != nil && m.mixdrop.email != "" && m.mixdrop.token != "" {
		m.hosts["Mixdrop"] = m.mixdrop.Upload
	}
	if m.pixeldrain != nil && m.pixeldrain.token != "" {
		m.hosts["PixelDrain"] = m.pixeldrain.Upload
	}
}

// NewMultiHostUploader creates a new multi-host uploader
func NewMultiHostUploader(voeSXAPIKey, streamtapeLogin, streamtapeKey, mixdropEmail, mixdropToken, pixeldrainToken string, log Logger) *MultiHostUploader {
        if log == nil {
                log = &nilLogger{}
        }
        return &MultiHostUploader{
                gofile:     NewGoFileUploader(),
                voesx:      NewVoeSXUploader(voeSXAPIKey),
                streamtape: NewStreamtapeUploader(streamtapeLogin, streamtapeKey),
                mixdrop:    NewMixdropUploader(mixdropEmail, mixdropToken),
                pixeldrain: NewPixeldrainUploader(pixeldrainToken),
                log:        log,
        }
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"

// nilLogger discards all log messages when no logger is provided.
type nilLogger struct{}

func (n *nilLogger) Info(format string, a ...any) {}
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

	for _, name := range hosts {
		uploadFn, ok := m.hosts[name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(host string, fn uploaderFunc) {
			defer wg.Done()
			m.log.Info("upload: starting %s upload for %s", host, filePath)
			link, err := fn(filePath)
			mu.Lock()
			results = append(results, UploadResult{
				Host:         host,
				DownloadLink: link,
				Error:        err,
			})
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
