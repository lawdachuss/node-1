package channel

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/HeapOfChaos/goondvr/chaturbate"
	"github.com/HeapOfChaos/goondvr/server"
	"github.com/HeapOfChaos/goondvr/supabase"
	"github.com/HeapOfChaos/goondvr/uploader"
)

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Pattern holds the date/time and sequence information for the filename pattern
type Pattern struct {
	Username string
	Site     string
	Year     string
	Month    string
	Day      string
	Hour     string
	Minute   string
	Second   string
	Sequence int
}

// NextFile prepares the next file to be created, by cleaning up the last file and generating a new one.
// ext is the file extension to use (e.g. ".ts" or ".mp4").
func (ch *Channel) NextFile(ext string) error {
	ch.fileMu.Lock()
	defer ch.fileMu.Unlock()

	if err := ch.cleanupLocked(); err != nil {
		return err
	}
	filename, err := ch.generateFilenameLocked()
	if err != nil {
		return err
	}
	if err := ch.createNewFileLocked(filename, ext); err != nil {
		return err
	}

	// Increment the sequence number for the next file
	ch.Sequence++
	return nil
}

// Cleanup cleans the file and resets it, called when the stream errors out or before next file was created.
func (ch *Channel) Cleanup() error {
	ch.fileMu.Lock()
	defer ch.fileMu.Unlock()

	return ch.cleanupLocked()
}

func (ch *Channel) cleanupLocked() error {
	if ch.File == nil {
		return nil
	}
	filename := ch.File.Name()

	// Capture values before reset so finalization has accurate data
	capturedDuration := ch.Duration
	capturedFilesize := ch.Filesize

	defer func() {
		ch.Filesize = 0
		ch.Duration = 0
	}()

	// Sync the file to ensure data is written to disk
	if err := ch.File.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("sync file: %w", err)
	}
	if err := ch.File.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("close file: %w", err)
	}
	ch.File = nil

	// Delete the empty file
	fileInfo, err := os.Stat(filename)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat file delete zero file: %w", err)
	}
	if fileInfo != nil && fileInfo.Size() == 0 {
		if err := os.Remove(filename); err != nil {
			return fmt.Errorf("remove zero file: %w", err)
		}
		go ch.ScanTotalDiskUsage()
	} else if fileInfo != nil {
		ch.startFinalization()
		// Process in background - don't block recording
		go ch.finalizeRecordingAsync(filename, capturedDuration, capturedFilesize)
	}

	return nil
}

// GenerateFilename creates a filename based on the configured pattern and the current timestamp
func (ch *Channel) GenerateFilename() (string, error) {
	ch.fileMu.RLock()
	defer ch.fileMu.RUnlock()

	return ch.generateFilenameLocked()
}

func (ch *Channel) generateFilenameLocked() (string, error) {
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
		Site:     ch.Config.Site,
		Sequence: ch.Sequence,
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
	filename := buf.String()

	// Smart strategy: when uploads are disabled and a completed dir is configured,
	// write the recording directly into the completed directory so no post-recording
	// move (or cross-device copy) is ever needed. The file is written once, in-place.
	if !server.Config.EnableGoFileUpload {
		completedDir := completedDirForChannel(ch)
		if completedDir != "" {
			recordingRoot := recordingDirFromPattern(ch.Config.Pattern)
			filename = redirectToCompletedDir(filename, recordingRoot, completedDir)
		}
	}

	return filename, nil
}

// redirectToCompletedDir rewrites a recording path so it lands directly inside
// completedDir, preserving any sub-directory structure relative to recordingRoot.
// Example: "videos/alice_2026-05-02_12-00-00" with root "videos" and
// completedDir "videos/completed" -> "videos/completed/alice_2026-05-02_12-00-00".
func redirectToCompletedDir(filename, recordingRoot, completedDir string) string {
	cleanRoot := filepath.Clean(recordingRoot)
	cleanCompleted := filepath.Clean(completedDir)

	// Avoid double-nesting if the file is already inside completedDir.
	if strings.HasPrefix(filepath.Clean(filename)+string(os.PathSeparator), cleanCompleted+string(os.PathSeparator)) ||
		filepath.Clean(filename) == cleanCompleted {
		return filename
	}

	// Strip the recording root prefix to get the relative path (dir + basename).
	rel, err := filepath.Rel(cleanRoot, filepath.Dir(filename))
	if err != nil || strings.HasPrefix(rel, "..") {
		// Can't compute a clean relative path; just put the file directly in completedDir.
		return filepath.Join(completedDir, filepath.Base(filename))
	}

	if rel == "." {
		return filepath.Join(completedDir, filepath.Base(filename))
	}
	return filepath.Join(completedDir, rel, filepath.Base(filename))
}

// CreateNewFile creates a new file for the channel using the given filename and extension.
func (ch *Channel) CreateNewFile(filename, ext string) error {
	ch.fileMu.Lock()
	defer ch.fileMu.Unlock()

	return ch.createNewFileLocked(filename, ext)
}

