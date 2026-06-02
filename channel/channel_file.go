package channel

import (
        "bytes"
        "errors"
        "fmt"
        "html/template"
        "io"
        "log"
        "os"
        "path/filepath"
        "strings"
        "time"

        "github.com/teacat/chaturbate-dvr/config"
        "github.com/teacat/chaturbate-dvr/internal"
        "github.com/teacat/chaturbate-dvr/server"
        "github.com/teacat/chaturbate-dvr/uploader"
)

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

// NextFile prepares the next file to be created, by cleaning up the last file and generating a new one
func (ch *Channel) NextFile() error {
        if err := ch.Cleanup(true); err != nil {
                return err
        }
        filename, err := ch.GenerateFilename()
        if err != nil {
                return err
        }
        ch.CurrentFilename = filename
        if err := ch.CreateNewFile(filename); err != nil {
                return err
        }

        // Increment the sequence number for the next file
        ch.Sequence++
        return nil
}

// Cleanup closes any open recording files and either queues them for later
// post-processing (isRotation=true, during file rotation) or processes the
// entire pending queue (isRotation=false, when the session ends).
func (ch *Channel) Cleanup(isRotation bool) error {
        ch.cleanupMu.Lock()
        defer ch.cleanupMu.Unlock()

        if ch.File == nil && ch.AudioFile == nil && len(ch.pendingFiles) == 0 {
                return nil
        }

	// Close any open files and add them to the pending queue.
	// Errors from closeTrackedFile are logged but not returned — aborting
	// would strand ALL previously queued pendingFiles permanently.
	if ch.File != nil || ch.AudioFile != nil {
		videoPath, videoInfo, err := closeTrackedFile(ch.File)
		if err != nil {
			ch.Error("cleanup: video file close: %s", err.Error())
		}
		audioPath, audioInfo, err := closeTrackedFile(ch.AudioFile)
		if err != nil {
			ch.Error("cleanup: audio file close: %s", err.Error())
		}

		ch.File = nil
		ch.AudioFile = nil
		ch.CurrentFilename = ""
		ch.stateMu.Lock()
		ch.Filesize = 0
		ch.Duration = 0
		ch.stateMu.Unlock()

		// Skip empty files (both tracks zero/missing).
		if ch.HasSeparateAudio {
			if videoInfo == nil && audioInfo == nil {
				if !isRotation {
					ch.processPendingQueue()
				}
				return nil
			}
		} else {
			if videoInfo == nil || videoInfo.Size() == 0 {
				if !isRotation {
					ch.processPendingQueue()
				}
				return nil
			}
		}

		hasSeparateAudio := ch.HasSeparateAudio
		ch.pendingFiles = append(ch.pendingFiles, pendingFile{
			videoPath:        videoPath,
			audioPath:        audioPath,
			hasSeparateAudio: hasSeparateAudio,
		})
		if videoPath != "" {
			ch.Info("cleanup: queued %s for post-processing (%d pending)", filepath.Base(videoPath), len(ch.pendingFiles))
		} else if audioPath != "" {
			ch.Info("cleanup: queued %s for post-processing (%d pending)", filepath.Base(audioPath), len(ch.pendingFiles))
		}
	}

        ch.processPendingQueue()
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

	if pf.hasSeparateAudio && audioPath != "" {
		ch.processPendingMuxPair(videoPath, audioPath)
		return
	}

        // Single-stream file — move to output dir (triggers preview + upload).
        if _, err := os.Stat(videoPath); err == nil {
                if ch.Config.Compress {
                        ch.CompressFile(videoPath)
                } else {
                        ch.MoveToOutputDir(videoPath)
                }
        }
}

