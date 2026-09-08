// Command backfillremotethumbs restores missing static thumbnails for
// recordings whose local video has already been deleted ("videos" page shows
// no thumbnail).
//
// Unlike backfillst (Streamtape's token-free API lets us Range-download the
// video head), the hosts that actually stored these recordings — GoFile,
// Mixdrop, Vidara, VOE.sx — gate direct video downloads behind
// Cloudflare/turnstile sessions and obfuscated players, so we cannot pull the
// video file to render a frame. However, Vidara renders its own thumbnail for
// each uploaded video and exposes it as the page's <meta property="og:image">,
// which we can fetch with a plain HTTP GET and re-host via the standard
// MultiImageUploader.
//
// The Vidara og:image resolution + download lives in the shared
// github.com/teacat/chaturbate-dvr/recovery package (the same logic used by the
// automated thumbnail sweep on the fleet). This command is the standalone,
// on-demand front end.
//
// Flow for each missing-thumbnail recording:
//  1. load all upload_links and recordings; keep recordings whose
//     thumbnail_url is empty AND that have a Vidara link,
//  2. delegate og:image scrape + download to recovery.VidaraThumb,
//  3. re-host it via MultiImageUploader (Catbox -> Pixhost -> freeimage.host),
//     keeping the host mirror map,
//  4. PATCH recordings.thumbnail_url and save a preview_images row,
//  5. log a per-host summary.
//
// Usage:
//
//	go run ./cmd/backfillremotethumbs [-dry] [-max N]
//
// Flags:
//
//	-dry   resolve + report thumbnail URLs only, do not upload or PATCH
//	-max N process at most N recordings (all by default)
package main

import (
	"bufio"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/database"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/recovery"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

// downloadClient reuses a single HTTP client with browser-like headers so the
// Vidara thumbnail CDN serves us the real JPEG (it 403s default curl UAs).
var downloadClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	},
}

