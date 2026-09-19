package channel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteSidecarFilesKeepsSidecarsWhileAssetUploadInFlight pins the ordering
// that was violated in production:
//
// The pipeline's collect stops waiting for a presentation asset after
// thumbnailAssetTimeout and deliberately lets the asset's goroutine keep
// uploading mirrors in the background (its onHost callback keeps persisting
// late URLs).  stageCleanup then saw all three URLs present, verified them in
// the DB and deleted the local sidecars — while those still-running uploads
// were reading them.  Every remaining attempt failed with "imgpile: open file:
// ..." / "imgbb: read file: ..." minutes after the pipeline had logged
// "completed ... successfully".
//
// Node-9 / Lusty_cherry_2026-09-18_13-24-49.mp4 is the observed example:
//
//	14:18:11 pipeline: stage cleanup
//	14:18:12 cleanup: removed Lusty_cherry_2026-09-18_13-24-49.mp4
//	14:18:12 pipeline: completed ... successfully
//	14:22:20 UploadToAll: ImgBB failed for ....sprite.jpg  (read file: open ...)
//	14:23:32 UploadToAll: ImgBB failed for ....thumb.jpg
//	14:23:32 UploadToAll: ImgBB failed for ....preview.webp
//
// Deletion must therefore wait for the upload that owns the file.
func TestDeleteSidecarFilesKeepsSidecarsWhileAssetUploadInFlight(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "alice_2026-01-01_12-00-00.mp4")
	sidecars := []string{video + ".thumb.jpg", video + ".sprite.jpg", video + ".preview.webp"}
	mustWriteFile(t, video)
	for _, s := range sidecars {
		mustWriteFile(t, s)
	}

	assertSidecars := func(stage string, wantPresent bool) {
		t.Helper()
		for _, s := range sidecars {
			_, err := os.Stat(s)
			if wantPresent && err != nil {
				t.Fatalf("%s: sidecar %s was deleted while its upload was still running: %v",
					stage, filepath.Base(s), err)
			}
			if !wantPresent && err == nil {
				t.Fatalf("%s: sidecar %s survived after every asset upload finished", stage, filepath.Base(s))
			}
		}
	}

	// Cleanup running while the (abandoned) asset goroutine is still uploading
	// must leave every sidecar alone.
	markThumbnailAssetUploadStart(video)
	t.Cleanup(func() { markThumbnailAssetUploadDone(video) })
	DeleteSidecarFiles(video)
	assertSidecars("one asset in flight", true)

	// All three assets share one video path, so the marker has to survive until
	// the LAST goroutine finishes — not the first.
	markThumbnailAssetUploadStart(video)
	markThumbnailAssetUploadDone(video)
	if !IsThumbnailAssetUploadInFlight(video) {
		t.Fatal("marker cleared while an asset upload was still in flight (one of three finished)")
	}
	DeleteSidecarFiles(video)
	assertSidecars("two assets in flight", true)

	// Last asset done: deletion is allowed again, and the main video is never
	// touched by DeleteSidecarFiles (stageCleanup removes it separately, so a
	// kept sidecar must not strand the video).
	markThumbnailAssetUploadDone(video)
	DeleteSidecarFiles(video)
	assertSidecars("all uploads finished", false)
	if _, err := os.Stat(video); err != nil {
		t.Errorf("DeleteSidecarFiles removed the main video: %v", err)
	}
}

// TestThumbnailAssetUploadMarkerClearsOnLastDone guards the other direction: a
// marker that is never cleared would make DeleteSidecarFiles skip forever and
// let sidecars accumulate on disk.
func TestThumbnailAssetUploadMarkerClearsOnLastDone(t *testing.T) {
	video := filepath.Join(t.TempDir(), "bob_2026-01-01_12-00-00.mp4")

	if IsThumbnailAssetUploadInFlight(video) {
		t.Fatal("unknown path reported as in flight")
	}
	markThumbnailAssetUploadStart(video)
	if !IsThumbnailAssetUploadInFlight(video) {
		t.Fatal("started asset upload not reported as in flight")
	}
	markThumbnailAssetUploadDone(video)
	if IsThumbnailAssetUploadInFlight(video) {
		t.Fatal("marker survived the last markThumbnailAssetUploadDone — sidecars would never be deleted")
	}
}