func (ch *Channel) processPendingMuxPair(videoPath, audioPath string) {
        videoInfo, _ := os.Stat(videoPath)
        audioInfo, _ := os.Stat(audioPath)

        switch {
        case videoInfo == nil && audioInfo == nil:
                return
        case videoInfo == nil:
                ch.Info("mux: video track missing; preserving audio-only file %s", filepath.Base(audioPath))
                if ch.Config.Compress {
                        ch.CompressFile(audioPath)
                } else {
                        ch.MoveToOutputDir(audioPath)
                }
                return
        case audioInfo == nil:
                ch.Info("mux: audio track missing; preserving video-only file %s", filepath.Base(videoPath))
                if ch.Config.Compress {
                        ch.CompressFile(videoPath)
                } else {
                        ch.MoveToOutputDir(videoPath)
                }
                return
        }

	// Both tracks exist — mux them together.
	finalOutput := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + ".muxed.mp4"
	if err := ch.MuxAV(videoPath, audioPath, finalOutput); err != nil {
                ch.Info("mux: ffmpeg mux failed, trying native fallback: %s", err.Error())
                if nativeErr := ch.MuxAVNative(videoPath, audioPath, finalOutput); nativeErr != nil {
                        ch.Error("mux failed for %s: %v", filepath.Base(videoPath), nativeErr)
                        return
                }
        }

        if ok, reason := muxOutputLooksValid(finalOutput, videoInfo, audioInfo); !ok {
                ch.Error("mux: output looks corrupt (%s); keeping sidecars %s and %s", reason, filepath.Base(videoPath), filepath.Base(audioPath))
                _ = os.Remove(finalOutput)
                return
        }

        _ = os.Remove(videoPath)
        _ = os.Remove(audioPath)
        ch.Info("delete: removed sidecar %s", filepath.Base(videoPath))
        ch.Info("delete: removed sidecar %s", filepath.Base(audioPath))

        if ch.Config.Compress {
                ch.CompressFile(finalOutput)
        } else {
                ch.MoveToOutputDir(finalOutput)
        }
}

// muxOutputLooksValid returns true if the muxed MP4 exists and contains data.
// With `-c copy -shortest` the output is intentionally truncated to the shorter
// track's duration, so we cannot compare file sizes against the input sum.
// Trust ffmpeg's exit code — if it returned 0 the file is valid.
func muxOutputLooksValid(outputPath string, _ /*videoInfo*/, _ /*audioInfo*/ os.FileInfo) (bool, string) {
        finalInfo, err := os.Stat(outputPath)
        if err != nil {
                return false, fmt.Sprintf("stat: %s", err.Error())
        }
        if finalInfo.Size() == 0 {
                return false, "empty output"
        }
        return true, ""
}

// MoveToOutputDir relocates a finalized recording into server.Config.OutputDir.
// Errors are non-fatal: the recording is already safely written at srcPath.
func (ch *Channel) MoveToOutputDir(srcPath string) string {
        if server.Config == nil || server.Config.OutputDir == "" {
	ch.UploadWg.Add(1)
		go func() {
			defer ch.UploadWg.Done()
			UploadSem <- struct{}{}
			ch.uploadSem <- struct{}{}
			defer func() { <-ch.uploadSem; <-UploadSem }()
			ch.generatePreviewAndUpload(srcPath)
		}()
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
        if err := moveFile(srcPath, destPath); err != nil {
                ch.Error("output-dir: move %s: %s", filepath.Base(srcPath), err.Error())
                return srcPath
        }
        ch.Info("output-dir: moved %s -> %s", filepath.Base(srcPath), destPath)
	ch.UploadWg.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] upload goroutine for %s: %v", filepath.Base(destPath), r)
			}
			ch.UploadWg.Done()
		}()
		UploadSem <- struct{}{}
		ch.uploadSem <- struct{}{}
		defer func() { <-ch.uploadSem; <-UploadSem }()
		ch.generatePreviewAndUpload(destPath)
	}()
	return destPath
}

func (ch *Channel) generatePreviewAndUpload(filePath string) {
	thumbURL, spriteURL, previewURL := ch.generateThumbnail(filePath)
	ch.uploadFile(filePath, thumbURL, spriteURL, previewURL)
}

// uniqueDestPath returns path if it does not exist, otherwise appends
// " (n)" before the extension until an unused path is found. Gives up
// after 1000 tries and returns the last candidate.
func uniqueDestPath(path string) string {
        if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
                return path
        }
        ext := filepath.Ext(path)
        base := path[:len(path)-len(ext)]
        for i := 1; i < 1000; i++ {
                candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
                if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
                        return candidate
                }
        }
        return fmt.Sprintf("%s (999)%s", base, ext)
}