func main() {
	dry := flag.Bool("dry", false, "resolve + report thumbnail URLs only")
	max := flag.Int("max", 0, "max recordings to process (0 = all)")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Ldate)
	loadDotEnv(".env")
	cfg := configFromEnv()
	if cfg.SupabaseURL == "" || cfg.SupabaseAPIKey == "" {
		log.Fatal("SUPABASE_URL / SUPABASE_API_KEY not set")
	}
	if !*dry && cfg.SupabaseServiceRoleKey == "" {
		log.Fatal("SUPABASE_SERVICE_ROLE_KEY not set (required for writes)")
	}
	server.Config = cfg
	server.SyncNodeEnvironment()

	client := server.GetDBClient()
	if client == nil {
		log.Fatal("Supabase not configured")
	}

	// 1. Load all upload links and group them by recording_id.
	links, err := client.GetAllUploadLinks()
	if err != nil {
		log.Fatalf("get upload links: %v", err)
	}
	linksByRec := map[string][]database.UploadLink{}
	for _, l := range links {
		linksByRec[l.RecordingID] = append(linksByRec[l.RecordingID], l)
	}
	log.Printf("loaded %d upload links across %d recordings", len(links), len(linksByRec))

	// 2. Load all recordings and pick those with an empty thumbnail_url.
	recs, err := client.GetAllRecordings()
	if err != nil {
		log.Fatalf("get recordings: %v", err)
	}
	var missing []database.Recording
	for i := range recs {
		if strings.TrimSpace(recs[i].ThumbnailURL) == "" {
			missing = append(missing, recs[i])
		}
	}
	log.Printf("%d recordings total, %d missing a thumbnail", len(recs), len(missing))

	// 3. Build work items: for each missing recording, find its Vidara link.
	type workItem struct {
		rec  *database.Recording
		link database.UploadLink
	}
	var work []workItem
	for i := range missing {
		rec := &missing[i]
		for _, l := range linksByRec[rec.ID] {
			if strings.EqualFold(l.Host, "Vidara") && strings.TrimSpace(l.URL) != "" {
				work = append(work, workItem{rec, l})
				break
			}
		}
	}
	log.Printf("of %d missing-thumbnail recordings, %d have a Vidara link to recover from", len(missing), len(work))

	if *max > 0 && len(work) > *max {
		work = work[:*max]
	}

	// 4+3. Resolve + optionally upload + PATCH.
	imgUploader := uploader.NewMultiImageUploader()
	var (
		ok       int
		failed   int
		noThumb  int
		rehosted int
	)
	for i, w := range work {
		code := recovery.VidaraCode(w.link.URL)
		if code == "" {
			log.Printf("[%d/%d] %s: bad Vidara URL %q", i+1, len(work), w.rec.Filename, w.link.URL)
			failed++
			continue
		}

		if *dry {
			// Dry-run: just resolve and report the host thumbnail URL.
			hostThumb, err := resolveVidaraThumb(code)
			if err != nil {
				log.Printf("[%d/%d] %s: resolve thumb: %v", i+1, len(work), w.rec.Filename, err)
				failed++
				continue
			}
			if hostThumb == "" {
				log.Printf("[%d/%d] %s: no og:image on page", i+1, len(work), w.rec.Filename)
				noThumb++
				continue
			}
			log.Printf("[%d/%d] %s: host thumbnail %s", i+1, len(work), w.rec.Filename, hostThumb)
			ok++
			continue
		}

		// Download the host thumbnail (og:image scrape + fetch + save).
		finalPath, err := recovery.VidaraThumb(w.link.URL, os.TempDir(), downloadClient)
		if err != nil {
			log.Printf("[%d/%d] %s: recover thumb: %v", i+1, len(work), w.rec.Filename, err)
			failed++
			continue
		}
		if finalPath == "" {
			log.Printf("[%d/%d] %s: no og:image on page", i+1, len(work), w.rec.Filename)
			noThumb++
			continue
		}

		// Re-host via the standard image pipeline.
		results := imgUploader.UploadToAll(finalPath, nil)
		os.Remove(finalPath)
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
			log.Printf("  ERROR: no image host accepted the thumbnail")
			failed++
			continue
		}

		// 4. PATCH recordings + preview_images.
		if err := server.UpdateRecordingThumbnails(w.rec.Filename, primary, "", ""); err != nil {
			log.Printf("  ERROR patch recordings: %v", err)
			failed++
			continue
		}
		if err := server.SavePreviewLinks(w.rec.Filename, primary, "", "", mirrors, nil, nil); err != nil {
			log.Printf("  ERROR patch preview_images: %v", err)
		}
		log.Printf("  OK -> %s (%d host mirrors)", primary, len(mirrors))
		ok++
		rehosted++
		time.Sleep(2 * time.Second) // pace image-host uploads
	}

	log.Printf("done: %d ok (%d re-hosted), %d failed, %d no-thumbnail-page", ok, rehosted, failed, noThumb)
}

// resolveVidaraThumb fetches the Vidara embed page and returns the og:image
// thumbnail URL, or "" if none is present. It mirrors recovery.VidaraThumb's
// page-scrape but only reports the URL (used for -dry).
func resolveVidaraThumb(code string) (string, error) {
	return recovery.ResolveVidaraImageURL(code, downloadClient)
}

func configFromEnv() *entity.Config {
	return &entity.Config{
		SupabaseURL:            env("SUPABASE_URL"),
		SupabaseAPIKey:         env("SUPABASE_API_KEY"),
		SupabaseServiceRoleKey: env("SUPABASE_SERVICE_ROLE_KEY"),
		FFmpegPath:             env("FFMPEG_PATH"),
		Domain:                 or(env("DOMAIN"), "https://www.cb.xxx/"),
	}
}

func env(k string) string { return os.Getenv(k) }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		if exe, e2 := os.Executable(); e2 == nil {
			f, err = os.Open(filepath.Join(filepath.Dir(exe), path))
		}
		if err != nil {
			return
		}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