func (ch *Channel) createNewFileLocked(filename, ext string) error {

	// Ensure the directory exists before creating the file
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return fmt.Errorf("mkdir all: %w", err)
	}

	// Open the file in append mode, create it if it doesn't exist
	file, err := os.OpenFile(filename+ext, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("cannot open file: %s: %w", filename, err)
	}

	ch.File = file
	return nil
}

// recordingDirFromPattern extracts the base directory from a filename pattern
// like "videos/{{.Username}}_..." → "videos".
func recordingDirFromPattern(pattern string) string {
	idx := strings.Index(pattern, "{{")
	if idx == -1 {
		return "."
	}
	dir := filepath.Dir(pattern[:idx])
	if dir == "" || dir == "." {
		return "."
	}
	return dir
}

func completedDirForChannel(ch *Channel) string {
	if server.Config.CompletedDir != "" {
		return server.Config.CompletedDir
	}
	return filepath.Join(recordingDirFromPattern(ch.Config.Pattern), "completed")
}

func finalOutputExt(filename string) string {
	if server.Config.FFmpegContainer == "mkv" {
		return ".mkv"
	}
	if server.Config.FinalizeMode == "none" {
		return filepath.Ext(filename)
	}
	return ".mp4"
}

func finalOutputPath(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	return base + finalOutputExt(filename)
}

// ScanTotalDiskUsage calculates the total bytes of all recordings for this channel
// by walking the recording directory for files whose name starts with the username.
// The result is stored in TotalDiskUsageBytes.
func (ch *Channel) ScanTotalDiskUsage() {
	recordingDir := filepath.Clean(recordingDirFromPattern(ch.Config.Pattern))
	dirs := []string{recordingDir}
	completedDir := completedDirForChannel(ch)
	cleanCompletedDir := filepath.Clean(completedDir)
	if cleanCompletedDir != "" &&
		cleanCompletedDir != recordingDir &&
		!strings.HasPrefix(cleanCompletedDir+string(os.PathSeparator), recordingDir+string(os.PathSeparator)) {
		dirs = append(dirs, completedDir)
	}
	prefix := ch.Config.Username
	var total int64
	for _, dir := range dirs {
		// Check if directory exists before walking
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// Skip directories we can't access
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasPrefix(filepath.Base(path), prefix) {
				if info, err2 := d.Info(); err2 == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	ch.fileMu.Lock()
	ch.TotalDiskUsageBytes = total
	ch.fileMu.Unlock()
}

// ShouldSwitchFile determines whether a new file should be created.
func (ch *Channel) ShouldSwitchFile() bool {
	ch.fileMu.RLock()
	defer ch.fileMu.RUnlock()

	return ch.shouldSwitchFileLocked()
}

func (ch *Channel) shouldSwitchFileLocked() bool {
	maxFilesizeBytes := int64(ch.Config.MaxFilesize) * 1024 * 1024
	maxDurationSeconds := ch.Config.MaxDuration * 60

	return (ch.Duration >= float64(maxDurationSeconds) && ch.Config.MaxDuration > 0) ||
		(ch.Filesize >= maxFilesizeBytes && ch.Config.MaxFilesize > 0)
}

// isMP4InitSegment reports whether b looks like an fMP4 init segment containing
// top-level ftyp/moov boxes and no media fragments yet.
func isMP4InitSegment(b []byte) bool {
	var hasFtyp bool
	var hasMoov bool

	for pos := 0; pos+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[pos:]))
		if size < 8 || pos+size > len(b) {
			return false
		}

		switch string(b[pos+4 : pos+8]) {
		case "ftyp":
			hasFtyp = true
		case "moov":
			hasMoov = true
		case "moof", "mdat", "mfra":
			return false
		}
		pos += size
	}

	return hasFtyp && hasMoov
}

