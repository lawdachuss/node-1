// Command pruneorphanpreviews removes preview_images rows whose recordings row no
// longer exists (older than a grace period), the litter left behind when a
// recording row is deleted or replaced while its preview links were written
// without a recording_id — which is why the table's ON DELETE CASCADE never
// reaches them.
//
// It runs the same code path as the periodic in-app sweep
// (server.CleanupOrphanedPreviewImages), so it is also the right way to clear an
// existing backlog without waiting for a tick.
//
// Usage:
//
//	go run ./cmd/pruneorphanpreviews -dry-run
//	go run ./cmd/pruneorphanpreviews
//	go run ./cmd/pruneorphanpreviews -output-dir D:\videos
//
// Pass -output-dir (or set OUTPUT_DIR) when running ON a recording node so video
// files still sitting on its disk keep their preview row; without it, the sweep
// only protects filenames that still have a recordings row.
package main

import (
	"bufio"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "report how many rows would be pruned without deleting anything")
	outputDir := flag.String("output-dir", "", "output directory of this node, kept out of the prune (defaults to $OUTPUT_DIR)")
	flag.Parse()

	loadDotEnv(".env")

	dir := *outputDir
	if dir == "" {
		dir = os.Getenv("OUTPUT_DIR")
	}
	server.Config = &entity.Config{
		SupabaseURL:            env("SUPABASE_URL"),
		SupabaseAPIKey:         env("SUPABASE_API_KEY"),
		SupabaseServiceRoleKey: env("SUPABASE_SERVICE_ROLE_KEY"),
		OutputDir:              dir,
	}
	server.SyncNodeEnvironment()

	if server.Config.SupabaseURL == "" || server.Config.SupabaseAPIKey == "" {
		log.Fatal("SUPABASE_URL / SUPABASE_API_KEY not set")
	}
	if server.GetDBClient() == nil {
		log.Fatal("Supabase not configured")
	}
	// preview_images is a service-role-only table for writes (RLS grants anon reads
	// and service_role everything), so a delete without this key is silently
	// rejected rather than fatal.  Fail loudly instead of reporting "pruned 0".
	if !*dryRun && server.Config.SupabaseServiceRoleKey == "" {
		log.Fatal("SUPABASE_SERVICE_ROLE_KEY not set (required to delete preview_images rows)")
	}

	if *dryRun {
		count, err := server.CountOrphanedPreviewImages()
		if err != nil {
			log.Fatalf("count: %v", err)
		}
		log.Printf("dry run: %d preview_images row(s) would be pruned", count)
		return
	}

	deleted := server.CleanupOrphanedPreviewImages()
	log.Printf("pruned %d orphaned preview_images row(s)", deleted)
}

func env(k string) string { return os.Getenv(k) }

// loadDotEnv mirrors the other maintenance commands: .env provides the Supabase
// credentials, and real environment variables win.
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

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
