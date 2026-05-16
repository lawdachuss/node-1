package uploader

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// ProgressReader wraps an io.Reader and reports progress
type ProgressReader struct {
	reader          io.Reader
	total           int64
	current         int64
	lastPrint       time.Time
	bytesSincePrint int64
	onProgress      func(current, total int64, speed float64)
}

// NewProgressReader creates a new progress reader
func NewProgressReader(reader io.Reader, total int64, onProgress func(current, total int64, speed float64)) *ProgressReader {
	return &ProgressReader{
		reader:     reader,
		total:      total,
		lastPrint:  time.Now(),
		onProgress: onProgress,
	}
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(&pr.current, int64(n))
		pr.bytesSincePrint += int64(n)
		
		// Report progress every 500ms
		elapsed := time.Since(pr.lastPrint)
		if elapsed > 500*time.Millisecond {
			current := atomic.LoadInt64(&pr.current)
			speed := float64(pr.bytesSincePrint) / elapsed.Seconds() / 1024 / 1024 // MB/s
			
			if pr.onProgress != nil {
				pr.onProgress(current, pr.total, speed)
			}
			pr.lastPrint = time.Now()
			pr.bytesSincePrint = 0
		}
	}
	return n, err
}

// FormatBytes formats bytes to human readable format
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