// finalizeRecordingAsync processes a completed recording file in the background
// This allows recording to continue while conversion and upload happen in parallel
func (ch *Channel) finalizeRecordingAsync(filename string, recordedDuration float64, recordedFilesize int64) {
	defer ch.finishFinalization()
	
	ch.Info("🎬 starting background processing for `%s`", filepath.Base(filename))

	finalPath := filename
	
	// Step 1: Convert/remux if needed (runs in parallel with recording)
	if server.Config.FinalizeMode != "none" {
		inputExt := filepath.Ext(filename)
		outputExt := finalOutputExt(filename)
		ch.Info("🔄 converting `%s` (%s) to %s format using %s mode...", filepath.Base(filename), inputExt, outputExt, server.Config.FinalizeMode)
		processedPath, err := ch.runFFmpegFinalizer(filename)
		if err != nil {
			ch.Error("❌ ffmpeg %s failed for `%s`: %s", server.Config.FinalizeMode, filename, err.Error())
			ch.Error("❌ CRITICAL: conversion failed - will NOT upload .ts file")
			ch.Info("keeping original recording locally because finalization failed")
			// Move to completed dir but don't upload
			completedDir := completedDirForChannel(ch)
			if completedDir != "" {
				cleanCompleted := filepath.Clean(completedDir)
				cleanFinal := filepath.Clean(filepath.Dir(filename))
				if cleanFinal != cleanCompleted {
					dst, err := moveRecordingToDir(filename, recordingDirFromPattern(ch.Config.Pattern), completedDir)
					if err != nil {
						ch.Error("move failed recording `%s`: %s", filename, err.Error())
					} else {
						ch.Info("failed recording moved to `%s`", dst)
					}
				}
			}
			go ch.ScanTotalDiskUsage()
			return // Don't upload unconverted .ts files
		} else {
			ch.Info("✅ conversion complete: `%s` (%s)", filepath.Base(processedPath), filepath.Ext(processedPath))
			if processedPath != filename {
				if err := os.Remove(filename); err != nil {
					ch.Error("remove original after ffmpeg finalization `%s`: %s", filename, err.Error())
				} else {
					ch.Info("✅ removed original %s file", inputExt)
				}
			}
			finalPath = processedPath
		}
	} else if strings.HasSuffix(filename, ".mp4") {
		ch.Info("building seek index for mp4 file...")
		if err := chaturbate.BuildSeekIndex(filename); err != nil {
			ch.Error("seek index %s: %v", filename, err)
		}
	}
	
	// Step 1.5: Validate file before upload (ensure it's .mp4 or .mkv, not .ts)
	finalExt := strings.ToLower(filepath.Ext(finalPath))
	if finalExt != ".mp4" && finalExt != ".mkv" {
		ch.Error("❌ CRITICAL: final file has invalid extension %s (expected .mp4 or .mkv)", finalExt)
		ch.Error("❌ refusing to upload unconverted file - this indicates FFmpeg conversion failed silently")
		ch.Info("keeping file locally: `%s`", filepath.Base(finalPath))
		// Move to completed dir but don't upload
		completedDir := completedDirForChannel(ch)
		if completedDir != "" {
			cleanCompleted := filepath.Clean(completedDir)
			cleanFinal := filepath.Clean(filepath.Dir(finalPath))
			if cleanFinal != cleanCompleted {
				dst, err := moveRecordingToDir(finalPath, recordingDirFromPattern(ch.Config.Pattern), completedDir)
				if err != nil {
					ch.Error("move invalid file `%s`: %s", finalPath, err.Error())
				} else {
					ch.Info("invalid file moved to `%s`", dst)
				}
			}
		}
		go ch.ScanTotalDiskUsage()
		return
	}

	// Step 2: Upload to multiple hosts if enabled (runs in parallel with recording)
	if server.Config.EnableGoFileUpload {
		fileInfo, _ := os.Stat(finalPath)
		fileSizeMB := float64(fileInfo.Size()) / (1024 * 1024)
		ch.Info("📤 uploading `%s` (%.2f MB) to multiple hosts...", filepath.Base(finalPath), fileSizeMB)
		
		// Log API key status for debugging
		ch.Info("🔑 API key status:")
		ch.Info("  • GoFile: always enabled (no key required)")
		if server.Config.TurboViPlayAPIKey != "" {
			ch.Info("  • TurboViPlay: ✓ configured (key: %s...)", server.Config.TurboViPlayAPIKey[:min(10, len(server.Config.TurboViPlayAPIKey))])
		} else {
			ch.Info("  • TurboViPlay: ✗ not configured")
		}
		if server.Config.VoeSXAPIKey != "" {
			ch.Info("  • VOE.sx: ✓ configured (key: %s...)", server.Config.VoeSXAPIKey[:min(10, len(server.Config.VoeSXAPIKey))])
		} else {
			ch.Info("  • VOE.sx: ✗ not configured")
		}
		if server.Config.StreamtapeLogin != "" && server.Config.StreamtapeAPIKey != "" {
			ch.Info("  • Streamtape: ✓ configured (login: %s)", server.Config.StreamtapeLogin)
		} else {
			ch.Info("  • Streamtape: ✗ not configured")
		}
		
		// Create multi-host uploader with API keys (GoFile + TurboViPlay + VOE.sx + Streamtape)
		multiUploader := uploader.NewMultiHostUploader(
			server.Config.TurboViPlayAPIKey,
			server.Config.VoeSXAPIKey,
			server.Config.StreamtapeLogin,
			server.Config.StreamtapeAPIKey,
		)
		
		// Upload to all hosts in parallel
		ch.Info("🚀 starting parallel uploads to all configured hosts...")
		results := multiUploader.UploadToAll(finalPath)
		
		// Get successful uploads
		successfulUploads := uploader.GetSuccessfulUploads(results)
		
		if len(successfulUploads) == 0 {
			ch.Error("❌ all uploads failed for `%s`", finalPath)
			for _, result := range results {
				ch.Error("  ✗ %s: %v", result.Host, result.Error)
			}
			ch.Info("keeping local file because all uploads failed")
		} else {
			// Log results
			ch.Info("✅ upload completed: %d/%d hosts successful", len(successfulUploads), len(results))
			for _, result := range results {
				if result.Error == nil {
					ch.Info("  ✓ %s: %s", result.Host, result.DownloadLink)
				} else {
					ch.Error("  ✗ %s: %v", result.Host, result.Error)
				}
			}
			
			// Step 2.5: Generate and upload thumbnail to ImgBB
			var thumbnailLink string
			ch.Info("📸 generating thumbnail for `%s`...", filepath.Base(finalPath))
			tempThumbPath, err := generateThumbnail(finalPath)
			if err != nil {
				ch.Error("❌ thumbnail generation failed: %s", err.Error())
			} else {
				// Upload thumbnail to ImgBB (free image hosting with API)
				ch.Info("uploading thumbnail to ImgBB...")
				if server.Config.ImgBBAPIKey == "" {
					ch.Error("❌ ImgBB API key not configured, skipping thumbnail upload")
					os.Remove(tempThumbPath)
				} else {
					thumbnailUploader := uploader.NewThumbnailUploader(server.Config.ImgBBAPIKey)
					uploadedURL, err := thumbnailUploader.Upload(tempThumbPath)
					if err != nil {
						ch.Error("❌ thumbnail upload to ImgBB failed: %s", err.Error())
						// Clean up temp file
						os.Remove(tempThumbPath)
					} else {
						ch.Info("✓ thumbnail uploaded to ImgBB: %s", uploadedURL)
						thumbnailLink = uploadedURL
						// Clean up temp file after successful upload
						os.Remove(tempThumbPath)
					}
				}
			}
			
			// Extract links by host for database logging
			var gofileLink, turboviplayLink, voesxLink, streamtapeLink string
			for _, result := range successfulUploads {
				switch result.Host {
				case "GoFile":
					gofileLink = result.DownloadLink
				case "TurboViPlay":
					turboviplayLink = result.DownloadLink
				case "VOE.sx":
					voesxLink = result.DownloadLink
				case "Streamtape":
					streamtapeLink = result.DownloadLink
				}
			}

			// Store all successful upload links in database
			// Only create database folders and log after successful upload
			// Store in GitHub Actions database (JSON files) - creates folders only on success
			ch.Info("💾 logging upload to GitHub Actions database...")
			if err := ch.logUploadToDatabase(finalPath, gofileLink, turboviplayLink, voesxLink, streamtapeLink, thumbnailLink, recordedDuration, recordedFilesize); err != nil {
				ch.Error("❌ failed to log upload to GitHub Actions database: %s", err.Error())
			} else {
				ch.Info("✓ upload logged to GitHub Actions database")
			}
			
			// Store upload records in Supabase when enabled (independent of EnableSupabase flag)
			// The flag controls whether uploads happen, but if we have credentials, we should use them
			if server.Config.SupabaseURL != "" && server.Config.SupabaseAPIKey != "" {
				ch.Info("💾 storing upload record in Supabase...")
				supabaseClient := supabase.NewClient(server.Config.SupabaseURL, server.Config.SupabaseAPIKey)
				
				// Store single record with all links
				if err := supabaseClient.InsertMultiHostUploadRecord(
					ch.Config.Username,
					filepath.Base(finalPath),
					gofileLink,
					turboviplayLink,
					voesxLink,
					streamtapeLink,
					thumbnailLink,
				); err != nil {
					ch.Error("❌ failed to store multi-host upload record in Supabase: %s", err.Error())
				} else {
					ch.Info("✓ multi-host upload record stored in Supabase (%d hosts)", len(successfulUploads))
				}
			} else {
				ch.Info("⚠️  Supabase credentials not configured, skipping Supabase upload")
			}
			
			// Step 3: Delete local file only after successful upload and database logging
			ch.Info("🗑️  deleting local file `%s`...", filepath.Base(finalPath))
			if err := os.Remove(finalPath); err != nil {
				ch.Error("❌ failed to delete local file `%s`: %s", finalPath, err.Error())
			} else {
				ch.Info("✓ local file deleted successfully")
			}
			
			go ch.ScanTotalDiskUsage()
			return
		}
	}

	// Smart strategy: file was written directly into completedDir, no move needed.
	// Only fall back to moveRecordingToDir if the file ended up outside completedDir
	// (e.g. upload was toggled mid-session or completedDir changed).
	completedDir := completedDirForChannel(ch)
	if completedDir != "" {
		cleanCompleted := filepath.Clean(completedDir)
		cleanFinal := filepath.Clean(filepath.Dir(finalPath))
		if cleanFinal != cleanCompleted {
			dst, err := moveRecordingToDir(finalPath, recordingDirFromPattern(ch.Config.Pattern), completedDir)
			if err != nil {
				ch.Error("move completed recording `%s`: %s", finalPath, err.Error())
			} else {
				ch.Info("completed recording moved to `%s`", dst)
			}
		} else {
			ch.Info("recording already in completed dir: `%s`", filepath.Base(finalPath))
		}
	}

	go ch.ScanTotalDiskUsage()
}

