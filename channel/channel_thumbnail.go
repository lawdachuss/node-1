package channel

import (
        "context"
        "fmt"
        "log"
        "os"
        "os/exec"
        "path/filepath"
        "strconv"
        "strings"
        "time"

        "github.com/teacat/chaturbate-dvr/uploader"
)

const numSpriteFrames = 10

// generateThumbnail is the channel-scoped wrapper — logs go to the channel log.
func (ch *Channel) generateThumbnail(videoPath string) {
        generateThumbnailForFile(videoPath,
                func(f string, a ...interface{}) { ch.Info(f, a...) },
                func(f string, a ...interface{}) { ch.Error(f, a...) },
        )
}

// GenerateThumbnailForFile is a standalone thumbnail generator that can be
// called outside of a channel context (e.g. for pre-existing video files).
func GenerateThumbnailForFile(videoPath string) {
        generateThumbnailForFile(videoPath,
                func(f string, a ...interface{}) { log.Printf("[thumb] "+f, a...) },
                func(f string, a ...interface{}) { log.Printf("[thumb:err] "+f, a...) },
        )
}

// generateThumbnailForFile extracts a thumbnail and a sprite sheet of 10
// evenly-spaced frames from the video. Both are uploaded to 0x0.st.
// URLs are saved as sidecars: video.mp4.thumb and video.mp4.sprite
func generateThumbnailForFile(videoPath string, info, errFn func(string, ...interface{})) {
        ext := strings.ToLower(filepath.Ext(videoPath))
        if ext != ".mp4" && ext != ".mkv" {
                return
        }

        baseName := filepath.Base(videoPath)
        host := uploader.NewFreeimageUploader()

        // ── 1. Thumbnail frame at 5s ──────────────────────────────────────────────
        thumbPath := videoPath + ".thumb"
        if _, err := os.Stat(thumbPath); os.IsNotExist(err) {
                tmpJPG := videoPath + ".tmp_thumb.jpg"
                ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
                err := exec.CommandContext(ctx, "ffmpeg",
                        "-y", "-i", videoPath,
                        "-ss", "00:00:05",
                        "-vframes", "1",
                        "-s", "320x180",
                        "-q:v", "3",
                        tmpJPG,
                ).Run()
                cancel()

                if err != nil {
                        info("thumb: frame extract failed for %s: %v", baseName, err)
                } else if _, statErr := os.Stat(tmpJPG); statErr == nil {
                        if url, e := host.Upload(tmpJPG); e == nil {
                                os.WriteFile(thumbPath, []byte(url), 0644)
                                info("thumb: uploaded thumbnail for %s", baseName)
                        } else {
                                errFn("thumb: upload failed for %s: %v", baseName, e)
                        }
                        os.Remove(tmpJPG)
                }
        }

        // ── 2. Sprite sheet: N frames evenly spaced ───────────────────────────────
        spritePath := videoPath + ".sprite"
        if _, err := os.Stat(spritePath); os.IsNotExist(err) {
                duration := 30.0
                probeCtx, probeCancel := context.WithTimeout(context.Background(), 15*time.Second)
                if out, e := exec.CommandContext(probeCtx, "ffprobe",
                        "-v", "error",
                        "-show_entries", "format=duration",
                        "-of", "default=noprint_wrappers=1:nokey=1",
                        videoPath,
                ).Output(); e == nil {
                        if d, e := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); e == nil && d > 1 {
                                duration = d
                        }
                }
                probeCancel()

                tmpDir := videoPath + ".sprite_frames"
                os.MkdirAll(tmpDir, 0755)
                interval := duration / float64(numSpriteFrames)
                allOK := true

                for i := 0; i < numSpriteFrames && allOK; i++ {
                        seek := float64(i) * interval
                        framePath := filepath.Join(tmpDir, fmt.Sprintf("f_%02d.jpg", i))
                        frameCtx, frameCancel := context.WithTimeout(context.Background(), 30*time.Second)
                        if out, e := exec.CommandContext(frameCtx, "ffmpeg",
                                "-y",
                                "-ss", fmt.Sprintf("%.1f", seek),
                                "-i", videoPath,
                                "-vframes", "1",
                                "-s", "320x180",
                                "-q:v", "3",
                                framePath,
                        ).CombinedOutput(); e != nil {
                                info("thumb: sprite frame %d/%d failed for %s: %v", i+1, numSpriteFrames, baseName, e)
                                if len(out) > 0 {
                                        msg := string(out)
                                        if len(msg) > 300 {
                                                msg = msg[:300]
                                        }
                                        info("thumb: ffmpeg output: %s", msg)
                                }
                                allOK = false
                        }
                        frameCancel()
                }

                if allOK {
                        tmpSprite := videoPath + ".tmp_sprite.jpg"
                        args := []string{"-y"}
                        for i := 0; i < numSpriteFrames; i++ {
                                args = append(args, "-i", filepath.Join(tmpDir, fmt.Sprintf("f_%02d.jpg", i)))
                        }
                        args = append(args,
                                "-filter_complex", fmt.Sprintf("hstack=inputs=%d", numSpriteFrames),
                                "-frames:v", "1",
                                "-q:v", "3",
                                tmpSprite,
                        )

                        tileCtx, tileCancel := context.WithTimeout(context.Background(), 30*time.Second)
                        if out, e := exec.CommandContext(tileCtx, "ffmpeg", args...).CombinedOutput(); e != nil {
                                info("thumb: sprite tile failed for %s: %v", baseName, e)
                                if len(out) > 0 {
                                        msg := string(out)
                                        if len(msg) > 300 {
                                                msg = msg[:300]
                                        }
                                        info("thumb: ffmpeg output: %s", msg)
                                }
                        } else if _, e := os.Stat(tmpSprite); e == nil {
                                if url, ue := host.Upload(tmpSprite); ue == nil {
                                        os.WriteFile(spritePath, []byte(url), 0644)
                                        info("thumb: uploaded sprite for %s", baseName)
                                } else {
                                        errFn("thumb: sprite upload failed for %s: %v", baseName, ue)
                                }
                                os.Remove(tmpSprite)
                        }
                        tileCancel()
                }

                os.RemoveAll(tmpDir)
        }
}
