package server

import (
	"testing"
	"time"
)

// TestLookupPreviewLinksVerbatim covers the exact-filename path.
func TestLookupPreviewLinksVerbatim(t *testing.T) {
	previews := map[string][3]string{
		"alice_2026-09-01_10-00-00.mp4": {"https://thumb", "https://sprite", "https://preview"},
	}
	links, ok := lookupPreviewLinks(previews, "alice_2026-09-01_10-00-00.mp4")
	if !ok {
		t.Fatal("exact filename match should succeed")
	}
	if links[0] != "https://thumb" {
		t.Errorf("thumb = %q, want https://thumb", links[0])
	}
}

// TestLookupPreviewLinksMergedFallback ensures a "<file>.merged.mp4" recording
// falls back to the preview_images row stored under the original filename.
func TestLookupPreviewLinksMergedFallback(t *testing.T) {
	previews := map[string][3]string{
		"bob_2026-09-01_10-00-00.mp4": {"https://thumb", "https://sprite", "https://preview"},
	}
	links, ok := lookupPreviewLinks(previews, "bob_2026-09-01_10-00-00.mp4.merged.mp4")
	if !ok {
		t.Fatal("merged file should fall back to original filename")
	}
	if links[0] != "https://thumb" {
		t.Errorf("thumb = %q, want https://thumb", links[0])
	}
}

// TestLookupPreviewLinksNoMatch verifies unknown files report not-found.
func TestLookupPreviewLinksNoMatch(t *testing.T) {
	previews := map[string][3]string{
		"alice.mp4": {"https://thumb", "", ""},
	}
	if _, ok := lookupPreviewLinks(previews, "nobody.mp4"); ok {
		t.Fatal("unknown file should not match")
	}
	if _, ok := lookupPreviewLinks(previews, "nobody2.mp4.merged.mp4"); ok {
		t.Fatal("unknown merged file should not match")
	}
}

// TestMergeThumbAssets preserves existing sprite/preview and only fills gaps.
func TestMergeThumbAssets(t *testing.T) {
	sprite, preview := mergeThumbAssets("https://existing-sprite", "", "https://existing-preview", "")
	if sprite != "https://existing-sprite" {
		t.Errorf("sprite = %q, want existing sprite preserved", sprite)
	}
	if preview != "https://existing-preview" {
		t.Errorf("preview = %q, want existing preview preserved", preview)
	}

	sprite, preview = mergeThumbAssets("", "https://new-sprite", "https://existing-preview", "")
	if sprite != "https://new-sprite" {
		t.Errorf("sprite = %q, want new sprite when existing empty", sprite)
	}
	if preview != "https://existing-preview" {
		t.Errorf("preview = %q, want existing preview preserved", preview)
	}
}

// TestRecThumbUnfixableBackoff verifies a marked file is skipped and that the
// skip expires after the backoff window (so later-generated previews are seen).
func TestRecThumbUnfixableBackoff(t *testing.T) {
	defer func() {
		recThumbUnfixableMu.Lock()
		recThumbUnfixable = map[string]time.Time{}
		recThumbUnfixableMu.Unlock()
	}()

	recThumbUnfixableMu.Lock()
	recThumbUnfixable["stuck_2026-09-01_00-00-00.mp4"] = time.Now().Add(-time.Hour) // 1h ago, still < 24h
	recThumbUnfixableMu.Unlock()

	skips := recThumbUnfixableSkips()
	if !skips["stuck_2026-09-01_00-00-00.mp4"] {
		t.Fatal("recently-marked file should be skipped")
	}

	// Age a file past the backoff and confirm it is pruned (eligible again).
	recThumbUnfixableMu.Lock()
	recThumbUnfixable["stuck_2026-09-01_00-00-00.mp4"] = time.Now().Add(-recThumbUnfixableBackoff - time.Minute)
	recThumbUnfixableMu.Unlock()

	skips = recThumbUnfixableSkips()
	if skips["stuck_2026-09-01_00-00-00.mp4"] {
		t.Fatal("expired entry should be pruned and no longer skipped")
	}
	recThumbUnfixableMu.Lock()
	_, stillThere := recThumbUnfixable["stuck_2026-09-01_00-00-00.mp4"]
	recThumbUnfixableMu.Unlock()
	if stillThere {
		t.Fatal("expired entry should be removed from the map")
	}
}