func moveRecordingToDir(src, recordingRoot, completedDir string) (string, error) {
	dstDir := completedDir

	srcDir := filepath.Dir(src)
	cleanRoot := filepath.Clean(recordingRoot)
	cleanSrcDir := filepath.Clean(srcDir)
	if relDir, err := filepath.Rel(cleanRoot, cleanSrcDir); err == nil && relDir != ".." && !strings.HasPrefix(relDir, ".."+string(os.PathSeparator)) {
		if relDir != "." {
			dstDir = filepath.Join(completedDir, relDir)
		}
	}

	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir completed dir: %w", err)
	}

	dst := filepath.Join(dstDir, filepath.Base(src))
	if src == dst {
		return dst, nil
	}

	if err := os.Rename(src, dst); err == nil {
		return dst, nil
	} else if !isCrossDeviceRename(err) {
		return "", fmt.Errorf("rename completed file: %w", err)
	}

	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return "", fmt.Errorf("remove source after copy: %w", err)
	}
	return dst, nil
}

func isCrossDeviceRename(err error) bool {
	linkErr := &os.LinkError{}
	return errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return fmt.Errorf("create destination file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy file: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync destination file: %w", err)
	}
	return nil
}

func (ch *Channel) runFFmpegFinalizer(filename string) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg not found in PATH")
	}

	outExt := finalOutputExt(filename)
	finalPath := finalOutputPath(filename)
	tempOutput := strings.TrimSuffix(filename, filepath.Ext(filename)) + ".finalizing" + outExt
	_ = os.Remove(tempOutput)

	args := []string{"-nostdin", "-y", "-i", filename}
	switch server.Config.FinalizeMode {
	case "remux":
		// Fix negative/discontinuous timestamps common in HLS .ts recordings
		args = append(args, "-avoid_negative_ts", "make_zero")
		args = append(args, "-c", "copy")
		if outExt == ".mp4" {
			// +faststart moves moov atom to front for instant web playback
			args = append(args, "-movflags", "+faststart")
		}
	case "transcode":
		// Fix negative/discontinuous timestamps common in HLS .ts recordings
		args = append(args, "-avoid_negative_ts", "make_zero")
		encoder := strings.TrimSpace(server.Config.FFmpegEncoder)
		if encoder == "" {
			encoder = "libx264"
		}
		args = append(args, "-c:v", encoder)
		args = append(args, qualityArgsForEncoder(encoder, server.Config.FFmpegQuality)...)
		if preset := strings.TrimSpace(server.Config.FFmpegPreset); preset != "" {
			args = append(args, "-preset", preset)
		}
		// Re-encode audio to AAC for maximum compatibility; copy if already AAC
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ar", "48000")
		if outExt == ".mp4" {
			// +faststart moves moov atom to front for instant web playback
			args = append(args, "-movflags", "+faststart")
		}
	default:
		return "", fmt.Errorf("unsupported finalization mode %q", server.Config.FinalizeMode)
	}
	args = append(args, tempOutput)

	ch.Info("running ffmpeg %s for `%s`", server.Config.FinalizeMode, filepath.Base(filename))
	cmd := exec.Command("ffmpeg", args...)
	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tempOutput)
		msg := strings.TrimSpace(string(outputBytes))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	if finalPath == filename {
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			_ = os.Remove(tempOutput)
			return "", fmt.Errorf("remove original before replace: %w", err)
		}
	}
	if err := os.Rename(tempOutput, finalPath); err != nil {
		_ = os.Remove(tempOutput)
		return "", fmt.Errorf("rename finalized output: %w", err)
	}
	return finalPath, nil
}

