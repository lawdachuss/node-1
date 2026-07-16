package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

var pendingDirMu sync.Map // map[string]*sync.Mutex, keyed by channel username

// pendingMu returns the per-channel mutex for the given username.
// Each channel has its own lock so one channel's ffmpeg encode does not
// block another channel's pending directory operations.
func pendingMu(username string) *sync.Mutex {
	v, _ := pendingDirMu.LoadOrStore(username, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type Pattern struct {
	Username string
	Sequence int
	Year     string
	Month    string
	Day      string
	Hour     string
	Minute   string
	Second   string
}

// CloseMode controls whether Cleanup processes pending files immediately
// or defers processing for later (batched session stop).
type CloseMode int

const (
	CloseProcess CloseMode = iota // close files + process pending (rotation, pause, stream error)
	CloseQueue                    // close files only, defer processing to ProcessPending (session stop)
)

// NextFile prepares the next file to be created, by cleaning up the last file and generating a new one
func (ch *Channel) NextFile() error {
	if err := ch.Cleanup(CloseProcess); err != nil {
		return err
	}
	filename, err := ch.GenerateFilename()
	if err != nil {
		return err
	}
	ch.stateMu.Lock()
	ch.CurrentFilename = filename
	ch.videoSegmentCount = 0
	ch.audioSegmentCount = 0
	ch.stateMu.Unlock()

	if err := ch.CreateNewFile(filename); err != nil {
		return err
	}

	// Increment the sequence number for the next file
	ch.Sequence.Add(1)
	return nil
}

// Cleanup closes any open recording files.
// CloseProcess: also mux/compress/upload pending files asynchronously (rotation, pause, stream error).
// CloseQueue:   only close and queue files; caller must call ProcessPending() later (session stop).
func (ch *Channel) Cleanup(mode CloseMode) error {
	ch.cleanupMu.Lock()
	defer ch.cleanupMu.Unlock()

	if ch.File == nil && ch.AudioFile == nil && len(ch.pendingFiles) == 0 {
		return nil
	}

	// Close any open files and add them to the pending queue (or remove empty ones).
	// Errors from closeTrackedFile are logged but not returned — aborting
	// would strand ALL previously queued pendingFiles permanently.
	if ch.File != nil || ch.AudioFile != nil {
		videoPath, _, err := closeTrackedFile(ch.File)
		if err != nil {
			ch.Error("cleanup: video file close: %s", err.Error())
		}
		audioPath, _, err := closeTrackedFile(ch.AudioFile)
		if err != nil {
			ch.Error("cleanup: audio file close: %s", err.Error())
		}

		ch.stateMu.Lock()
		ch.File = nil
		ch.AudioFile = nil
		ch.CurrentFilename = ""
		ch.Filesize = 0
		ch.Duration = 0
		hasVideo := ch.videoSegmentCount > 0
		hasAudio := ch.audioSegmentCount > 0
		ch.stateMu.Unlock()

		// Remove files that contain only init segments with no media data.
		if ch.HasSeparateAudio && !hasVideo && !hasAudio {
			if videoPath != "" {
				os.Remove(videoPath)
			}
			if audioPath != "" {
				os.Remove(audioPath)
			}
		} else if !ch.HasSeparateAudio && !hasVideo {
			if videoPath != "" {
				os.Remove(videoPath)
			}
			if mode == CloseQueue && len(ch.pendingFiles) > 10 {
				ch.Warn("cleanup: %d pending files accumulated during rotation — will be processed when recording ends", len(ch.pendingFiles))
			}
		} else {
			ch.stateMu.Lock()
			hasSeparateAudio := ch.HasSeparateAudio
			skipMinDur := ch.Config.IsPaused.Load() || ch.skipMinDurOnExit
			ch.stateMu.Unlock()
			ch.skipMinDurOnExit = false
			ch.pendingFiles = append(ch.pendingFiles, pendingFile{
				videoPath:        videoPath,
				audioPath:        audioPath,
				hasSeparateAudio: hasSeparateAudio,
				skipMinDuration:  skipMinDur,
			})
			if videoPath != "" {
				ch.Info("cleanup: queued %s for post-processing (%d pending)", filepath.Base(videoPath), len(ch.pendingFiles))
			} else if audioPath != "" {
				ch.Info("cleanup: queued %s for post-processing (%d pending)", filepath.Base(audioPath), len(ch.pendingFiles))
			}
		}
	}

	if mode == CloseProcess && len(ch.pendingFiles) > 0 {
		files := ch.pendingFiles
		ch.pendingFiles = nil
		ch.pendingWg.Add(1)
		go func() {
			defer ch.pendingWg.Done()
			for _, pf := range files {
				ch.processPendingFile(pf)
			}
		}()
	}
	return nil
}

// processPendingQueue processes all pending files: mux A/V if needed, move to
// output dir, generate previews, upload, save metadata, and delete local files.
// Must be called with cleanupMu held.
func (ch *Channel) processPendingQueue() {
	if len(ch.pendingFiles) == 0 {
		return
	}
	ch.Info("cleanup: processing %d pending file(s)", len(ch.pendingFiles))

	for _, pf := range ch.pendingFiles {
		ch.processPendingFile(pf)
	}
	ch.pendingFiles = nil
}

func (ch *Channel) processPendingFile(pf pendingFile) {
	videoPath := pf.videoPath
	audioPath := pf.audioPath

	if pf.hasSeparateAudio {
		ch.processPendingMuxPair(videoPath, audioPath, pf.skipMinDuration)
		return
	}

	// Single-stream file — move to output dir (triggers preview + upload).
	if _, err := os.Stat(videoPath); err == nil {
		if ch.Config.Compress {
			normalized, normErr := normalizeFMP4Timestamps(videoPath, func(msg string) { ch.Info("normalize: %s", msg) })
			if normErr != nil {
				ch.Warn("normalize: could not reset timestamps for %s: %v — uploading with original timestamps", filepath.Base(videoPath), normErr)
			}
			if ch.handleMinDurationAndMerge(normalized, pf.skipMinDuration) {
				return // video was deferred to pending or merged+uploaded
			}
			ch.CompressFile(normalized)
			return
		} else {
			normalized, normErr := normalizeFMP4Timestamps(videoPath, func(msg string) { ch.Info("normalize: %s", msg) })
			if normErr != nil {
				ch.Warn("normalize: could not reset timestamps for %s: %v — uploading with original timestamps", filepath.Base(videoPath), normErr)
			}
			if ch.handleMinDurationAndMerge(normalized, pf.skipMinDuration) {
				return // video was deferred to pending or merged+uploaded
			}
			ch.MoveToOutputDir(normalized)
		}
	}
}

func (ch *Channel) processPendingMuxPair(videoPath, audioPath string, skipMinDuration bool) {
	videoInfo, _ := os.Stat(videoPath)
	audioInfo, _ := os.Stat(audioPath)

	switch {
	case videoInfo == nil && audioInfo == nil:
		return
	case videoInfo == nil:
		if muxedFileFromSidecar(audioPath) != "" {
			ch.Info("mux: stale audio sidecar %s (muxed version exists) — removing", filepath.Base(audioPath))
			os.Remove(audioPath)
			return
		}
		ch.Info("mux: video track missing; preserving audio-only file %s", filepath.Base(audioPath))
		normalized, normErr := normalizeFMP4Timestamps(audioPath, func(msg string) { ch.Info("normalize: %s", msg) })
		if normErr != nil {
			ch.Warn("normalize: could not reset timestamps for %s: %v — uploading with original timestamps", filepath.Base(audioPath), normErr)
		}
		if ch.handleMinDurationAndMerge(normalized, skipMinDuration) {
			return
		}
		if ch.Config.Compress {
			ch.CompressFile(normalized)
		} else {
			ch.MoveToOutputDir(normalized)
		}
		return
	case audioInfo == nil:
		if muxedFileFromSidecar(videoPath) != "" {
			ch.Info("mux: stale video sidecar %s (muxed version exists) — removing", filepath.Base(videoPath))
			os.Remove(videoPath)
			return
		}
		ch.Info("mux: audio track missing; preserving video-only file %s", filepath.Base(videoPath))
		normalized, normErr := normalizeFMP4Timestamps(videoPath, func(msg string) { ch.Info("normalize: %s", msg) })
		if normErr != nil {
			ch.Warn("normalize: could not reset timestamps for %s: %v — uploading with original timestamps", filepath.Base(videoPath), normErr)
		}
		if ch.handleMinDurationAndMerge(normalized, skipMinDuration) {
			return
		}
		if ch.Config.Compress {
			ch.CompressFile(normalized)
		} else {
			ch.MoveToOutputDir(normalized)
		}
		return
	}

	// Both tracks exist — mux them together.
	finalOutput := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + ".muxed.mp4"
	if err := ch.MuxAV(videoPath, audioPath, finalOutput); err != nil {
		ch.Info("mux: ffmpeg mux failed, trying native fallback: %s", err.Error())
		if nativeErr := ch.MuxAVNative(videoPath, audioPath, finalOutput); nativeErr != nil {
			ch.Error("mux failed for %s: %v — uploading tracks separately", filepath.Base(videoPath), nativeErr)
			_ = os.Remove(finalOutput)
			ch.MoveToOutputDir(videoPath)
			ch.MoveToOutputDir(audioPath)
			return
		}
	}

	if ok, reason := validateVideoFile(finalOutput, true); !ok {
		ch.Error("mux: output looks corrupt (%s); uploading sidecars %s and %s separately", reason, filepath.Base(videoPath), filepath.Base(audioPath))
		_ = os.Remove(finalOutput)
		ch.MoveToOutputDir(videoPath)
		ch.MoveToOutputDir(audioPath)
		return
	}

	_ = os.Remove(videoPath)
	_ = os.Remove(audioPath)
	ch.Info("delete: removed sidecar %s", filepath.Base(videoPath))
	ch.Info("delete: removed sidecar %s", filepath.Base(audioPath))

	if ch.Config.Compress {
		normalized, normErr := normalizeFMP4Timestamps(finalOutput, func(msg string) { ch.Info("normalize: %s", msg) })
		if normErr != nil {
			ch.Warn("normalize: could not reset timestamps on muxed output %s: %v — uploading with original timestamps", filepath.Base(finalOutput), normErr)
		}
		if ch.handleMinDurationAndMerge(normalized, skipMinDuration) {
			return // video was deferred to pending or merged+uploaded
		}
		ch.CompressFile(normalized)
	} else {
		normalized, normErr := normalizeFMP4Timestamps(finalOutput, func(msg string) { ch.Info("normalize: %s", msg) })
		if normErr != nil {
			ch.Warn("normalize: could not reset timestamps on muxed output %s: %v — uploading with original timestamps", filepath.Base(finalOutput), normErr)
		}
		if ch.handleMinDurationAndMerge(normalized, skipMinDuration) {
			return // video was deferred to pending or merged+uploaded
		}
		ch.MoveToOutputDir(normalized)
	}
}

// validateVideoFile performs a real integrity check on a finalized video
// before it is uploaded or kept.  The previous muxOutputLooksValid only
// checked the file was non-empty, so a structurally-valid but playback-corrupt
// file (e.g. produced by native fMP4 muxing of incomplete segments) was still
// uploaded.  We require a valid video stream with positive duration, and —
// when fullDecode is set — a complete null-decode with no ffmpeg decode errors.
// Corrupt files are rejected so they are never pushed to a host.
func validateVideoFile(filePath string, fullDecode bool) (bool, string) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return false, fmt.Sprintf("stat: %v", err)
	}
	if fi.Size() == 0 {
		return false, "empty file"
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer probeCancel()
	out, perr := config.FFprobeCommandContext(probeCtx,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_type,width,height:format=duration",
		"-of", "json",
		filePath,
	).Output()
	if perr != nil {
		return false, fmt.Sprintf("probe failed: %v", perr)
	}
	var p struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return false, fmt.Sprintf("probe parse: %v", err)
	}
	hasVideo := false
	for _, s := range p.Streams {
		if s.CodecType == "video" && s.Width > 0 && s.Height > 0 {
			hasVideo = true
			break
		}
	}
	if !hasVideo {
		return false, "no valid video stream"
	}
	if d, e := strconv.ParseFloat(p.Format.Duration, 64); e != nil || d <= 0 {
		return false, "invalid or zero duration"
	}

	if !fullDecode {
		return true, ""
	}

	// Full null-decode catches corruption a probe cannot see (missing/garbage
	// frames, broken references). Bounded by a deadline so huge files don't
	// block the pipeline — a timeout is treated as "accept", not "reject".
	decCtx, decCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer decCancel()
	decOut, derr := config.FFmpegCommandContext(decCtx,
		"-v", "error",
		"-i", filePath,
		"-f", "null", "-",
	).CombinedOutput()
	if derr != nil {
		if decCtx.Err() == context.DeadlineExceeded {
			return true, "" // too large to decode in time — accept
		}
		ds := string(decOut)
		if len(ds) > 500 {
			ds = ds[len(ds)-500:]
		}
		return false, fmt.Sprintf("decode error: %v (ffmpeg: %s)", derr, ds)
	}
	return true, ""
}

