package router

import "testing"

// TestOrphanNeedsUpload pins the rule that decides whether an on-disk file is
// listed as an orphan.  The regression it guards: a recordings row is written at
// ENQUEUE time, before any upload, so "the row exists" must never be treated as
// "the file is in the cloud" — that is how recordings whose upload never landed
// stayed invisible to the orphan list (and to the /api/orphans/retry rescue path)
// forever.
func TestOrphanNeedsUpload(t *testing.T) {
	safe := map[string]bool{"uploaded.mp4": true, "linked-merged.mp4.merged.mp4": true}
	rows := map[string]bool{
		"uploaded.mp4":                   true,
		"no-links.mp4":                   true,
		"linked-merged.mp4.merged.mp4":   true,
		"no-links-merged.mp4.merged.mp4": true,
	}

	cases := []struct {
		name       string
		filename   string
		wantOrphan bool
	}{
		// Proven in the cloud — never an orphan.
		{"uploaded recording", "uploaded.mp4", false},
		// The case that used to be hidden: row exists, zero upload links.
		{"recording with no upload links", "no-links.mp4", true},
		// No metadata at all — the classic orphan.
		{"unknown file", "stranger.mp4", true},
		// Bare merge intermediate: held on disk by design until the session ends.
		{"merge intermediate", "live-session.mp4.merged.mp4", false},
		// A .merged. file WITH a row is a published recording, so it is judged
		// like any other file — these are stranded like anything else.
		{"published merged, linked", "linked-merged.mp4.merged.mp4", false},
		{"published merged, no links", "no-links-merged.mp4.merged.mp4", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orphanNeedsUpload(tc.filename, rows, safe); got != tc.wantOrphan {
				t.Errorf("orphanNeedsUpload(%q) = %v, want %v", tc.filename, got, tc.wantOrphan)
			}
		})
	}
}

// TestOrphanNeedsUploadFallsBackToRowExistence documents the degradation path: when
// the upload-link index is unavailable, scanOrphanFiles seeds safeInCloud from the
// recordings table itself, so a Supabase hiccup cannot turn the whole disk into
// orphans.
func TestOrphanNeedsUploadFallsBackToRowExistence(t *testing.T) {
	// Index unavailable → the caller marks every recordings row as safe.
	rows := map[string]bool{"has-row.mp4": true}
	safe := map[string]bool{"has-row.mp4": true}

	if orphanNeedsUpload("has-row.mp4", rows, safe) {
		t.Error("with the index unavailable a file whose row exists must not be reported as an orphan")
	}
	if !orphanNeedsUpload("no-row.mp4", rows, safe) {
		t.Error("with the index unavailable a file with no row is still an orphan")
	}
}