func qualityArgsForEncoder(encoder string, quality int) []string {
	if quality <= 0 {
		// CRF 23 is the libx264 default; good balance of quality vs size for streaming content.
		// Lower = better quality but larger file. Range: 18 (near-lossless) to 28 (acceptable).
		quality = 23
	}
	lower := strings.ToLower(strings.TrimSpace(encoder))
	switch {
	case strings.Contains(lower, "nvenc"):
		return []string{"-cq", fmt.Sprintf("%d", quality)}
	case strings.Contains(lower, "qsv"), strings.Contains(lower, "vaapi"), strings.Contains(lower, "amf"):
		return []string{"-global_quality", fmt.Sprintf("%d", quality)}
	default:
		return []string{"-crf", fmt.Sprintf("%d", quality)}
	}
}

// logUploadToDatabase stores upload record in local JSON database (GitHub Actions compatible)
// This function only creates folders and files after a successful GoFile upload
func (ch *Channel) logUploadToDatabase(filePath, gofileLink, turboviplayLink, voesxLink, streamtapeLink, thumbnailLink string, recordedDuration float64, recordedFilesize int64) error {
	// Validate inputs before creating any folders
	if gofileLink == "" && turboviplayLink == "" && voesxLink == "" && streamtapeLink == "" {
		return fmt.Errorf("all upload links are empty, cannot log to database")
	}
	
	// Get file info before creating database structure
	fileInfo, err := os.Stat(filePath)
	var fileSize int64
	if err == nil {
		fileSize = fileInfo.Size()
	} else {
		// File might have been deleted already, use captured value
		fileSize = recordedFilesize
	}
	
	// Create database directory structure ONLY after validating upload success
	// Structure: database/<username>/<date>/
	currentDate := time.Now().UTC().Format("2006-01-02")
	dbDir := filepath.Join("database", ch.Config.Username, currentDate)
	
	// Create directory atomically
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	
	recordsFile := filepath.Join(dbDir, "recordings.json")
	
	// Create new record with all metadata
	record := map[string]interface{}{
		"id":             fmt.Sprintf("%s_%d_%d", ch.Config.Username, time.Now().Unix(), time.Now().Nanosecond()),
		"username":       ch.Config.Username,
		"site":           ch.Config.Site,
		"filename":       filepath.Base(filePath),
		"gofile_link":       gofileLink,
		"turboviplay_link":  turboviplayLink,
		"voesx_link":        voesxLink,
		"streamtape_link":   streamtapeLink,
		"thumbnail_link":    thumbnailLink,
		"uploaded_at":       time.Now().UTC().Format(time.RFC3339),
		"filesize_bytes":    fileSize,
		"status":            "uploaded",
		"duration_seconds":  recordedDuration,
	}
	
	// Read existing data or create new structure
	var data map[string]interface{}
	if fileData, err := os.ReadFile(recordsFile); err == nil {
		if err := json.Unmarshal(fileData, &data); err != nil {
			return fmt.Errorf("parse existing records: %w", err)
		}
	} else if os.IsNotExist(err) {
		// Initialize new database file structure
		data = map[string]interface{}{
			"date":     currentDate,
			"username": ch.Config.Username,
			"site":     ch.Config.Site,
			"recordings": []interface{}{},
			"summary": map[string]interface{}{
				"total_recordings":  0,
				"total_size_bytes": 0,
			},
		}
	} else {
		return fmt.Errorf("read records file: %w", err)
	}
	
	// Append new record
	recordings, ok := data["recordings"].([]interface{})
	if !ok {
		recordings = []interface{}{}
	}
	recordings = append(recordings, record)
	data["recordings"] = recordings
	
	// Update summary statistics
	var totalSize int64
	for _, rec := range recordings {
		if recMap, ok := rec.(map[string]interface{}); ok {
			if size, ok := recMap["filesize_bytes"].(int64); ok {
				totalSize += size
			} else if size, ok := recMap["filesize_bytes"].(float64); ok {
				totalSize += int64(size)
			}
		}
	}
	
	summary := map[string]interface{}{
		"total_recordings":  len(recordings),
		"total_size_bytes": totalSize,
		"last_updated":     time.Now().UTC().Format(time.RFC3339),
	}
	data["summary"] = summary
	
	// Write atomically using temp file
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal records: %w", err)
	}
	
	tempFile := recordsFile + ".tmp"
	if err := os.WriteFile(tempFile, jsonData, 0644); err != nil {
		return fmt.Errorf("write temp records file: %w", err)
	}
	
	// Atomic rename
	if err := os.Rename(tempFile, recordsFile); err != nil {
		os.Remove(tempFile) // Clean up temp file on error
		return fmt.Errorf("rename records file: %w", err)
	}
	
	return nil
}