// muxedFileFromSidecar checks if a .video.muxed.mp4 file exists for the
// given sidecar path (.video.mp4 or .audio.mp4).  Returns the muxed path if
// it exists, or "" otherwise.
func muxedFileFromSidecar(sidecarPath string) string {
	base := sidecarPath
	// Strip .video.mp4 or .audio.mp4 suffix.
	for _, suf := range []string{".video.mp4", ".audio.mp4"} {
		if strings.HasSuffix(base, suf) {
			muxedPath := strings.TrimSuffix(base, suf) + ".video.muxed.mp4"
			if _, err := os.Stat(muxedPath); err == nil {
				return muxedPath
			}
			return ""
		}
	}
	return ""
}

// videoExt returns true if the extension is a known video extension.
func videoExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".mp4" || ext == ".mkv"
}

// isSidecar returns true if the filename appears to be a sidecar/preview file.
// Note: .video.muxed.mp4 is the final muxed output (not a sidecar), while
// .video.mp4 and .audio.mp4 are raw A/V track files (sidecars).
func isSidecar(name string) bool {
	return strings.HasSuffix(name, ".thumb.webp") ||
		strings.HasSuffix(name, ".thumb.jpg") ||
		strings.HasSuffix(name, ".sprite.webp") ||
		strings.HasSuffix(name, ".sprite.jpg") ||
		strings.HasSuffix(name, ".preview.webp") ||
		strings.HasSuffix(name, ".preview.mp4") ||
		strings.HasSuffix(name, ".thumb") ||
		strings.HasSuffix(name, ".sprite") ||
		strings.HasSuffix(name, ".video.mp4") ||
		strings.HasSuffix(name, ".audio.mp4")
}

// MoveToOutputDir relocates a finalized recording into server.Config.OutputDir.
// Errors are non-fatal: the recording is already safely written at srcPath.
func (ch *Channel) MoveToOutputDir(srcPath string) string {
	// Enqueue the file into the pipeline for thumbnail → upload → metadata → cleanup.
	// The pipeline handles all lifecycle (semaphore, waitgroup, state persistence).
	enqueue := func(filePath string) {
		ch.PipelineQueue.EnqueueFile(filePath)
	}

	if server.Config == nil || server.Config.OutputDir == "" {
		enqueue(srcPath)
		return srcPath
	}

	destDir := server.Config.OutputDir
	if server.Config.PerModelFolder {
		destDir = filepath.Join(destDir, ch.Config.Username)
	}
	if err := os.MkdirAll(destDir, 0777); err != nil {
		ch.Error("output-dir: mkdir %s: %s", destDir, err.Error())
		return srcPath
	}

	destPath := uniqueDestPath(filepath.Join(destDir, filepath.Base(srcPath)))
	ch.Info("output-dir: moving %s (%s) -> %s", filepath.Base(srcPath), resolvePathForLog(srcPath), destPath)
	// Mark in-flight before moveFile so the watcher's fsnotify handler
	// sees the file as already claimed by the pipeline.
	MarkUploadInFlight(destPath)
	if err := moveFile(srcPath, destPath); err != nil {
		ch.Error("output-dir: move %s to %s: %s — uploading from original location (%s)", filepath.Base(srcPath), destDir, err.Error(), resolvePathForLog(srcPath))
		MarkUploadDone(destPath) // release the failed-dest marker before marking src
		MarkUploadInFlight(srcPath)
		enqueue(srcPath)
		return srcPath
	}
	// Verify the destination actually exists after the move — on some
	// Windows configurations os.Rename can return nil without moving
	// the file (e.g. when src and dest resolve to the same path via
	// symlinks or junctions).  If the dest is missing, fall back to
	// the original location.
	if _, statErr := os.Stat(destPath); statErr != nil {
		ch.Error("output-dir: post-move stat of dest %s failed: %v — uploading from destination path (%s)", destPath, statErr, resolvePathForLog(destPath))
		MarkUploadDone(destPath) // release the failed-dest marker before re-marking dest
		MarkUploadInFlight(destPath)
		enqueue(destPath)
		return destPath
	}
	ch.Info("output-dir: moved %s -> %s", filepath.Base(srcPath), destPath)
	enqueue(destPath)
	return destPath
}

