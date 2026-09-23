package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// previewImageGraceDays is how long a preview_images row may exist without a
// recordings row before it is pruned.
//
// The grace period is what makes the sweep safe to run automatically: the
// preview links are written by the uploader's early onHost path, so a row can
// legitimately exist for a few minutes before its recording row lands, and a
// recordings row that is mid-repair (orphan recovery, RPC fallback) may briefly
// be missing too.  A week is far beyond any of those windows — the observed
// backlog is months old.
const previewImageGraceDays = 7

// CleanupOrphanedPreviewImages deletes preview_images rows whose recording no
// longer exists, returning how many were removed.
//
// preview_images is presentation metadata keyed by filename (thumb / sprite /
// preview URLs).  A row whose recordings row is gone can never be shown or
// repaired again — it is pure litter, and it accumulates: the table's
// recording_id FK cascades on delete, but the rows in the wild carry a NULL
// recording_id, so nothing removes them.  They are also invisible to every
// "keep this recording" path, which goes through the recordings table.
//
// Two things are never pruned:
//   - filenames that still have a recordings row (any row, with or without
//     upload links): for a recording whose local file is gone, the DB assets are
//     the last remaining copy of its thumbnail;
//   - video files still present on this node's disk, whose preview row is the
//     only image a not-yet-uploaded recording has.
func CleanupOrphanedPreviewImages() int {
	client := GetDBClient()
	if client == nil {
		return 0
	}

	keep, err := previewPruneKeepSet()
	if err != nil {
		fmt.Printf("[WARN] preview prune: %v\n", err)
		return 0
	}

	deleted, err := client.DeleteOrphanedPreviewImages(previewPruneCutoff(), keep)
	if err != nil {
		fmt.Printf("[WARN] preview prune: %v\n", err)
	}
	if deleted > 0 {
		fmt.Printf("[cleanup] pruned %d orphaned preview_images row(s) older than %d days\n",
			deleted, previewImageGraceDays)
		cacheClear()
	}
	return deleted
}

// CountOrphanedPreviewImages reports how many rows CleanupOrphanedPreviewImages
// would remove, without deleting anything (used by the maintenance command's
// -dry-run).  It shares the scan and the keep set, so the two cannot drift.
func CountOrphanedPreviewImages() (int, error) {
	client := GetDBClient()
	if client == nil {
		return 0, fmt.Errorf("supabase not configured")
	}
	keep, err := previewPruneKeepSet()
	if err != nil {
		return 0, err
	}
	return client.CountOrphanedPreviewImages(previewPruneCutoff(), keep)
}

// previewPruneCutoff is the age boundary of the grace period.
func previewPruneCutoff() time.Time {
	return time.Now().UTC().AddDate(0, 0, -previewImageGraceDays)
}

// previewPruneKeepSet is the "do not touch" set: every filename that still has a
// recordings row, plus every video file on this node's disk.
//
// Row existence — not upload links — is the right test for a preview row: a
// recording whose upload never landed still owns its preview images (they are the
// last copy of its thumbnail once the local file is gone), so it has to survive.
// It also keeps this sweep independent of the no-host RPC, so it works on every
// project regardless of which migrations are deployed.
func previewPruneKeepSet() (map[string]bool, error) {
	client := GetDBClient()
	if client == nil {
		return nil, fmt.Errorf("supabase not configured")
	}
	keep, err := client.GetRecordingFilenames()
	if err != nil {
		return nil, fmt.Errorf("could not load recordings index: %w", err)
	}
	if keep == nil {
		return nil, fmt.Errorf("could not load recordings index")
	}
	// keep is freshly built by that call, so annotating it in place is safe.
	for _, name := range localVideoFilenames() {
		keep[name] = true
	}
	return keep, nil
}

// localVideoFilenames lists the completed video files present on this node's disk
// (videos/ plus the configured OutputDir, non-recursive like the other disk
// scans).  Used to protect preview rows whose video has not been uploaded yet.
func localVideoFilenames() []string {
	dirs := []string{"videos"}
	if Config != nil && Config.OutputDir != "" {
		dirs = append(dirs, Config.OutputDir)
	}

	var names []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			switch strings.ToLower(filepath.Ext(name)) {
			case ".mp4", ".mkv", ".ts":
				names = append(names, name)
			}
		}
	}
	return names
}
