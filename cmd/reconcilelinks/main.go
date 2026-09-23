// Command reconcilelinks restores upload_links rows for recordings whose upload
// leg died between the journal write and the per-host link save, running the same
// code path every node uses at startup (server.ReconcileMissingUploadLinks).
//
// The fleet already runs that sweep on every restart and on the maintenance
// ticker, so this command exists to repair without waiting for a restart: right
// after the no-host function is deployed, or after an outage that left links
// missing fleet-wide.  It is additive and idempotent — SaveUploadLink upserts on
// (recording_id, host) — so running it twice changes nothing the second time.
//
// Usage:
//
//	go run ./cmd/reconcilelinks
//
// It requires the recordings_without_upload_links() function (migration
// 20260923000000); without it the sweep reports that it is unavailable and
// restores nothing.  Restored rows are stamped with this machine's node identity.
package main

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

func main() {
	loadDotEnv(".env")

	server.Config = &entity.Config{
		SupabaseURL:            env("SUPABASE_URL"),
		SupabaseAPIKey:         env("SUPABASE_API_KEY"),
		SupabaseServiceRoleKey: env("SUPABASE_SERVICE_ROLE_KEY"),
	}
	server.SyncNodeEnvironment()

	if server.Config.SupabaseURL == "" || server.Config.SupabaseAPIKey == "" {
		log.Fatal("SUPABASE_URL / SUPABASE_API_KEY not set")
	}
	if server.GetDBClient() == nil {
		log.Fatal("Supabase not configured")
	}
	// upload_links is service-role-only for writes; without this key every INSERT
	// is silently rejected and the sweep would report "restored 0".
	if server.Config.SupabaseServiceRoleKey == "" {
		log.Fatal("SUPABASE_SERVICE_ROLE_KEY not set (required to write upload_links rows)")
	}

	// Report the write path explicitly before touching hundreds of rows: if the
	// database is rejecting upload_links writes, every restore would fail and the
	// useful message would be buried in warnings.
	if err := server.CheckUploadLinkWritePath(); err != nil {
		log.Fatalf("upload-link write path is BROKEN — no link can be saved: %v", err)
	}
	log.Printf("upload-link write path OK (probe row written and removed)")

	restored := server.ReconcileMissingUploadLinks()
	log.Printf("restored %d upload link(s) from the upload journal", restored)
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