// resolvePathForLog resolves a path to its absolute form for logging.
// If resolution fails, returns the original path unchanged.
func resolvePathForLog(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func (ch *Channel) generatePreviewAndUpload(filePath string) {
	ch.PipelineQueue.EnqueueFile(filePath)
}

// uniqueDestPath returns path if it does not exist, otherwise appends
// " (n)" before the extension until an unused path is found.
func uniqueDestPath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(path)
	base := path[:len(path)-len(ext)]
	for i := 1; i < 100000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
	return fmt.Sprintf("%s (99999)%s", base, ext)
}

func moveFile(src, dest string) error {
	// Retry rename with backoff for transient Windows locks (AV, Search Indexer, etc.).
	for i := 0; i < 3; i++ {
		if err := os.Rename(src, dest); err == nil {
			return nil
		}
		time.Sleep(time.Duration(50*(1<<i)) * time.Millisecond)
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		in.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		in.Close()
		out.Close()
		os.Remove(dest)
		return err
	}
	// Sync before close so a crash between close and os.Remove(src) can't
	// leave a truncated destination alongside a deleted source.
	if err := out.Sync(); err != nil {
		in.Close()
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		in.Close()
		os.Remove(dest)
		return err
	}
	// Close the source handle BEFORE removing the file.  On Windows,
	// DeleteFileW fails with ERROR_ACCESS_DENIED when any handle is
	// still open, so defer in.Close() would keep the file busy.
	in.Close()

	// Retry remove with aggressive backoff (up to ~50s total) so transient
	// Windows locks (AV scanner, Search Indexer) have time to release.
	// If still locked, try rename + delete as a fallback.
	for i := 0; i < 20; i++ {
		if err := os.Remove(src); err == nil {
			return nil
		}
		if i >= 10 {
			tmpPath := fmt.Sprintf("%s.deleting.%d", src, i)
			if renameErr := os.Rename(src, tmpPath); renameErr == nil {
				if removeErr := os.Remove(tmpPath); removeErr == nil {
					return nil
				}
				os.Rename(tmpPath, src)
			}
		}
		backoff := time.Duration(100*(1<<uint(min(i, 8)))) * time.Millisecond // 100ms, 200ms, 400ms, … 25.6s
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
		time.Sleep(backoff)
	}
	// Source could not be removed — return the error so the caller can handle
	// the duplicate without silently leaving orphaned files.
	return fmt.Errorf("could not remove source after copy: %w", os.Remove(src))
}

// GenerateFilename creates a filename based on the configured pattern and the current timestamp
func (ch *Channel) GenerateFilename() (string, error) {
	var buf bytes.Buffer

	// Parse the filename pattern defined in the channel's config
	tpl, err := template.New("filename").Parse(ch.Config.Pattern)
	if err != nil {
		return "", fmt.Errorf("filename pattern error: %w", err)
	}

	// Get the current time based on the Unix timestamp when the stream was started
	t := time.Unix(ch.StreamedAt, 0)
	pattern := &Pattern{
		Username: ch.Config.Username,
		Sequence: int(ch.Sequence.Load()),
		Year:     t.Format("2006"),
		Month:    t.Format("01"),
		Day:      t.Format("02"),
		Hour:     t.Format("15"),
		Minute:   t.Format("04"),
		Second:   t.Format("05"),
	}

	if err := tpl.Execute(&buf, pattern); err != nil {
		return "", fmt.Errorf("template execution error: %w", err)
	}
	return buf.String(), nil
}

// CreateNewFile creates a new file for the channel using the given filename
func (ch *Channel) CreateNewFile(filename string) error {
	// Ensure the directory exists before creating the file
	if err := os.MkdirAll(filepath.Dir(filename), 0777); err != nil {
		return fmt.Errorf("mkdir all: %w", err)
	}

	videoPath := ch.videoPath(filename)
	file, err := os.OpenFile(videoPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0777)
	if err != nil {
		return fmt.Errorf("cannot open file: %s: %w", filename, err)
	}
	ch.File = file

	if len(ch.InitSegment) > 0 {
		n, err := ch.File.Write(ch.InitSegment)
		if err != nil {
			ch.File.Close()
			ch.File = nil
			return fmt.Errorf("write init segment: %w", err)
		}
		ch.stateMu.Lock()
		ch.Filesize += n
		ch.stateMu.Unlock()
	}

	if ch.HasSeparateAudio {
		audioPath := ch.audioPath(filename)
		audioFile, err := os.OpenFile(audioPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0777)
		if err != nil {
			_ = ch.File.Close()
			ch.File = nil
			return fmt.Errorf("cannot open audio file: %s: %w", filename, err)
		}
		ch.AudioFile = audioFile

		if len(ch.AudioInitSegment) > 0 {
			if _, err := ch.AudioFile.Write(ch.AudioInitSegment); err != nil {
				_ = ch.File.Close()
				_ = ch.AudioFile.Close()
				ch.File = nil
				ch.AudioFile = nil
				return fmt.Errorf("write audio init segment: %w", err)
			}
		}
	}

	return nil
}

func (ch *Channel) videoPath(filename string) string {
	if ch.HasSeparateAudio {
		return filename + ".video.mp4"
	}
	return filename + ".mp4"
}

func (ch *Channel) audioPath(filename string) string {
	return filename + ".audio.mp4"
}

func closeTrackedFile(file *os.File) (string, os.FileInfo, error) {
	if file == nil {
		return "", nil, nil
	}

	filename := file.Name()
	if err := file.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		file.Close()
		return filename, nil, fmt.Errorf("sync file: %w", err)
	}
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return filename, nil, fmt.Errorf("close file: %w", err)
	}

	fileInfo, err := os.Stat(filename)
	if err != nil && !os.IsNotExist(err) {
		return filename, nil, fmt.Errorf("stat file: %w", err)
	}
	if fileInfo != nil && fileInfo.Size() == 0 {
		if err := os.Remove(filename); err != nil {
			return filename, nil, fmt.Errorf("remove zero file: %w", err)
		}
		fileInfo = nil
	}

	return filename, fileInfo, nil
}

// moveToPendingDir moves a file into the .pending/<username>/ directory.
// Acquires pendingDirMu so it cannot race with handleMinDurationAndMerge,
// which may call deletePendingSegments concurrently.
//
// The destination name is collision-safe: if a file with the same basename
// already exists in the target pending dir (e.g. a stray being relocated back
// to its owner, or a duplicate from a retry), the moved file is given a unique
// " (n)" suffix rather than silently failing the rename and leaving the stray
// stranded in the wrong channel's directory forever.
func moveToPendingDir(filePath, username string) error {
	mu := pendingMu(username)
	mu.Lock()
	defer mu.Unlock()

	pendingDir := pendingSegmentsDir(username)
	if err := os.MkdirAll(pendingDir, 0777); err != nil {
		return fmt.Errorf("create pending dir: %w", err)
	}
	dest := uniqueDestPath(filepath.Join(pendingDir, filepath.Base(filePath)))
	return os.Rename(filePath, dest)
}

// DeleteSidecarFiles removes preview sidecar files associated with a video path.
// Skips .preview.mp4 if the video is currently in-flight (being uploaded) to
// prevent a race between DeleteSidecarFiles and the preview upload goroutine.
func DeleteSidecarFiles(videoPath string) {
	for _, suffix := range []string{".thumb.webp", ".thumb.jpg", ".sprite.webp", ".sprite.jpg", ".preview.webp", ".thumb", ".sprite"} {
		os.Remove(videoPath + suffix)
	}
	// Only delete .preview.mp4 when the video is NOT in-flight — the preview
	// upload goroutine checks this file's existence before uploading and would
	// fail with "file not found" if DeleteSidecarFiles raced ahead of it.
	if !IsUploadInFlight(videoPath) {
		os.Remove(videoPath + ".preview.mp4")
	}
}

// removeFileWithRetry attempts to remove a file, retrying up to 5 times
// with exponential backoff.  This handles transient Windows file locks
// from AV scanners, Search Indexer, upload handles still closing, etc.
//
// After 2 attempts, it tries a rename-then-delete strategy: renaming
// the file can succeed even when deletion is blocked by a reader (common with
// Windows Defender), which often releases the lock on the original path.
// Returns nil if the file was removed (or didn't exist).
func removeFileWithRetry(path string) error {
	for i := 0; i < 5; i++ {
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			return nil
		}

		if i >= 2 {
			tmpPath := fmt.Sprintf("%s.deleting.%d", path, i)
			if renameErr := os.Rename(path, tmpPath); renameErr == nil {
				if removeErr := os.Remove(tmpPath); removeErr == nil {
					return nil
				}
				os.Rename(tmpPath, path)
			}
		}

		backoff := time.Duration(500*(1<<uint(min(i, 6)))) * time.Millisecond // 0.5s, 1s, 2s, 4s, 8s, 16s, 32s
		if backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
		time.Sleep(backoff)
	}
	return os.Remove(path) // final attempt, return the error
}