// generateThumbnail creates a thumbnail image from a video file using FFmpeg
func generateThumbnail(videoPath string) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg not found in PATH")
	}

	// Create thumbnail path (same directory, .jpg extension)
	thumbnailPath := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + "_thumb.jpg"
	
	// Remove existing thumbnail if it exists
	_ = os.Remove(thumbnailPath)

	// Generate thumbnail at 2 seconds into the video (safer for short videos)
	// -ss 2: seek to 2 seconds
	// -i: input file
	// -vframes 1: extract 1 frame
	// -vf scale=640:-2: scale to 640px width, maintain aspect ratio (divisible by 2)
	// -q:v 2: high quality JPEG (1-31, lower is better)
	// -y: overwrite output file
	args := []string{
		"-y",
		"-ss", "2",
		"-i", videoPath,
		"-vframes", "1",
		"-vf", "scale=640:-2",
		"-q:v", "2",
		thumbnailPath,
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Try without seeking if that failed (for very short videos)
		args = []string{
			"-y",
			"-i", videoPath,
			"-vframes", "1",
			"-vf", "scale=640:-2",
			"-q:v", "2",
			thumbnailPath,
		}
		cmd = exec.Command("ffmpeg", args...)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ffmpeg thumbnail generation failed: %s: %w", string(output), err)
		}
	}

	// Verify thumbnail was created and has content
	fileInfo, err := os.Stat(thumbnailPath)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("thumbnail file was not created")
	}
	if fileInfo.Size() < 1000 { // Less than 1KB is probably corrupted
		os.Remove(thumbnailPath)
		return "", fmt.Errorf("thumbnail file is too small (corrupted)")
	}

	return thumbnailPath, nil
}

