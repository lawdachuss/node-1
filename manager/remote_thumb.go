package manager

import (
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/database"
	"github.com/teacat/chaturbate-dvr/recovery"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

// remoteThumbCooldown is how long a single node waits before re-attempting a
// recording whose remote-thumbnail recovery failed. It prevents the every-30-min
// sweep from hammering Streamtape/Vidara with the same unfixable file on every
// tick, while still retrying failures on a normal cadence.
const remoteThumbCooldown = 3 * time.Hour

// remoteThumbPacing spaces out remote downloads/uploads so a mass recovery never
// trips image-host rate limits or Streamtape's ticket rate limiter.
const remoteThumbPacing = 3 * time.Second

// remoteThumbState holds per-node, in-memory cooldowns for remote recovery.
var (
	remoteThumbMu   sync.Mutex
	remoteThumbLast = map[string]time.Time{}
)

// remoteThumbVidaraHTTP is a shared browser-like HTTP client for fetching Vidara
// og:image thumbnails (Vidara's CDN 403s default curl UAs).
var remoteThumbVidaraHTTP = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	},
}

// SyncRemoteThumbnails restores a static thumbnail for recordings whose local
// video is no longer on disk but which still exist on an upload host — Streamtape
// (partial Range download of the head, render a frame) or Vidara (host-rendered
// og:image). It runs alongside the cheap DB->DB copy in server.SyncRecordingsThumbnails.
//
// It is safe to call on every sweep from every node: records whose recovery
// succeeds drop out of the missing-thumbnail set, and failures are cooled down
// per-node so the fleet does not re-download the same unfixable file on every
// 30-minute tick.
func (m *Manager) SyncRemoteThumbnails() {
	remoteThumbMu.Lock()
	if m.remoteThumbScanning {
		remoteThumbMu.Unlock()
		return
	}
	m.remoteThumbScanning = true
	remoteThumbMu.Unlock()
	defer func() {
		remoteThumbMu.Lock()
		m.remoteThumbScanning = false
		remoteThumbMu.Unlock()
	}()

	client := server.GetDBClient()
	if client == nil {
		return
	}

	stLogin := server.Config.StreamtapeLogin
	stKey := server.Config.StreamtapeKey

	// Load upload links once and index by recording id.
	links, err := client.GetAllUploadLinks()
	if err != nil {
		log.Printf("[remote-thumb] could not load upload links: %v", err)
		return
	}
	linksByRec := map[string][]database.UploadLink{}
	for _, l := range links {
		linksByRec[l.RecordingID] = append(linksByRec[l.RecordingID], l)
	}

	recordings, err := client.GetRecordingsMissingThumbnails()
	if err != nil {
		log.Printf("[remote-thumb] could not load recordings: %v", err)
		return
	}
	if len(recordings) == 0 {
		return
	}

	workDir, err := os.MkdirTemp("", "remote-thumb")
	if err != nil {
		log.Printf("[remote-thumb] temp dir: %v", err)
		return
	}
	defer os.RemoveAll(workDir)

	imgUploader := uploader.NewMultiImageUploader()
	recovered := 0
	attempted := 0
	for i := range recordings {
		rec := &recordings[i]
		if rec.ThumbnailURL != "" {
			continue
		}
		remoteThumbMu.Lock()
		last, seen := remoteThumbLast[rec.Filename]
		remoteThumbMu.Unlock()
		if seen && time.Since(last) < remoteThumbCooldown {
			continue
		}
		// Find a recovery source among the recording's upload links.
		var stCode, vidaraURL string
		for _, l := range linksByRec[rec.ID] {
			host := strings.ToLower(l.Host)
			switch {
			case strings.Contains(host, "streamtape") && l.URL != "":
				if c := recovery.ExtractStreamtapeCode(l.URL); c != "" {
					stCode = c
				}
			case strings.Contains(host, "vidara") && l.URL != "":
				vidaraURL = l.URL
			}
		}

		var (
			localImg  string
			recoverRR error
		)
		if stCode != "" && stLogin != "" && stKey != "" {
			attempted++
			// Prefer Streamtape (a real video frame); fall back to Vidara's
			// og:image if the Streamtape grab fails or the code is stale.
			if img, err := recovery.StreamtapeThumb(stCode, workDir, stLogin, stKey, 4, 2, 0); err != nil {
				log.Printf("[remote-thumb] %s: streamtape recover failed (%v) — trying Vidara", rec.Filename, err)
				recoverRR = err
			} else {
				localImg = img
			}
		}
		if localImg == "" && vidaraURL != "" {
			attempted++
			if img, err := recovery.VidaraThumb(vidaraURL, workDir, remoteThumbVidaraHTTP); err != nil {
				log.Printf("[remote-thumb] %s: vidara recover failed: %v", rec.Filename, err)
				recoverRR = err
			} else {
				localImg = img
				recoverRR = nil
			}
		}
		if recoverRR != nil && localImg == "" {
			log.Printf("[remote-thumb] %s: recover failed: %v", rec.Filename, recoverRR)
			markRemoteThumbFailed(rec.Filename)
			continue
		}
		if localImg == "" {
			// e.g. Vidara page had no og:image — not currently fixable.
			log.Printf("[remote-thumb] %s: no recoverable thumbnail (streamtape ticket or Vidara og:image unavailable)", rec.Filename)
			markRemoteThumbFailed(rec.Filename)
			continue
		}

		// Upload the recovered image through the standard image pipeline.
		results := imgUploader.UploadToAll(localImg, nil)
		mirrors := map[string]string{}
		var primary string
		for _, r := range results {
			if r.Err == nil && r.URL != "" {
				mirrors[r.Host] = r.URL
				if primary == "" {
					primary = r.URL
				}
			}
		}
		if primary == "" {
			log.Printf("[remote-thumb] %s: no image host accepted the thumbnail", rec.Filename)
			markRemoteThumbFailed(rec.Filename)
			continue
		}

		// Persist to both tables. These recordings have no sprite/preview, so
		// only the thumbnail is written (empty fields are preserved by the API).
		if err := server.UpdateRecordingThumbnails(rec.Filename, primary, "", ""); err != nil {
			log.Printf("[remote-thumb] %s: patch recordings: %v", rec.Filename, err)
			markRemoteThumbFailed(rec.Filename)
			continue
		}
		if err := server.SavePreviewLinks(rec.Filename, primary, "", "", mirrors, nil, nil); err != nil {
			log.Printf("[remote-thumb] %s: patch preview_images: %v", rec.Filename, err)
		}
		log.Printf("[remote-thumb] %s: recovered -> %s (%d host mirrors)", rec.Filename, primary, len(mirrors))
		recovered++
		time.Sleep(remoteThumbPacing)
	}
	if recovered > 0 {
		log.Printf("[remote-thumb] recovered thumbnails for %d recording(s) (%d attempted)", recovered, attempted)
	}
}

// markRemoteThumbFailed records a failed recovery attempt for cooldown tracking.
func markRemoteThumbFailed(filename string) {
	remoteThumbMu.Lock()
	remoteThumbLast[filename] = time.Now()
	remoteThumbMu.Unlock()
}
