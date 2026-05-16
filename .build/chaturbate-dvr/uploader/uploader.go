package uploader

import (
	"fmt"
	"sync"
)

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
	gofile      *GoFileUploader
	turboviplay *TurboViPlayUploader
	voesx       *VoeSXUploader
	streamtape  *StreamtapeUploader
	sendcm      *SendCMUploader
	byse        *ByseUploader
	log         Logger
}

// NewMultiHostUploader creates a new multi-host uploader
func NewMultiHostUploader(turboViPlayAPIKey, voeSXAPIKey, streamtapeLogin, streamtapeAPIKey, sendCMAPIKey, byseAPIKey string, log Logger) *MultiHostUploader {
	if log == nil {
		log = &nilLogger{}
	}
	return &MultiHostUploader{
		gofile:      NewGoFileUploader(),
		turboviplay: NewTurboViPlayUploader(turboViPlayAPIKey),
		voesx:       NewVoeSXUploader(voeSXAPIKey),
		streamtape:  NewStreamtapeUploader(streamtapeLogin, streamtapeAPIKey),
		sendcm:      NewSendCMUploader(sendCMAPIKey),
		byse:        NewByseUploader(byseAPIKey),
		log:         log,
	}
}

// nilLogger discards all log messages when no logger is provided.
type nilLogger struct{}

func (n *nilLogger) Info(format string, a ...any) {}
func (n *nilLogger) Error(format string, a ...any) {}

// UploadToAll uploads a file to all configured hosts in parallel
// Returns a slice of results, one for each host
func (m *MultiHostUploader) UploadToAll(filePath string) []UploadResult {
	var wg sync.WaitGroup
	results := []UploadResult{}
	resultsMu := sync.Mutex{}

	// Upload to GoFile
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.log.Info("upload: starting GoFile upload for %s", filePath)
		link, err := m.gofile.Upload(filePath)
		resultsMu.Lock()
		results = append(results, UploadResult{
			Host:         "GoFile",
			DownloadLink: link,
			Error:        err,
		})
		resultsMu.Unlock()
		if err != nil {
			m.log.Error("upload: GoFile failed for %s: %v", filePath, err)
		} else {
			m.log.Info("upload: GoFile successful for %s: %s", filePath, link)
		}
	}()

	// Upload to TurboViPlay (only if API key is configured)
	if m.turboviplay != nil && m.turboviplay.apiKey != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.log.Info("upload: starting TurboViPlay upload for %s", filePath)
			link, err := m.turboviplay.Upload(filePath)
			resultsMu.Lock()
			results = append(results, UploadResult{
				Host:         "TurboViPlay",
				DownloadLink: link,
				Error:        err,
			})
			resultsMu.Unlock()
			if err != nil {
				m.log.Error("upload: TurboViPlay failed for %s: %v", filePath, err)
			} else {
				m.log.Info("upload: TurboViPlay successful for %s: %s", filePath, link)
			}
		}()
	}

	// Upload to VOE.sx (only if API key is configured)
	if m.voesx != nil && m.voesx.apiKey != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.log.Info("upload: starting VOE.sx upload for %s", filePath)
			link, err := m.voesx.Upload(filePath)
			resultsMu.Lock()
			results = append(results, UploadResult{
				Host:         "VOE.sx",
				DownloadLink: link,
				Error:        err,
			})
			resultsMu.Unlock()
			if err != nil {
				m.log.Error("upload: VOE.sx failed for %s: %v", filePath, err)
			} else {
				m.log.Info("upload: VOE.sx successful for %s: %s", filePath, link)
			}
		}()
	}

	// Upload to Streamtape (only if credentials are configured)
	if m.streamtape != nil && m.streamtape.login != "" && m.streamtape.apiKey != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.log.Info("upload: starting Streamtape upload for %s", filePath)
			link, err := m.streamtape.Upload(filePath)
			resultsMu.Lock()
			results = append(results, UploadResult{
				Host:         "Streamtape",
				DownloadLink: link,
				Error:        err,
			})
			resultsMu.Unlock()
			if err != nil {
				m.log.Error("upload: Streamtape failed for %s: %v", filePath, err)
			} else {
				m.log.Info("upload: Streamtape successful for %s: %s", filePath, link)
			}
		}()
	}

	// Upload to SendCM (always, guest upload if no API key)
	if m.sendcm != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.log.Info("upload: starting SendCM upload for %s", filePath)
			link, err := m.sendcm.Upload(filePath)
			resultsMu.Lock()
			results = append(results, UploadResult{
				Host:         "SendCM",
				DownloadLink: link,
				Error:        err,
			})
			resultsMu.Unlock()
			if err != nil {
				m.log.Error("upload: SendCM failed for %s: %v", filePath, err)
			} else {
				m.log.Info("upload: SendCM successful for %s: %s", filePath, link)
			}
		}()
	}

	// Upload to Byse (only if API key is configured)
	if m.byse != nil && m.byse.apiKey != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.log.Info("upload: starting Byse upload for %s", filePath)
			link, err := m.byse.Upload(filePath)
			resultsMu.Lock()
			results = append(results, UploadResult{
				Host:         "Byse",
				DownloadLink: link,
				Error:        err,
			})
			resultsMu.Unlock()
			if err != nil {
				m.log.Error("upload: Byse failed for %s: %v", filePath, err)
			} else {
				m.log.Info("upload: Byse successful for %s: %s", filePath, link)
			}
		}()
	}

	// Wait for all uploads to complete
	wg.Wait()

	return results
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