// ProcessOrphanedFile processes an orphaned recording file (interrupted recording)
// This is called on startup to handle files that weren't uploaded due to interruption
func (ch *Channel) ProcessOrphanedFile(filePath string) {
	ch.Info("processing orphaned file: `%s`", filepath.Base(filePath))
	
	// Get file info
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		ch.Error("failed to stat orphaned file `%s`: %s", filePath, err.Error())
		return
	}
	
	// Skip empty files
	if fileInfo.Size() == 0 {
		ch.Info("removing empty orphaned file: `%s`", filepath.Base(filePath))
		os.Remove(filePath)
		return
	}
	
	// Duration is unknown for orphaned files; use 0 rather than a misleading estimate.
	var approximateDuration float64
	
	ch.Info("orphaned file size: %.2f MB", float64(fileInfo.Size())/(1024*1024))
	
	// Process the file (same as finalizeRecordingAsync but without the finalization flag)
	finalPath := filePath
	
	// Step 1: Convert/remux if needed
	if server.Config.FinalizeMode != "none" {
		ch.Info("🔄 converting orphaned file `%s` to %s...", filepath.Base(filePath), finalOutputExt(filePath))
		processedPath, err := ch.runFFmpegFinalizer(filePath)
		if err != nil {
			ch.Error("❌ ffmpeg %s failed for orphaned file `%s`: %s", server.Config.FinalizeMode, filePath, err.Error())
			ch.Error("❌ CRITICAL: conversion failed - will NOT upload .ts file")
			ch.Info("keeping original file locally because finalization failed")
			// Move to completed dir but don't upload
			completedDir := completedDirForChannel(ch)
			if completedDir != "" {
				cleanCompleted := filepath.Clean(completedDir)
				cleanFinal := filepath.Clean(filepath.Dir(filePath))
				if cleanFinal != cleanCompleted {
					dst, err := moveRecordingToDir(filePath, recordingDirFromPattern(ch.Config.Pattern), completedDir)
					if err != nil {
						ch.Error("move failed orphaned file `%s`: %s", filePath, err.Error())
					} else {
						ch.Info("failed orphaned file moved to `%s`", dst)
					}
				}
			}
			go ch.ScanTotalDiskUsage()
			return // Don't upload unconverted .ts files
		} else {
			ch.Info("✅ conversion complete: `%s`", filepath.Base(processedPath))
			if processedPath != filePath {
				if err := os.Remove(filePath); err != nil {
					ch.Error("remove original after ffmpeg finalization `%s`: %s", filePath, err.Error())
				} else {
					ch.Info("✅ removed original .ts file")
				}
			}
			finalPath = processedPath
		}
	} else if strings.HasSuffix(filePath, ".mp4") {
		if err := chaturbate.BuildSeekIndex(filePath); err != nil {
			ch.Error("seek index %s: %v", filePath, err)
		}
	}
	
	// Step 1.5: Validate file before upload (ensure it's .mp4 or .mkv, not .ts)
	finalExt := strings.ToLower(filepath.Ext(finalPath))
	if finalExt != ".mp4" && finalExt != ".mkv" {
		ch.Error("❌ CRITICAL: orphaned file has invalid extension %s (expected .mp4 or .mkv)", finalExt)
		ch.Error("❌ refusing to upload unconverted file - this indicates FFmpeg conversion failed silently")
		ch.Info("keeping file locally: `%s`", filepath.Base(finalPath))
		// Move to completed dir but don't upload
		completedDir := completedDirForChannel(ch)
		if completedDir != "" {
			cleanCompleted := filepath.Clean(completedDir)
			cleanFinal := filepath.Clean(filepath.Dir(finalPath))
			if cleanFinal != cleanCompleted {
				dst, err := moveRecordingToDir(finalPath, recordingDirFromPattern(ch.Config.Pattern), completedDir)
				if err != nil {
					ch.Error("move invalid orphaned file `%s`: %s", finalPath, err.Error())
				} else {
					ch.Info("invalid orphaned file moved to `%s`", dst)
				}
			}
		}
		go ch.ScanTotalDiskUsage()
		return
	}
	
	// Step 2: Upload to multiple hosts if enabled
	if server.Config.EnableGoFileUpload {
		ch.Info("📤 uploading orphaned file `%s` to multiple hosts...", filepath.Base(finalPath))
		
		// Log API key status for debugging
		ch.Info("🔑 API key status:")
		ch.Info("  • GoFile: always enabled (no key required)")
		if server.Config.TurboViPlayAPIKey != "" {
			ch.Info("  • TurboViPlay: ✓ configured")
		} else {
			ch.Info("  • TurboViPlay: ✗ not configured")
		}
		if server.Config.VoeSXAPIKey != "" {
			ch.Info("  • VOE.sx: ✓ configured")
		} else {
			ch.Info("  • VOE.sx: ✗ not configured")
		}
		if server.Config.StreamtapeLogin != "" && server.Config.StreamtapeAPIKey != "" {
			ch.Info("  • Streamtape: ✓ configured")
		} else {
			ch.Info("  • Streamtape: ✗ not configured")
		}
		
		// Create multi-host uploader
		multiUploader := uploader.NewMultiHostUploader(
			server.Config.TurboViPlayAPIKey,
			server.Config.VoeSXAPIKey,
			server.Config.StreamtapeLogin,
			server.Config.StreamtapeAPIKey,
		)
		
		// Upload to all hosts in parallel
		ch.Info("🚀 starting parallel uploads to all configured hosts...")
		results := multiUploader.UploadToAll(finalPath)
		
		// Get successful uploads
		successfulUploads := uploader.GetSuccessfulUploads(results)
		
		if len(successfulUploads) == 0 {
			ch.Error("❌ all uploads failed for orphaned file `%s`", finalPath)
			for _, result := range results {
				ch.Error("  ✗ %s: %v", result.Host, result.Error)
			}
			ch.Info("keeping local file because all uploads failed")
		} else {
			// Log results
			ch.Info("✅ upload completed: %d/%d successful", len(successfulUploads), len(results))
			for _, result := range results {
				if result.Error == nil {
					ch.Info("  ✓ %s: %s", result.Host, result.DownloadLink)
				} else {
					ch.Error("  ✗ %s: %v", result.Host, result.Error)
				}
			}
			
			// Step 2.5: Generate and upload thumbnail to ImgBB
			var thumbnailLink string
			ch.Info("📸 generating thumbnail for orphaned file `%s`...", filepath.Base(finalPath))
			tempThumbPath, err := generateThumbnail(finalPath)
			if err != nil {
				ch.Error("❌ thumbnail generation failed: %s", err.Error())
			} else {
				// Upload thumbnail to ImgBB
				ch.Info("uploading thumbnail to ImgBB...")
				if server.Config.ImgBBAPIKey == "" {
					ch.Error("❌ ImgBB API key not configured, skipping thumbnail upload")
					os.Remove(tempThumbPath)
				} else {
					thumbnailUploader := uploader.NewThumbnailUploader(server.Config.ImgBBAPIKey)
					uploadedURL, err := thumbnailUploader.Upload(tempThumbPath)
					if err != nil {
						ch.Error("❌ thumbnail upload to ImgBB failed: %s", err.Error())
						os.Remove(tempThumbPath)
					} else {
						ch.Info("✓ thumbnail uploaded to ImgBB: %s", uploadedURL)
						thumbnailLink = uploadedURL
						os.Remove(tempThumbPath)
					}
				}
			}
			
			// Store all successful upload links in database

			// Extract links by host for database logging
			var gofileLink, turboviplayLink, voesxLink, streamtapeLink string
			for _, result := range successfulUploads {
				switch result.Host {
				case "GoFile":
					gofileLink = result.DownloadLink
				case "TurboViPlay":
					turboviplayLink = result.DownloadLink
				case "VOE.sx":
					voesxLink = result.DownloadLink
				case "Streamtape":
					streamtapeLink = result.DownloadLink
				}
			}

			// Store in GitHub Actions database (JSON files)
			if err := ch.logUploadToDatabase(finalPath, gofileLink, turboviplayLink, voesxLink, streamtapeLink, thumbnailLink, approximateDuration, fileInfo.Size()); err != nil {
				ch.Error("failed to log upload to GitHub Actions database: %s", err.Error())
			} else {
				ch.Info("upload logged to GitHub Actions database")
			}
			
			// Store upload records in Supabase when credentials are available
			if server.Config.SupabaseURL != "" && server.Config.SupabaseAPIKey != "" {
				ch.Info("storing orphaned file upload record in Supabase...")
				supabaseClient := supabase.NewClient(server.Config.SupabaseURL, server.Config.SupabaseAPIKey)
				// Store single record with all links (reuse vars from outer scope)
				if err := supabaseClient.InsertMultiHostUploadRecord(
					ch.Config.Username,
					filepath.Base(finalPath),
					gofileLink,
					turboviplayLink,
					voesxLink,
					streamtapeLink,
					thumbnailLink,
				); err != nil {
					ch.Error("failed to store multi-host upload record in Supabase: %s", err.Error())
				} else {
					ch.Info("✓ multi-host upload record stored in Supabase (%d hosts)", len(successfulUploads))
				}
			} else {
				ch.Info("Supabase credentials not configured, skipping Supabase upload")
			}
			
			// Step 3: Delete local file after successful upload
			ch.Info("deleting local orphaned file `%s`...", filepath.Base(finalPath))
			if err := os.Remove(finalPath); err != nil {
				ch.Error("failed to delete local file `%s`: %s", finalPath, err.Error())
			} else {
				ch.Info("local orphaned file deleted successfully")
			}
			
			go ch.ScanTotalDiskUsage()
			return
		}
	}
	
	// Smart strategy: file was written directly into completedDir, no move needed.
	// Only fall back to moveRecordingToDir if the file ended up outside completedDir.
	completedDir := completedDirForChannel(ch)
	if completedDir != "" {
		cleanCompleted := filepath.Clean(completedDir)
		cleanFinal := filepath.Clean(filepath.Dir(finalPath))
		if cleanFinal != cleanCompleted {
			dst, err := moveRecordingToDir(finalPath, recordingDirFromPattern(ch.Config.Pattern), completedDir)
			if err != nil {
				ch.Error("move orphaned recording `%s`: %s", finalPath, err.Error())
			} else {
				ch.Info("orphaned recording moved to `%s`", dst)
			}
		} else {
			ch.Info("orphaned recording already in completed dir: `%s`", filepath.Base(finalPath))
		}
	}
	
	go ch.ScanTotalDiskUsage()
}