func moveFile(src, dest string) error {
        if err := os.Rename(src, dest); err == nil {
                return nil
        }

        in, err := os.Open(src)
        if err != nil {
                return err
        }
        defer in.Close()

        out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
        if err != nil {
                return err
        }
        if _, err := io.Copy(out, in); err != nil {
                out.Close()
                os.Remove(dest)
                return err
        }
        // Sync before close so a crash between close and os.Remove(src) can't
        // leave a truncated destination alongside a deleted source.
        if err := out.Sync(); err != nil {
                out.Close()
                os.Remove(dest)
                return err
        }
        if err := out.Close(); err != nil {
                os.Remove(dest)
                return err
        }
        return os.Remove(src)
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

// CleanupOrphanedFiles processes orphaned sidecar files left behind by
// cancelled or crashed post-processing runs. Instead of deleting them,
// it runs them through the full pipeline: mux (if split A/V), generate
// thumbnails, upload to hosts, save metadata to Supabase, then delete.
func CleanupOrphanedFiles() {
        if server.Config == nil {
                return
        }

        dirs := []string{"videos"}
        if server.Config.OutputDir != "" {
                dirs = append(dirs, server.Config.OutputDir)
        }

        for _, dir := range dirs {
                entries, err := os.ReadDir(dir)
                if err != nil {
                        continue
                }

                // Classify files by type
                type fileInfo struct {
                        path string
                        name string
                }
                mainVideos := map[string]fileInfo{} // stem -> info
                muxedFiles := map[string]fileInfo{} // stem -> info
                videoParts := map[string]fileInfo{} // stem -> info (.video.mp4)
                audioParts := map[string]fileInfo{} // stem -> info (.audio.mp4)

                for _, e := range entries {
                        if e.IsDir() {
                                continue
                        }
                        name := e.Name()
                        path := filepath.Join(dir, name)
                        ext := strings.ToLower(filepath.Ext(name))

                        switch {
                        case strings.HasSuffix(name, ".video.muxed.mp4"):
                                stem := strings.TrimSuffix(name, ".video.muxed.mp4")
                                muxedFiles[stem] = fileInfo{path, name}
                        case strings.HasSuffix(name, ".video.mp4"):
                                stem := strings.TrimSuffix(name, ".video.mp4")
                                videoParts[stem] = fileInfo{path, name}
                        case strings.HasSuffix(name, ".audio.mp4"):
                                stem := strings.TrimSuffix(name, ".audio.mp4")
                                audioParts[stem] = fileInfo{path, name}
                        case (ext == ".mp4" || ext == ".mkv" || ext == ".ts") &&
                                !strings.Contains(name, ".video.") &&
                                !strings.Contains(name, ".audio.") &&
                                !strings.Contains(name, ".muxed."):
                                stem := strings.TrimSuffix(name, filepath.Ext(name))
                                mainVideos[stem] = fileInfo{path, name}
                        }
                }

		// Process orphaned muxed files (output from a mux that was never uploaded)
		sem := make(chan struct{}, 5)
		for stem, info := range muxedFiles {
			if _, hasMain := mainVideos[stem]; hasMain {
				continue
			}
			stem, info := stem, info
			sem <- struct{}{}
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[PANIC] processing orphaned muxed file %s: %v", info.name, r)
					}
					<-sem
				}()
				recoveryLogf(info.name, "processing orphaned muxed file %s", info.name)

				// Check journal to skip files that were already fully uploaded
				if IsAlreadyFullyUploaded(info.path) {
					recoveryLogf(info.name, "all hosts already have this file per journal — removing local copy")
					os.Remove(info.path)
					DeleteSidecarFiles(info.path)
					_ = stem
					return
				}

				thumbURL, spriteURL, previewURL := GenerateThumbnailForFile(info.path)
				UploadOrphanedFile(info.path, thumbURL, spriteURL, previewURL)
				DeleteSidecarFiles(info.path)
				_ = stem
			}()
		}

		// Process orphaned split A/V pairs (mux them first, then upload)
		for stem, vInfo := range videoParts {
			if _, hasMain := mainVideos[stem]; hasMain {
				continue
			}
			aInfo, hasAudio := audioParts[stem]
			if !hasAudio {
				continue
			}

			stem, vInfo, aInfo := stem, vInfo, aInfo
			sem <- struct{}{}
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[PANIC] muxing orphaned split A/V pair %s: %v", stem, r)
					}
					<-sem
				}()

				// Mux the pair
				muxedPath := filepath.Join(dir, stem+".video.muxed.mp4")
				recoveryLogf(vInfo.name, "recovery: muxing orphaned split A/V pair %s", stem)
				if err := muxVideoAudio(vInfo.path, aInfo.path, muxedPath); err != nil {
					recoveryLogf(vInfo.name, "recovery: mux failed for %s: %v — uploading video-only", stem, err)
				// Fall back to uploading just the video track
				thumbURL, spriteURL, previewURL := GenerateThumbnailForFile(vInfo.path)
				UploadOrphanedFile(vInfo.path, thumbURL, spriteURL, previewURL)
					DeleteSidecarFiles(vInfo.path)
					return
				}

				// Delete source sidecars
				os.Remove(vInfo.path)
				os.Remove(aInfo.path)

				// Generate thumbnails, upload, and clean up
				thumbURL, spriteURL, previewURL := GenerateThumbnailForFile(muxedPath)
				UploadOrphanedFile(muxedPath, thumbURL, spriteURL, previewURL)
				DeleteSidecarFiles(muxedPath)
				os.Remove(muxedPath)
			}()
		}

		// Wait for all orphan processing to complete
		for i := 0; i < cap(sem); i++ {
			sem <- struct{}{}
		}

		// Clean up orphaned sidecar files whose main video no longer exists
			sidecarExts := []string{".thumb.jpg", ".sprite.jpg", ".thumb", ".sprite"}
                for _, e := range entries {
                        if e.IsDir() {
                                continue
                        }
                        name := e.Name()
                        path := filepath.Join(dir, name)
                        for _, suffix := range sidecarExts {
                                if !strings.HasSuffix(name, suffix) {
                                        continue
                                }
                                base := strings.TrimSuffix(name, suffix)
                                hasMain := false
                                for ext := range map[string]bool{".mp4": true, ".mkv": true, ".ts": true} {
                                        if _, ok := mainVideos[base+ext]; ok {
                                                hasMain = true
                                                break
                                        }
                                }
                                if !hasMain {
                                        os.Remove(path)
                                }
                                break
                        }
                }
        }
}