// normalizeFMP4Timestamps remuxes an fMP4 recording to reset the timeline.
// Stripchat's LL-HLS segments carry absolute server timestamps (e.g. start at
// 5044s), which makes the file appear hours long.  A fast stream-copy remux
// with -movflags +faststart normalises the timestamps and moves the moov atom
// to the front for immediate playback.  Falls back to a re-encode if stream
// copy fails (some fragmented MP4 files can't be remuxed with -c copy).
//
// warn is an optional logger (nil-safe) used to report A/V realignment.
func normalizeFMP4Timestamps(videoPath string, warn func(string)) (string, error) {
	tmpPath := videoPath + ".normalized.mp4"
	ok := false

	// Attempt 1: fast stream-copy remux with -fflags +genpts.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		config.AcquireFFmpeg()
		defer config.ReleaseFFmpeg()
		err := config.FFmpegCommandContext(ctx,
			"-y",
			"-fflags", "+genpts",
			"-i", videoPath,
			"-c", "copy",
			"-movflags", "+faststart",
			tmpPath,
		).Run()
		if err == nil {
			if renameErr := os.Rename(tmpPath, videoPath); renameErr == nil {
				ok = true
			}
		}
	}()
	if !ok {
		os.Remove(tmpPath)

		// Attempt 2: re-encode with libx264 to force clean timestamps.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		config.AcquireFFmpeg()
		defer config.ReleaseFFmpeg()
		err := config.FFmpegCommandContext(ctx,
			"-y",
			"-i", videoPath,
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-crf", "28",
			"-c:a", "aac",
			"-movflags", "+faststart",
			tmpPath,
		).Run()
		if err != nil {
			os.Remove(tmpPath)
			return videoPath, fmt.Errorf("normalize fast+reencode both failed: %w", err)
		}
		if err := os.Rename(tmpPath, videoPath); err != nil {
			os.Remove(tmpPath)
			return videoPath, err
		}
	}

	// Remove any residual initial A/V offset so audio and video start together.
	aligned, aerr := alignAVStart(videoPath, warn)
	if aerr != nil {
		// Non-fatal — keep the normalized file as-is.
		return videoPath, nil
	}
	return aligned, nil
}

