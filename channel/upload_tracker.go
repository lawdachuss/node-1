package channel

import (
	"path/filepath"
	"sync"
)

var (
	pendingUploadsMu sync.Mutex
	pendingUploads   = make(map[string]struct{})
)

// MarkUploadInFlight records that a file is currently being uploaded by the
// channel system.  The watcher calls IsUploadInFlight before processing a
// file and skips it when this returns true, preventing duplicate uploads
// and the "file not found" race when DeleteLocalAfterUpload fires.
func MarkUploadInFlight(filePath string) {
	filePath = normalizeUploadPath(filePath)
	pendingUploadsMu.Lock()
	pendingUploads[filePath] = struct{}{}
	pendingUploadsMu.Unlock()
}

// MarkUploadDone removes a file from the in-flight set.  Called via defer in
// upload goroutines so the set is always cleaned up.
func MarkUploadDone(filePath string) {
	filePath = normalizeUploadPath(filePath)
	pendingUploadsMu.Lock()
	delete(pendingUploads, filePath)
	pendingUploadsMu.Unlock()
}

// IsUploadInFlight returns true if the file is currently being uploaded.
func IsUploadInFlight(filePath string) bool {
	filePath = normalizeUploadPath(filePath)
	pendingUploadsMu.Lock()
	_, ok := pendingUploads[filePath]
	pendingUploadsMu.Unlock()
	return ok
}

// InFlightCount returns the number of files currently marked in-flight.
// Exposed for diagnostics: a count far larger than the number of active
// pipelines suggests stale markers were left behind by a crashed flow — the
// "already uploading, skipping duplicate" failure mode.
func InFlightCount() int {
	pendingUploadsMu.Lock()
	defer pendingUploadsMu.Unlock()
	return len(pendingUploads)
}

// ─── Presentation-asset (thumb/sprite/preview) uploads ────────────────────────

var (
	thumbnailAssetUploadsMu sync.Mutex
	thumbnailAssetUploads   = make(map[string]int)
)

// markThumbnailAssetUploadStart records that one of the thumbnail generator's
// three asset goroutines (thumbnail/sprite/preview) is uploading for videoPath.
//
// Those goroutines can OUTLIVE whoever started them: the collector in
// generateThumbnailForFile stops waiting for an asset after
// thumbnailAssetTimeout and deliberately leaves the goroutine running so its
// background mirror uploads still reach the DB through onHost.  A concurrent
// flow that deleted the local sidecars at that point (the pipeline's
// stageCleanup, the orphan sweep, the watcher) pulled the very files out from
// under it: every remaining attempt then failed with "imgpile: open file: ..."
// / "imgbb: read file: ..." minutes AFTER the pipeline had logged "completed
// ... successfully" — the sidecars were already gone.  Sidecar deletion is
// therefore gated on this marker (see DeleteSidecarFiles).
//
// Counted, not a set: all three assets share one video path, so the marker must
// stay set until the LAST of them finishes.
func markThumbnailAssetUploadStart(videoPath string) {
	videoPath = normalizeUploadPath(videoPath)
	thumbnailAssetUploadsMu.Lock()
	thumbnailAssetUploads[videoPath]++
	thumbnailAssetUploadsMu.Unlock()
}

// markThumbnailAssetUploadDone clears one in-flight asset upload.  Always
// called via defer, so a panic can never leave the marker set — a stale marker
// only delays sidecar deletion (each asset goroutine removes its own sidecar),
// it never loses data.
func markThumbnailAssetUploadDone(videoPath string) {
	videoPath = normalizeUploadPath(videoPath)
	thumbnailAssetUploadsMu.Lock()
	if n := thumbnailAssetUploads[videoPath]; n > 1 {
		thumbnailAssetUploads[videoPath] = n - 1
	} else {
		delete(thumbnailAssetUploads, videoPath)
	}
	thumbnailAssetUploadsMu.Unlock()
}

// IsThumbnailAssetUploadInFlight reports whether a presentation-asset upload
// for videoPath may still be reading that video's sidecar files.
func IsThumbnailAssetUploadInFlight(videoPath string) bool {
	videoPath = normalizeUploadPath(videoPath)
	thumbnailAssetUploadsMu.Lock()
	n := thumbnailAssetUploads[videoPath]
	thumbnailAssetUploadsMu.Unlock()
	return n > 0
}

func normalizeUploadPath(filePath string) string {
	if abs, err := filepath.Abs(filePath); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(filePath)
}