// DeleteSidecarFiles removes preview sidecar files associated with a video path.
func DeleteSidecarFiles(videoPath string) {
        for _, suffix := range []string{".thumb.jpg", ".sprite.jpg", ".thumb", ".sprite"} {
                os.Remove(videoPath + suffix)
        }
}

// muxVideoAudio combines a separate video and audio file into a single MP4.
func muxVideoAudio(videoPath, audioPath, outputPath string) error {
        cmd := config.FFmpegCommand("-y",
                "-i", videoPath,
                "-i", audioPath,
                "-c", "copy",
                "-movflags", "+faststart",
                outputPath,
        )
        return cmd.Run()
}

// extractUsernameFromFilename parses "username_YYYY-MM-DD_HH-MM-SS.ext" to get the username.
// Uses a tighter pattern: looks for "_20" followed immediately by two digits, a hyphen, two digits,
// a hyphen, and two digits (i.e. the date portion YYYY-MM-DD).  This avoids false matches when
// a username itself contains "_20" (e.g. "alice_20_fan_2025-01-01...").
func extractUsernameFromFilename(filename string) string {
	if idx := strings.Index(filename, "_20"); idx > 0 {
		rest := filename[idx+1:]
		if len(rest) >= 10 && rest[3] == '-' && rest[6] == '-' {
			return filename[:idx]
		}
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
func configuredUploadHosts() []string {
	cfg := server.Config
	if cfg == nil {
		return nil
	}
	var hosts []string
	hosts = append(hosts, "GoFile")
	if cfg.TurboViPlayAPIKey != "" {
		hosts = append(hosts, "TurboViPlay")
	}
	if cfg.VoeSXAPIKey != "" {
		hosts = append(hosts, "VOE.sx")
	}
	if cfg.SendCMAPIKey != "" {
		hosts = append(hosts, "SendCM")
	}
	if cfg.ByseAPIKey != "" {
		hosts = append(hosts, "Byse")
	}
	if cfg.StreamtapeLogin != "" && cfg.StreamtapeKey != "" {
		hosts = append(hosts, "Streamtape")
	}
	if cfg.MixdropEmail != "" && cfg.MixdropToken != "" {
		hosts = append(hosts, "Mixdrop")
	}
	if cfg.PixelDrainToken != "" {
		hosts = append(hosts, "PixelDrain")
	}
	return hosts
}

// UploadOrphanedFile uploads a file to all configured hosts and saves metadata
// to Supabase. Unlike Channel.uploadFile, this doesn't require an active channel.
// Username is extracted from the filename; metadata fields are left empty.
//
// If every configured host fails on the first attempt, it retries up to 2 more
// times with a 60-second delay between attempts.  This handles transient network
// or API outages that can occur when the app restarts after a crash.
func UploadOrphanedFile(filePath, thumbURL, spriteURL, previewURL string) bool {
	cfg := server.Config
	if cfg == nil {
		return false
	}

	filename := filepath.Base(filePath)

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
		cfg.TurboViPlayAPIKey,
		cfg.VoeSXAPIKey,
		cfg.SendCMAPIKey,
		cfg.ByseAPIKey,
		cfg.StreamtapeLogin,
		cfg.StreamtapeKey,
		cfg.MixdropEmail,
		cfg.MixdropToken,
		cfg.PixelDrainToken,
		nil, // no logger for orphan recovery
	)

	allHosts := upl.AvailableHosts()

	// Determine which hosts still need the file
	hostsToTry := allHosts
	if len(completedHosts) > 0 {
		hostsToTry = difference(allHosts, completedHosts)
		if len(hostsToTry) == 0 {
			recoveryLogf(filename, "all hosts already have this file per journal — skipping upload")
			return true
		}
		recoveryLogf(filename, "%d/%d hosts already have this file — uploading to %d remaining",
			len(completedHosts), len(allHosts), len(hostsToTry))
	}

	var results []uploader.UploadResult
	var success []uploader.UploadResult
	for attempt := 1; attempt <= maxUploadAttempts; attempt++ {
		if attempt > 1 && len(hostsToTry) == 0 {
			break
		}
		attemptResults := upl.UploadSelected(filePath, hostsToTry)
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

		if attempt < maxUploadAttempts {
			failedHosts := failedHostNames(results, completedHosts)
			hostsToTry = failedHosts
			if len(hostsToTry) > 0 {
				recoveryLogf(filename, "%d hosts still pending — retrying in %s...", len(hostsToTry), retryDelay)
				time.Sleep(retryDelay)
			}
		}
	}

	if len(success) == 0 {
		recoveryLogf(filename, "[WARN] all upload attempts exhausted — file will be retried on next restart")
		return false
	}

	// Build links map
	links := map[string]string{}
	var embedURL string
	for _, r := range success {
		links[r.Host] = r.DownloadLink
		if embedURL == "" {
			embedURL = embedURLFromLink(r.Host, r.DownloadLink)
		}
	}

	stat, _ := os.Stat(filePath)
	var filesize int64
	if stat != nil {
		filesize = stat.Size()
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := server.SaveRecordingWithLinks(
		extractUsernameFromFilename(filename), filename, timestamp,
		"", nil, 0, "", 0, filesize, "", embedURL, thumbURL, spriteURL, links,
	); err != nil {
		recoveryLogf(filename, "failed to save recording to Supabase: %v", err)
	} else {
		recoveryLogf(filename, "saved recording metadata")
	}

	// Delete local file after successful upload + DB save
	if cfg.DeleteLocalAfterUpload {
		os.Remove(filePath)
		DeleteSidecarFiles(filePath)
		if fileHash != "" {
			if jErr := server.DeleteJournalByHash(fileHash); jErr != nil {
				recoveryLogf(filename, "could not delete journal: %v", jErr)
			}
		}
		recoveryLogf(filename, "removed local file")
	}

	return true
}

// ShouldSwitchFile determines whether a new file should be created.
func (ch *Channel) ShouldSwitchFile() bool {
        maxFilesizeBytes := ch.Config.MaxFilesize * 1024 * 1024
        maxDurationSeconds := ch.Config.MaxDuration * 60

        return (ch.Duration >= float64(maxDurationSeconds) && ch.Config.MaxDuration > 0) ||
                (ch.Filesize >= maxFilesizeBytes && ch.Config.MaxFilesize > 0)
}