// probeStreamStartTimes returns the start_time (presentation time of the first
// packet) for the first video and first audio stream.  A stream with no
// usable start_time yields -1.
func probeStreamStartTimes(path string) (videoStart, audioStart float64, err error) {
	probeCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, perr := config.FFprobeCommandContext(probeCtx,
		"-v", "error",
		"-select_streams", "v:0,a:0",
		"-show_entries", "stream=codec_type,start_time",
		"-of", "json",
		path,
	).Output()
	if perr != nil {
		return 0, 0, perr
	}
	var p struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			StartTime string `json:"start_time"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return 0, 0, err
	}
	parse := func(s string) float64 {
		v, e := strconv.ParseFloat(s, 64)
		if e != nil {
			return -1
		}
		return v
	}
	foundV, foundA := false, false
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if !foundV {
				videoStart = parse(s.StartTime)
				foundV = true
			}
		case "audio":
			if !foundA {
				audioStart = parse(s.StartTime)
				foundA = true
			}
		}
	}
	if !foundV {
		return 0, 0, fmt.Errorf("no video stream found")
	}
	return videoStart, audioStart, nil
}

// alignAVStart removes a residual constant offset between the audio and video
// tracks so both begin at the same presentation time.
//
// Why this is needed: LL-HLS recorders (incl. this one) start the separate
// audio playlist a fraction of a second — or, on the very first poll of a live
// stream, several seconds — after the video playlist.  MuxAV keeps that real
// offset via -copyts, and the stream-copy normalize step preserves it (it only
// shifts the global earliest timestamp to zero).  The result is audio that
// lags or leads the picture by a fixed amount for the entire recording.
//
// The fix shifts the audio track (only) to start at the video's start time
// using -itsoffset, which runs at the demuxer so it is a stream-copy operation
// — no re-encode, so video quality is untouched.  Offsets smaller than the
// threshold are left alone as they are imperceptible.
func alignAVStart(path string, warn func(string)) (string, error) {
	vStart, aStart, err := probeStreamStartTimes(path)
	if err != nil {
		return path, nil // can't measure — leave as-is
	}
	if aStart < 0 {
		return path, nil // no audio stream — nothing to align
	}
	if vStart < 0 {
		return path, nil // can't establish reference — skip
	}

	offset := aStart - vStart
	const threshold = 0.05
	if math.Abs(offset) < threshold {
		return path, nil // already aligned within perception threshold
	}
	if warn != nil {
		warn(fmt.Sprintf("audio/video start offset %.3fs detected; realigning audio to video start", offset))
	}

	// Feed the file twice: input 0 is video (no shift), input 1 is audio shifted
	// by -offset so audio now begins at the video's start time.
	tmpPath := path + ".aligned.mp4"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	defer config.AcquireFFmpeg()()
	shift := -offset
	err = config.FFmpegCommandContext(ctx,
		"-y",
		"-i", path,
		"-itsoffset", fmt.Sprintf("%f", shift),
		"-i", path,
		"-map", "0:v:0?",
		"-map", "1:a:0?",
		"-c", "copy",
		"-movflags", "+faststart",
		tmpPath,
	).Run()
	if err != nil {
		os.Remove(tmpPath)
		return path, nil // best-effort — keep original
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return path, nil
	}
	return path, nil
}

// dateSeparatorRe matches the "_YYYY-MM-DD_" / "_YYYY-MM-DD-" timestamp separator
// the recorder writes between the username and the time portion.  Anchoring on
// the full date (not just "_20") is what keeps usernames that themselves contain
// "_20" (e.g. "alice_20_fan_2025-01-01_...") from being mis-split: the regex
// skips the "_20" inside the username and lands on the real date.
var dateSeparatorRe = regexp.MustCompile(`_(20\d{2}-\d{2}-\d{2})[_-]`)

// findDateSeparatorIndex returns the byte index in stem of the "_" that begins
// the "_YYYY-MM-DD_" timestamp separator, or -1 if no date separator is found.
// Both extractUsernameFromFilename and extractTimestampFromFilename use it so
// they always agree on where the username ends.
func findDateSeparatorIndex(stem string) int {
	loc := dateSeparatorRe.FindStringSubmatchIndex(stem)
	if loc == nil {
		return -1
	}
	return loc[0] // index of the leading "_"
}

// extractUsernameFromFilename parses "username_YYYY-MM-DD_HH-MM-SS.ext" to get the username.
// It locates the "_YYYY-MM-DD_" timestamp separator (not merely the substring
// "_20", which can legitimately appear inside a username such as
// "alice_20_fan_2025-01-01_...") so the username portion is split correctly.
func extractUsernameFromFilename(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))

	// Strip "merged-" prefix that the merge system prepends.
	stem := strings.TrimPrefix(base, "merged-")

	// Find the real timestamp separator (_YYYY-MM-DD_ or _YYYY-MM-DD-).
	idx := findDateSeparatorIndex(stem)
	if idx < 0 {
		return ""
	}

	candidate := stem[:idx]

	// Deduplicate: merged filenames become "<user>-<user>" via the merge
	// system.  Usernames may contain hyphens (e.g. "Awesome-sona"), so we
	// try every split point and check whether left == right.
	searchFrom := 0
	for {
		hyphen := strings.Index(candidate[searchFrom:], "-")
		if hyphen < 0 {
			break
		}
		hyphen += searchFrom
		if candidate[:hyphen] == candidate[hyphen+1:] {
			return candidate[:hyphen]
		}
		searchFrom = hyphen + 1
	}

	return candidate
}

// extractTimestampFromFilename parses the standard recording timestamp from a
// filename like "username_2025-01-01_12-00-00.mp4" and returns it in Supabase
// format ("2025-01-01T12:00:00Z").  Returns "" if the pattern is not found.
func extractTimestampFromFilename(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	idx := findDateSeparatorIndex(base)
	if idx < 0 {
		return ""
	}
	ts := base[idx+1:] // "YYYY-MM-DD_HH-MM-SS..."
	if len(ts) >= 19 && ts[4] == '-' && ts[7] == '-' && ts[10] == '_' && ts[13] == '-' && ts[16] == '-' {
		return ts[:10] + "T" + ts[11:13] + ":" + ts[14:16] + ":" + ts[17:19] + "Z"
	}
	return ""
}

// recoveryLogf logs to both stdout and the channel's SSE log stream if the
// file can be associated with an active channel via its Manager.
func recoveryLogf(filename, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	username := extractUsernameFromFilename(filename)
	log.Printf("recovery [%s]: %s", username, msg)
	if m := server.Manager; m != nil && username != "" {
		m.PublishLog(username, "[recovery] "+msg)
	}
}

// isAlreadyFullyUploaded checks the upload journal to determine if a file has
// been successfully uploaded to all configured hosts.
func IsAlreadyFullyUploaded(filePath string) bool {
	if !server.RecordingExists(filepath.Base(filePath)) {
		return false
	}
	fileHash, err := internal.FastFileHash(filePath)
	if err != nil || fileHash == "" {
		return false
	}
	completed, err := server.LoadCompletedHosts(fileHash)
	if err != nil {
		return false
	}
	// Build the set of all configured hosts
	hosts := configuredUploadHosts()
	if len(hosts) == 0 {
		return false
	}
	done := make(map[string]bool, len(completed))
	for _, h := range completed {
		done[h] = true
	}
	for _, h := range hosts {
		if !done[h] {
			return false
		}
	}
	return true
}

// configuredUploadHosts returns the list of upload hosts that have their
// API keys configured in the server config.
//
// This delegates to uploader.NewMultiHostUploader(...).AvailableHosts() so that
// the set of hosts checked by IsAlreadyFullyUploaded is always identical to the
// set the pipeline actually uploads to.  Previously this maintained a separate
// hand-written list that drifted out of sync (it omitted SeekStreaming), which
// caused the watcher to consider a file "fully uploaded" — and delete the local
// copy — before SeekStreaming had received it.
func configuredUploadHosts() []string {
	cfg := server.Config
	if cfg == nil {
		return nil
	}
	upl := uploader.NewMultiHostUploader(
		cfg.VoeSXAPIKey,
		cfg.StreamtapeLogin,
		cfg.StreamtapeKey,
		cfg.MixdropEmail,
		cfg.MixdropToken,
		cfg.SeekStreamingKey,
		cfg.VidHideAPIKeys,
		cfg.StreamWishAPIKeys,
		nil,
		cfg.UpnshareKeys,
	)
	return upl.AvailableHosts()
}

// UploadOrphanedFile uploads a file to all configured hosts and saves metadata
// to Supabase. Unlike Channel.uploadFile, this doesn't require an active channel.
// Username is extracted from the filename; metadata fields are left empty.
//
// If every configured host fails on the first attempt, it retries up to 2 more
// times with a 60-second delay between attempts.  This handles transient network
// or API outages that can occur when the app restarts after a crash.
func UploadOrphanedFile(filePath, thumbURL, spriteURL, previewURL string) bool {
	MarkUploadInFlight(filePath)
	defer MarkUploadDone(filePath)
	cfg := server.Config
	if cfg == nil {
		return false
	}

	filename := filepath.Base(filePath)
	select {
	case UploadSem <- struct{}{}:
	case <-time.After(30 * time.Second):
		recoveryLogf(filename, "upload semaphore timeout for %s — skipping", filename)
		return false
	}
	defer func() { <-UploadSem }()

	recoveryLogf(filename, "uploading %s", filename)

	// Compute file hash for upload journal
	fileHash, hashErr := internal.FastFileHash(filePath)
	if hashErr != nil {
		recoveryLogf(filename, "could not hash (journal skipped): %v", hashErr)
	}

	// Load completed hosts from journal
	var completedHosts []string
	if fileHash != "" {
		var loadErr error
		completedHosts, loadErr = server.LoadCompletedHosts(fileHash)
		if loadErr != nil {
			recoveryLogf(filename, "could not load journal: %v", loadErr)
		}
	}

	// Save preview links first
	if thumbURL != "" || spriteURL != "" || previewURL != "" {
		if err := server.SavePreviewLinks(filename, thumbURL, spriteURL, previewURL); err != nil {
			recoveryLogf(filename, "could not save preview links: %v", err)
		}
	}

	// Upload to all configured hosts — retry up to 3 times if all hosts fail.
	const maxUploadAttempts = 3
	const retryDelay = 60 * time.Second

	upl := uploader.NewMultiHostUploader(
		cfg.VoeSXAPIKey,
		cfg.StreamtapeLogin,
		cfg.StreamtapeKey,
		cfg.MixdropEmail,
		cfg.MixdropToken,
		cfg.SeekStreamingKey,
		cfg.VidHideAPIKeys,
		cfg.StreamWishAPIKeys,
		nil, // no logger for orphan recovery
		cfg.UpnshareKeys,
	)

	allHosts := upl.AvailableHosts()

	// Determine which hosts still need the file
	hostsToTry := allHosts
	if len(completedHosts) > 0 {
		hostsToTry = difference(allHosts, completedHosts)
		if len(hostsToTry) == 0 {
			if server.RecordingExists(filename) {
				recoveryLogf(filename, "all hosts already have this file per journal — skipping upload")
				return true
			}
			recoveryLogf(filename, "stale journal has no Supabase recording; clearing journal and re-uploading")
			if fileHash != "" {
				if jErr := server.DeleteJournalByHash(fileHash); jErr != nil {
					recoveryLogf(filename, "could not clear stale journal: %v", jErr)
				}
			}
			completedHosts = nil
			hostsToTry = allHosts
		}
		recoveryLogf(filename, "%d/%d hosts already have this file — uploading to %d remaining",
			len(completedHosts), len(allHosts), len(hostsToTry))
	}

	var results []uploader.UploadResult
	var success []uploader.UploadResult
	stageCtx, stageCancel := context.WithTimeout(context.Background(), uploadStageMaxDuration)
	defer stageCancel()
	for attempt := 1; attempt <= maxUploadAttempts; attempt++ {
		if attempt > 1 && len(hostsToTry) == 0 {
			break
		}
		attemptCh := make(chan []uploader.UploadResult, 1)
		go func() {
			attemptCh <- upl.UploadSelected(filePath, hostsToTry)
		}()
		var attemptResults []uploader.UploadResult
		select {
		case attemptResults = <-attemptCh:
		case <-stageCtx.Done():
			recoveryLogf(filename, "upload: stage deadline (%s) reached — aborting recovery upload", uploadStageMaxDuration)
			goto recoveryUploadDone
		}
		results = append(results, attemptResults...)

		// Save journal entries for each result
		if fileHash != "" {
			stat, _ := os.Stat(filePath)
			var filesize int64
			if stat != nil {
				filesize = stat.Size()
			}
			for _, r := range attemptResults {
				status := "success"
				errMsg := ""
				if r.Error != nil {
					status = "failed"
					errMsg = r.Error.Error()
				}
				if jErr := server.SaveJournalEntry(fileHash, filename, r.Host, status, filesize, errMsg); jErr != nil {
					recoveryLogf(filename, "could not save journal for %s: %v", r.Host, jErr)
				}
			}
		}

		success = uploader.GetSuccessfulUploads(results)
		recoveryLogf(filename, "upload attempt %d/%d — %d/%d successful", attempt, maxUploadAttempts, len(success), len(allHosts))
		if len(success) >= len(allHosts) {
			break
		}

		// Exclude hosts with permanent or proxy errors from retries
		skipHosts := completedHosts
		for _, r := range results {
			if uploader.IsPermanentError(r.Error) || uploader.IsProxyError(r.Error) {
				skipHosts = append(skipHosts, r.Host)
			}
		}

		if attempt < maxUploadAttempts {
			failedHosts := failedHostNames(results, skipHosts)
			hostsToTry = failedHosts
			if len(hostsToTry) > 0 {
				recoveryLogf(filename, "%d hosts still pending — retrying in %s...", len(hostsToTry), retryDelay)
				time.Sleep(retryDelay)
			}
		}
	}

recoveryUploadDone:

	if len(success) == 0 {
		recoveryLogf(filename, "[WARN] all upload attempts exhausted — file will be retried on next restart")
		return false
	}

	// Build links map and extract SeekStreaming poster/preview
	links := map[string]string{}
	var embedURL string
	var seekPosterURL, seekPreviewURL string
	for _, r := range success {
		links[r.Host] = r.DownloadLink
		if embedURL == "" {
			embedURL = embedURLFromLink(r.Host, r.DownloadLink)
		}
		if r.Host == "SeekStreaming" {
			if r.PosterURL != "" {
				seekPosterURL = r.PosterURL
			}
			if r.PreviewURL != "" {
				seekPreviewURL = r.PreviewURL
			}
		}
	}

	stat, _ := os.Stat(filePath)
	var filesize int64
	if stat != nil {
		filesize = stat.Size()
	}

	timestamp := extractTimestampFromFilename(filename)
	if timestamp == "" {
		timestamp = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}

	dur, probeErr := VideoDurationSeconds(filePath)
	if probeErr != nil {
		recoveryLogf(filename, "could not probe duration: %v", probeErr)
	}

	dbSaved := false
	if err := server.SaveRecordingWithLinks(
		extractUsernameFromFilename(filename), filename, timestamp,
		"", nil, 0, "", 0, filesize, dur, "", embedURL, thumbURL, spriteURL, previewURL, links,
		seekPosterURL, seekPreviewURL,
	); err != nil {
		recoveryLogf(filename, "failed to save recording to Supabase: %v", err)
		if fileHash != "" {
			if jErr := server.DeleteJournalByHash(fileHash); jErr != nil {
				recoveryLogf(filename, "could not delete journal after DB failure: %v", jErr)
			}
		}
	} else {
		dbSaved = true
		recoveryLogf(filename, "saved recording metadata")
	}

	// Delete local file only once ALL hosts have the file safely and metadata
	// is persisted. Otherwise the file remains available for retry.
	if cfg.DeleteLocalAfterUpload && len(success) > 0 && dbSaved {
		DeleteSidecarFiles(filePath)
		if err := removeFileWithRetry(filePath); err != nil {
			recoveryLogf(filename, "could not remove local file: %v — will retry on next restart", err)
			return true
		}
		if fileHash != "" {
			if jErr := server.DeleteJournalByHash(fileHash); jErr != nil {
				recoveryLogf(filename, "could not delete journal: %v", jErr)
			}
		}
		recoveryLogf(filename, "removed local file")
	}

	return true
}

// pendingSegmentsDir returns the directory where short video segments are stored
// awaiting merge with the next recording.  A subdirectory per channel keeps
// segments from different models separate.
func pendingSegmentsDir(username string) string {
	dir := "videos"
	if server.Config != nil && server.Config.OutputDir != "" {
		dir = server.Config.OutputDir
	}
	return filepath.Join(dir, ".pending", username)
}

// collectPendingSegments returns sorted absolute paths of all pending segments
// for a given channel.
func collectPendingSegments(username string) []string {
	dir := pendingSegmentsDir(username)
	return collectPendingSegmentsInDir(dir)
}

// segmentsForChannel returns the pending segments that genuinely belong to
// username. Any segment whose filename actually indicates a DIFFERENT channel
// (a file that was misrouted into this channel's pending directory) is excluded
// and best-effort relocated to its correct channel's pending dir, so two
// channels' recordings are never merged into a single file.
//
// A segment whose username CANNOT be determined (empty, e.g. a truncated,
// corrupt, or off-pattern filename) is treated as a stray and routed to the
// isolated "_unknown" bucket rather than being claimed by whatever channel
// happens to scan the directory next — claiming it would wrongly merge an
// unrelated file into the current channel's recording.
func segmentsForChannel(username string) []string {
	all := collectPendingSegments(username)
	var keep, stray []string
	for _, s := range all {
		u := extractUsernameFromFilename(filepath.Base(s))
		if u == username {
			keep = append(keep, s)
		} else {
			stray = append(stray, s)
		}
	}
	for _, s := range stray {
		correct := extractUsernameFromFilename(filepath.Base(s))
		if correct != "" && correct != username {
			_ = moveToPendingDir(s, correct)
		} else {
			// Unknown/unparseable owner — isolate so it never pollutes a
			// real channel's merge set.
			_ = moveToPendingDir(s, "_unknown")
		}
	}
	return keep
}

// collectPendingSegmentsInDir returns sorted absolute paths of actual video
// files in dir, filtering out sidecar files, zero-byte files, non-MP4
// containers (e.g. .mkv produced by compression — these must never be fed
// back into the MP4 merge path, or they would be double-merged and corrupt
// the output), and merged-*.mp4 files that were already consolidated.
func collectPendingSegmentsInDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip sidecar files — they are not video segments.
		if isSidecar(name) {
			continue
		}
		// Skip merged-* files — they were already consolidated.
		if strings.HasPrefix(name, "merged-") {
			continue
		}
		// Only MP4 segments participate in the merge path. Compression
		// emits .mkv and must not be re-fed into the MP4 concat.
		if strings.ToLower(filepath.Ext(name)) != ".mp4" {
			continue
		}
		// Skip zero-byte files — they are corrupt/empty segments.
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths
}

// deletePendingSegments removes all pending segments for a channel and cleans
// up the (now empty) directory.
func deletePendingSegments(username string) {
	dir := pendingSegmentsDir(username)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	_ = os.Remove(dir)
}

// VideoDurationSeconds probes a video file and returns its duration in seconds.
// Falls back to parsing ffmpeg stderr output when ffprobe is unavailable or fails.
func VideoDurationSeconds(videoPath string) (float64, error) {
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer probeCancel()
	defer config.AcquireFFmpeg()()
	out, err := config.FFprobeCommandContext(probeCtx,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		videoPath,
	).Output()
	if err == nil {
		dur, parseErr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		if parseErr == nil {
			return dur, nil
		}
	}

	// Fallback: ask ffmpeg to decode null and parse "Duration:" from stderr.
	fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer fallbackCancel()
	cmd := config.FFmpegCommandContext(fallbackCtx, "-i", videoPath, "-f", "null", "-")
	stderr, fbErr := cmd.CombinedOutput()
	if fbErr == nil && len(stderr) > 0 {
		line := string(stderr)
		const durationPrefix = "Duration: "
		if idx := strings.Index(line, durationPrefix); idx >= 0 {
			rest := line[idx+len(durationPrefix):]
			if end := strings.IndexAny(rest, "., "); end > 0 {
				rest = rest[:end]
			}
			parts := strings.SplitN(rest, ":", 3)
			if len(parts) == 3 {
				hours, _ := strconv.ParseFloat(parts[0], 64)
				minutes, _ := strconv.ParseFloat(parts[1], 64)
				seconds, _ := strconv.ParseFloat(parts[2], 64)
				if hours >= 0 || minutes >= 0 || seconds >= 0 {
					return hours*3600 + minutes*60 + seconds, nil
				}
			}
		}
	}

	if err != nil {
		return 0, fmt.Errorf("probe %s: %w", filepath.Base(videoPath), err)
	}
	return 0, fmt.Errorf("probe %s: could not parse duration from ffprobe or ffmpeg", filepath.Base(videoPath))
}

// mergeVideos concatenates multiple video files into a single output.
//
// Strategy (two-phase):
//  1. Fast path — concat demuxer with stream copy.  Works when all segments
//     share the same codec parameters and each (except the first) starts with
//     a keyframe.  If it succeeds AND the output contains all expected streams
//     with reasonable duration, we are done.
//  2. Fallback — concat demuxer with re-encode (libx264 + AAC).  Fixes
//     codec-parameter mismatch, missing-keyframe, and broken-timestamp issues
//     that the stream-copy path cannot handle.
//
// In both phases the output is always normalized: pending segments carry
// absolute server timestamps (PTS=5044s from LL-HLS) that would otherwise
// make the merged file unseekable and break playback after the first segment
// boundary.
func mergeVideos(inputs []string, outputPath string) error {
	if len(inputs) < 2 {
		return fmt.Errorf("need at least 2 inputs, got %d", len(inputs))
	}

	// ── Pre-flight: validate every segment ──────────────────────────────
	for _, p := range inputs {
		dur, err := VideoDurationSeconds(p)
		if err != nil || dur <= 0 {
			if err == nil {
				err = fmt.Errorf("zero duration")
			}
			return fmt.Errorf("segment %s is invalid: %w", filepath.Base(p), err)
		}
	}

	// ── Build concat list for both phases ────────────────────────────────
	listFile, err := os.CreateTemp("", "concat-*.txt")
	if err != nil {
		return fmt.Errorf("create concat list: %w", err)
	}
	defer listFile.Close()
	defer os.Remove(listFile.Name())

	for _, p := range inputs {
		abs, aErr := filepath.Abs(p)
		if aErr != nil {
			abs = p
		}
		escaped := strings.ReplaceAll(abs, "'", "'\\''")
		if _, wErr := fmt.Fprintf(listFile, "file '%s'\n", escaped); wErr != nil {
			return fmt.Errorf("write concat list: %w", wErr)
		}
	}

	// Compute total input duration for validation later.
	var totalInputDur float64
	for _, p := range inputs {
		if d, e := VideoDurationSeconds(p); e == nil {
			totalInputDur += d
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	defer config.AcquireFFmpegHeavy()()

	// ── Phase 1: Fast stream-copy concat ────────────────────────────────
	streamCopyOK := config.FFmpegCommandContext(ctx,
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile.Name(),
		"-c", "copy",
		"-fflags", "+genpts",
		"-movflags", "+faststart",
		outputPath,
	).Run()

	if streamCopyOK == nil {
		// Probe the output — check it has reasonable duration and at least a video track.
		outputDur, probeErr := VideoDurationSeconds(outputPath)
		if probeErr == nil && outputDur > 0 && (totalInputDur <= 0 || outputDur >= totalInputDur*0.5) {
			tmpPath := outputPath + ".normalized.mp4"
			if err := config.FFmpegCommandContext(ctx,
				"-y",
				"-fflags", "+genpts",
				"-i", outputPath,
				"-c", "copy",
				"-movflags", "+faststart",
				tmpPath,
			).Run(); err != nil {
				os.Remove(tmpPath)
				return nil // best-effort — return the concat output as-is
			}
			os.Remove(outputPath)
			if rErr := os.Rename(tmpPath, outputPath); rErr != nil {
				os.Remove(tmpPath)
			}
			if _, aerr := alignAVStart(outputPath, func(msg string) { log.Printf("merge: %s", msg) }); aerr != nil {
				// best-effort safety net — keep the concatenated output as-is
				_ = aerr
			}
			return nil
		}
	}

	// Stream-copy failed or produced a bad output — clean up and re-try.
	os.Remove(outputPath)

	// ── Phase 2: Re-encode concat ───────────────────────────────────────
	reEncodeArgs := []string{
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile.Name(),
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-c:a", "aac",
		"-b:a", "128k",
	}

	// Force timestamps to start at zero on every segment boundary.
	// The setpts/asetpts filters reset the timeline so there are no gaps.
	reEncodeArgs = append(reEncodeArgs,
		"-vf", "setpts=PTS-STARTPTS",
		"-af", "asetpts=PTS-STARTPTS",
		"-movflags", "+faststart",
		outputPath,
	)

	if err := config.FFmpegCommandContext(ctx, reEncodeArgs...).Run(); err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("merge re-encode: %w", err)
	}

	if _, aerr := alignAVStart(outputPath, func(msg string) { log.Printf("merge: %s", msg) }); aerr != nil {
		// best-effort safety net — keep the concatenated output as-is
		_ = aerr
	}

	return nil
}

// handleMinDurationAndMerge checks whether a finalized video file meets the
// minimum-duration threshold.  If the feature is disabled the check is skipped
// and the caller proceeds to upload normally.
//
// When a video is shorter than the threshold it is moved into a pending
// directory.  If pending segments already exist (including the one just moved),
// they are merged together.  If the merged result meets the threshold it is
// uploaded via MoveToOutputDir; otherwise it stays pending for the next
// recording to extend it.
//
// The skipDefer flag is set when the channel is stopping/pausing — i.e. there
// will be NO future recording to merge with.  In that case any accumulated
// pending segments are force-merged and uploaded regardless of the threshold so
// that short clips are never silently discarded or left orphaned in a .pending
// directory with no process left to recover them (the background watcher that
// used to re-scan pending dirs on startup has been removed).
//
// Returns true if the video was handled (deferred to pending or merged+uploaded)
// so the caller should stop processing it.  Returns false when the caller
// should proceed with its normal upload logic.
func (ch *Channel) handleMinDurationAndMerge(videoPath string, skipDefer bool) bool {
	mu := pendingMu(ch.Config.Username)
	mu.Lock()

	minDur := ch.Config.MinDurationBeforeUpload
	if minDur <= 0 {
		if server.Config != nil && server.Config.MinDurationBeforeUpload > 0 {
			minDur = server.Config.MinDurationBeforeUpload
		} else {
			mu.Unlock()
			return false // feature disabled — proceed normally
		}
	}

	dur, err := VideoDurationSeconds(videoPath)
	if err != nil {
		ch.Warn("min-duration: could not probe %s: %v — deferring to pending", filepath.Base(videoPath), err)
		// On probe failure, keep the video pending rather than uploading a
		// corrupt/short file (it will be retried by RecoverPendingSegments).
		pendingDir := pendingSegmentsDir(ch.Config.Username)
		if mErr := os.MkdirAll(pendingDir, 0777); mErr == nil {
			destPath := uniqueDestPath(filepath.Join(pendingDir, filepath.Base(videoPath)))
			if rErr := os.Rename(videoPath, destPath); rErr == nil {
				mu.Unlock()
				return true
			}
		}
		// Cannot defer and must not upload a sub-threshold/corrupt file, so drop it.
		mu.Unlock()
		ch.Error("min-duration: probe failed and cannot defer %s — dropping (no upload)", filepath.Base(videoPath))
		os.Remove(videoPath)
		return true
	}

	if dur >= float64(minDur) {
		// Video is long enough. Before uploading, check if there are
		// pending segments to merge with.
		segments := segmentsForChannel(ch.Config.Username)
		if len(segments) == 0 {
			mu.Unlock()
			return false // no pending — proceed normally
		}

		// Merge pending segments with the current video.
		// Release the lock during the potentially long ffmpeg encode.
		mergedPath := videoPath + ".merged.mp4"
		mergeInputs := make([]string, len(segments))
		copy(mergeInputs, segments)
		allInputs := append(mergeInputs, videoPath)
		ch.Info("min-duration: merging %d pending segment(s) with %s", len(mergeInputs), filepath.Base(videoPath))
		mu.Unlock()
		mErr := mergeVideos(allInputs, mergedPath)
		if mErr != nil {
			os.Remove(mergedPath) // clean up partial output
			ch.Error("min-duration: merge failed: %v — uploading current video separately, clearing pending segments", mErr)
			for _, s := range mergeInputs {
				os.Remove(s)
			}
			return false
		}

		// Release the lock while probing the merged output; we only need it
		// again to delete the consumed inputs.
		mergedDur, probeErr := VideoDurationSeconds(mergedPath)
		if probeErr != nil {
			ch.Warn("min-duration: could not probe merged output, uploading anyway: %v", probeErr)
		} else if mergedDur < float64(minDur)*0.9 {
			// Merged result is unexpectedly short (timestamp/merge glitch).
			// Don't discard the pending segments blindly — upload the current
			// long-enough video on its own and let recovery retry the pendings.
			ch.Warn("min-duration: merged output for %s is %.1fs (expected >= %ds) — uploading current video separately, keeping pending for recovery",
				filepath.Base(mergedPath), mergedDur, minDur)
			os.Remove(mergedPath)
			return false
		}

		mu.Lock()
		for _, s := range mergeInputs {
			os.Remove(s)
		}
		_ = os.Remove(videoPath)
		mu.Unlock()

		if ch.Config.Compress {
			ch.Info("min-duration: merged -> %s (%.1fs), compressing before upload", filepath.Base(mergedPath), mergedDur)
			ch.CompressFile(mergedPath)
		} else {
			ch.Info("min-duration: merged -> %s (%.1fs), proceeding with upload", filepath.Base(mergedPath), mergedDur)
			ch.MoveToOutputDir(mergedPath)
		}
		return true
	}

	// Video is too short.
	if skipDefer {
		// Channel is stopping/pausing — no future recordings to extend this.
		// Force-merge with any pending segments; only upload if the merged
		// result meets the min-duration threshold.  If nothing to merge, or
		// the merged result is still too short, delete everything — a clip
		// below the useful threshold should never be uploaded.
		segments := segmentsForChannel(ch.Config.Username)
		if len(segments) == 0 {
			ch.Info("min-duration: %s is %.1fs (< %ds) on stop — no pending segments to merge, deleting (no data loss)",
				filepath.Base(videoPath), dur, minDur)
			mu.Unlock()
			os.Remove(videoPath)
			return true
		}

		mergeInputs := make([]string, len(segments))
		copy(mergeInputs, segments)
		allInputs := append(mergeInputs, videoPath)
		mergedPath := videoPath + ".merged.mp4"
		ch.Info("min-duration: %s is %.1fs (< %ds) on stop — force-merging %d pending segment(s)",
			filepath.Base(videoPath), dur, minDur, len(mergeInputs))
		mu.Unlock()

		mErr := mergeVideos(allInputs, mergedPath)
		if mErr != nil {
			os.Remove(mergedPath)
			ch.Error("min-duration: force-merge failed: %v — removing all segments (no data loss)", mErr)
			for _, s := range mergeInputs {
				os.Remove(s)
			}
			os.Remove(videoPath)
			return true
		}

		// Check merged duration against threshold before uploading.
		mergedDur, probeErr := VideoDurationSeconds(mergedPath)
		if probeErr != nil || mergedDur < float64(minDur) {
			if probeErr != nil {
				ch.Warn("min-duration: could not probe merged output %s (%v) — uploading anyway",
					filepath.Base(mergedPath), probeErr)
				// Can't confirm threshold is met; fall through to upload.
			} else {
				ch.Info("min-duration: merged result is %.1fs (< %ds) on stop — still too short, deleting (no data loss)",
					mergedDur, minDur)
				os.Remove(mergedPath)
				for _, s := range mergeInputs {
					os.Remove(s)
				}
				os.Remove(videoPath)
				return true
			}
		}

		mu.Lock()
		for _, s := range mergeInputs {
			os.Remove(s)
		}
		_ = os.Remove(videoPath)
		mu.Unlock()

		ch.Info("min-duration: merged -> %s (%.1fs >= %ds) on stop, uploading",
			filepath.Base(mergedPath), mergedDur, minDur)
		if ch.Config.Compress {
			ch.CompressFile(mergedPath)
		} else {
			ch.MoveToOutputDir(mergedPath)
		}
		return true
	}

	// Video is too short and the channel is still live — defer to pending so
	// the next recording can extend it past the threshold.
	pendingDir := pendingSegmentsDir(ch.Config.Username)
	if err := os.MkdirAll(pendingDir, 0777); err != nil {
		mu.Unlock()
		// Cannot defer, and we must NOT upload a sub-threshold video, so drop it.
		ch.Error("min-duration: cannot create pending dir %s: %v — dropping short video (no upload)", pendingDir, err)
		os.Remove(videoPath)
		return true
	}

	destPath := uniqueDestPath(filepath.Join(pendingDir, filepath.Base(videoPath)))
	if err := os.Rename(videoPath, destPath); err != nil {
		mu.Unlock()
		// Cannot defer, so drop rather than let it fall through to upload.
		ch.Error("min-duration: cannot move %s to pending: %v — dropping short video (no upload)", filepath.Base(videoPath), err)
		os.Remove(videoPath)
		return true
	}
	ch.Info("min-duration: %s is %.1fs (< %ds) — deferred to pending", filepath.Base(videoPath), dur, minDur)

	// If multiple segments have now accumulated, merge them and check the
	// combined duration. Only upload if the merged result meets the threshold.
	// Use segmentsForChannel (not collectPendingSegments) so a segment that
	// belongs to a DIFFERENT channel is never pulled into this merge.
	segments := segmentsForChannel(ch.Config.Username)
	if len(segments) > 1 {
		mergedPath := uniqueDestPath(filepath.Join(pendingDir, "merged-"+filepath.Base(destPath)))
		mergeInputs := make([]string, len(segments))
		copy(mergeInputs, segments)
		ch.Info("min-duration: merging %d pending segment(s)", len(mergeInputs))
		mu.Unlock()
		mErr := mergeVideos(mergeInputs, mergedPath)
		if mErr != nil {
			os.Remove(mergedPath) // clean up partial output
			ch.Error("min-duration: merge failed: %v — segments remain pending for next recording", mErr)
			return true
		}
		mu.Lock()

		mergedDur, mErr := VideoDurationSeconds(mergedPath)
		if mErr != nil {
			ch.Warn("min-duration: could not probe merged result, uploading anyway: %v", mErr)
			for _, s := range mergeInputs {
				os.Remove(s)
			}
			mu.Unlock()
			ch.MoveToOutputDir(mergedPath)
			return true
		}

		if mergedDur >= float64(minDur) {
			for _, s := range mergeInputs {
				os.Remove(s)
			}
			ch.Info("min-duration: merged %d segments = %.1fs (>= %ds) — uploading", len(mergeInputs), mergedDur, minDur)
			mu.Unlock()

			if ch.Config.Compress {
				ch.CompressFile(mergedPath)
			} else {
				ch.MoveToOutputDir(mergedPath)
			}
		} else {
			ch.Info("min-duration: merged %d segments = %.1fs (< %ds) — still pending", len(mergeInputs), mergedDur, minDur)
			for _, s := range mergeInputs {
				os.Remove(s)
			}
			// Keep the merged result pending (with a unique name) so the next
			// recording can extend it further.
			mergedDest := uniqueDestPath(filepath.Join(pendingDir, "merged-"+filepath.Base(destPath)))
			if mErr := os.Rename(mergedPath, mergedDest); mErr != nil {
				mu.Unlock()
				ch.Error("min-duration: cannot keep merged result pending: %v — dropping (no upload)", mErr)
				os.Remove(mergedPath)
				return true
			}
			mu.Unlock()
		}
	} else {
		mu.Unlock()
	}

	return true // video was deferred to pending (or merged+uploaded)
}

// RecoverPendingSegments processes any segments left in the channel's .pending
// directory from a previous run (or a session that ended with unmerged clips).
// With the background fsnotify watcher removed, nothing else re-scans these
// directories, so without this recovery short deferred segments would be
// orphaned forever and never uploaded.
//
// Stray segments owned by a different channel are relocated to their owner's
// pending dir; unparseable files go to _unknown.  The channel's own segments
// are force-merged and uploaded regardless of the minimum-duration threshold —
// there is no future recording to extend them, so data must not be lost.  A
// leftover below-threshold merged file is also uploaded so it is never stranded.
func (ch *Channel) RecoverPendingSegments() {
	user := ch.Config.Username
	own := segmentsForChannel(user)
	pendingDir := pendingSegmentsDir(user)

	// Check any stale merged-* files that were below threshold on a previous
	// run (they can never grow now and would otherwise be stranded forever).
	// Only upload if they meet the minimum-duration threshold.
	minDur := ch.Config.MinDurationBeforeUpload
	if minDur <= 0 && server.Config != nil && server.Config.MinDurationBeforeUpload > 0 {
		minDur = server.Config.MinDurationBeforeUpload
	}
	staleMerged := collectPendingSegmentsInDir(pendingDir)
	for _, m := range staleMerged {
		if !strings.HasPrefix(filepath.Base(m), "merged-") {
			continue
		}
		if minDur > 0 {
			dur, dErr := VideoDurationSeconds(m)
			if dErr == nil && dur < float64(minDur) {
				ch.Info("recovery: stale merged segment %s is %.1fs (< %ds) — too short, removing",
					filepath.Base(m), dur, minDur)
				os.Remove(m)
				continue
			}
		}
		ch.Info("recovery: uploading stale merged segment %s", filepath.Base(m))
		ch.MoveToOutputDir(m)
	}

	if len(own) == 0 {
		return
	}

	ch.Info("recovery: %d pending segment(s) found for %s — force-merging", len(own), user)
	mergedPath := filepath.Join(pendingDir, "merged-recovered-"+user+".mp4")
	if mErr := mergeVideos(own, mergedPath); mErr != nil {
		ch.Error("recovery: merge failed (%v) — removing %d orphaned segment(s) (no recovery upload)", mErr, len(own))
		os.Remove(mergedPath)
		for _, s := range own {
			os.Remove(s)
		}
		deletePendingSegments(user)
		return
	}

	// Check merged duration against minimum-duration threshold before uploading.
	mergedDur, probeErr := VideoDurationSeconds(mergedPath)
	if probeErr == nil && minDur > 0 && mergedDur < float64(minDur) {
		ch.Info("recovery: merged result is %.1fs (< %ds) — still too short, removing (no data loss)", mergedDur, minDur)
		os.Remove(mergedPath)
		deletePendingSegments(user)
		return
	}

	for _, s := range own {
		os.Remove(s)
	}
	ch.Info("recovery: merged -> %s (%.1fs), uploading", filepath.Base(mergedPath), mergedDur)
	if ch.Config.Compress {
		ch.CompressFile(mergedPath)
	} else {
		ch.MoveToOutputDir(mergedPath)
	}
}

// ShouldSwitchFile determines whether a new file should be created.
func (ch *Channel) ShouldSwitchFile() bool {
	maxFilesizeBytes := ch.Config.MaxFilesize * 1024 * 1024
	maxDurationSeconds := ch.Config.MaxDuration * 60

	ch.stateMu.Lock()
	dur := ch.Duration
	fsize := ch.Filesize
	ch.stateMu.Unlock()

	return (dur >= float64(maxDurationSeconds) && ch.Config.MaxDuration > 0) ||
		(fsize >= maxFilesizeBytes && ch.Config.MaxFilesize > 0)
}
