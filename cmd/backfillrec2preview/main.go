// Command backfillrec2preview copies thumbnail/sprite/preview URLs from the
// recordings table into the preview_images table for recordings that have
// thumbnails in recordings but not in preview_images.
//
// Usage:
//
//	go run ./cmd/backfillrec2preview [-dry-run] [-delay D]
package main

import (
	"bufio"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "Show what would be synced without writing")
	delay := flag.Duration("delay", 300*time.Millisecond, "Delay between PATCHes")
	flag.Parse()

	log.SetFlags(log.Ltime)
	log.Printf("backfillrec2preview: dry-run=%v delay=%v", *dryRun, *delay)

	loadDotEnv(".env")
	cfg := configFromEnv()
	if cfg.SupabaseURL == "" || cfg.SupabaseAPIKey == "" {
		log.Fatal("SUPABASE_URL / SUPABASE_API_KEY not set")
	}
	server.Config = cfg
	server.SyncNodeEnvironment()

	client := server.GetDBClient()
	if client == nil {
		log.Fatal("Supabase not configured")
	}

	log.Printf("loading all preview_images links...")
	previews := server.LoadAllPreviewLinks()
	log.Printf("preview_images rows loaded: %d", len(previews))

	log.Printf("loading all recordings...")
	recordings, err := client.GetAllRecordings()
	if err != nil {
		log.Fatalf("get recordings: %v", err)
	}
	log.Printf("recordings loaded: %d", len(recordings))

	type fix struct {
		filename               string
		thumb, sprite, preview string
		thumbMirrors           map[string]string
		spriteMirrors          map[string]string
		previewMirrors         map[string]string
	}
	var fixes []fix
	for i := range recordings {
		rec := &recordings[i]
		if rec.ThumbnailURL == "" {
			continue
		}
		if links, ok := previews[rec.Filename]; ok && links[0] != "" {
			continue // already has thumbnail in preview_images
		}
		fixes = append(fixes, fix{
			filename:       rec.Filename,
			thumb:          rec.ThumbnailURL,
			sprite:         rec.SpriteURL,
			preview:        rec.PreviewURL,
			thumbMirrors:   rec.ThumbnailMirrors,
			spriteMirrors:  rec.SpriteMirrors,
			previewMirrors: rec.PreviewMirrors,
		})
	}

	log.Printf("recordings with thumbnail in recordings but missing from preview_images: %d", len(fixes))

	okCount := 0
	for i, f := range fixes {
		if *dryRun {
			log.Printf("[%d/%d] %s -> %s", i+1, len(fixes), f.filename, f.thumb)
			continue
		}
		if err := server.SavePreviewLinks(f.filename, f.thumb, f.sprite, f.preview, f.thumbMirrors, f.spriteMirrors, f.previewMirrors); err != nil {
			log.Printf("  FAIL %s: %v", f.filename, err)
			continue
		}
		log.Printf("[%d/%d] OK %s -> %s", i+1, len(fixes), f.filename, f.thumb)
		okCount++
		if *delay > 0 {
			time.Sleep(*delay)
		}
	}

	log.Printf("done: synced %d/%d recordings", okCount, len(fixes))
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			val = strings.Trim(val, "\"'")
			os.Setenv(key, val)
		}
	}
}

func configFromEnv() *entity.Config {
	return &entity.Config{
		SupabaseURL:            env("SUPABASE_URL"),
		SupabaseAPIKey:         env("SUPABASE_API_KEY"),
		SupabaseServiceRoleKey: env("SUPABASE_SERVICE_ROLE_KEY"),
		Domain:                 or(env("DOMAIN"), "https://www.cb.xxx/"),
		FFmpegPath:             env("FFMPEG_PATH"),
	}
}

func env(k string) string { return os.Getenv(k) }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
