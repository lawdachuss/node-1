package channel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
)

// TestMergeTwoFiles validates that two consecutive same-session recordings are
// joined into one continuous video (timeline re-based) and the originals are
// removed on success.
func TestMergeTwoFiles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	server.Config = &entity.Config{FinalizeMode: "remux", FFmpegContainer: "mp4"}

	dir := t.TempDir()
	a := filepath.Join(dir, "cam_2026-01-01_00-00-00.mp4")
	b := filepath.Join(dir, "cam_2026-01-01_00-02-05.mp4")
	gen := func(out, freq string) {
		cmd := exec.Command("ffmpeg", "-y",
			"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=15",
			"-f", "lavfi", "-i", "sine=frequency="+freq+":duration=2",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
			"-f", "mp4", "-movflags", "+frag_keyframe+empty_moov", out)
		if outb, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v\n%s", out, err, outb)
		}
	}
	gen(a, "440")
	gen(b, "660")

	merged, err := mergeTwoFiles(a, b)
	if err != nil {
		t.Fatalf("mergeTwoFiles: %v", err)
	}
	da, _ := VideoDurationSeconds(a)
	db, _ := VideoDurationSeconds(b)
	dm, err := VideoDurationSeconds(merged)
	if err != nil {
		t.Fatalf("probe merged: %v", err)
	}
	if dm < (da+db)*0.85 {
		t.Fatalf("merged duration %.1fs < 85%% of inputs %.1fs", dm, da+db)
	}
	if _, err := os.Stat(a); err == nil {
		t.Errorf("input %s was not removed after merge", a)
	}
	if _, err := os.Stat(b); err == nil {
		t.Errorf("input %s was not removed after merge", b)
	}
	// Re-merge the merged file with a third cycle to ensure stable naming.
	c := filepath.Join(dir, "cam_2026-01-01_00-04-10.mp4")
	gen(c, "880")
	merged2, err := mergeTwoFiles(merged, c)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	dc, _ := VideoDurationSeconds(c)
	dm2, err := VideoDurationSeconds(merged2)
	if err != nil {
		t.Fatalf("probe merged2: %v", err)
	}
	if dm2 < (dm+dc)*0.85 {
		t.Fatalf("merged2 duration %.1fs < 85%% of %.1fs", dm2, dm+dc)
	}
}

// TestFlushHeldSessionMergeUploadsHeldFile locks the fix for the permanent
// stranding bug: a file parked in sessionMergeByUser whose session never
// produced another cycle (offline after HLS token expiry, empty/deleted final
// file) used to stay in the map forever — in-flight-marked, never enqueued,
// no recordings row — until the runner was torn down.  ProcessPending must
// flush the hold before awaiting uploads.
func TestFlushHeldSessionMergeUploadsHeldFile(t *testing.T) {
	oldConfig := server.Config
	defer func() { server.Config = oldConfig }()
	server.Config = &entity.Config{FinalizeMode: "remux", OutputDir: t.TempDir()}

	dir := t.TempDir()
	path := filepath.Join(dir, "alice_2025-01-01_12-00-00.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o666); err != nil {
		t.Fatalf("write: %v", err)
	}

	ch := &Channel{
		Config:   &entity.ChannelConfig{Username: "alice"},
		LogCh:    make(chan string, 20),
		UpdateCh: make(chan bool, 1),
	}
	ch.PipelineQueue = NewPipelineQueue(ch)
	// Stop the queue up front: the flushed file is enqueued, hits the stopped
	// branch (saves recovery state, no DB client in tests), and returns — so
	// the test exercises the flush and move without running real uploads.
	ch.PipelineQueue.Stop()

	// Simulate the hold: a continuation cycle parked for a merge that never
	// came, exactly as trySessionMerge's continuation branch leaves it.
	MarkUploadInFlight(path)
	sessionMergeMu.Lock()
	sessionMergeByUser["alice"] = &sessionMergeEntry{path: path, endedContinuation: true, updatedAt: time.Now()}
	sessionMergeMu.Unlock()

	ch.ProcessPending()

	sessionMergeMu.Lock()
	e := sessionMergeByUser["alice"]
	sessionMergeMu.Unlock()
	if e != nil {
		t.Fatalf("session-merge hold not released: entry still present for alice (%s)", e.path)
	}
	if IsUploadInFlight(path) {
		t.Error("held file still marked in-flight after flush — re-claims would be dropped as duplicates")
	}
	dest := filepath.Join(server.Config.OutputDir, "alice_2025-01-01_12-00-00.mp4")
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("held file not moved to OutputDir: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("held file still in recording dir — flush did not move it")
	}
}

// TestSessionMergeFailureReleasesBothFiles locks the prev.path leak fix: when
// mergeTwoFiles fails, trySessionMerge must release the parked file's
// in-flight marker ("uploading separately" must include prev, or prev is
// stranded — marked in-flight, never enqueued, invisible to recovery scans).
func TestSessionMergeFailureReleasesBothFiles(t *testing.T) {
	oldConfig := server.Config
	defer func() { server.Config = oldConfig }()
	server.Config = &entity.Config{FinalizeMode: "remux", FFmpegContainer: "mp4"}

	dir := t.TempDir()
	prev := filepath.Join(dir, "bob_2025-01-01_12-00-00.mp4")
	final := filepath.Join(dir, "bob_2025-01-01_12-20-00.mp4")
	// Garbage bytes: ffmpeg fails on both, no ffmpeg binary needed for failure.
	for _, p := range []string{prev, final} {
		if err := os.WriteFile(p, []byte("not a video"), 0o666); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	ch := &Channel{
		Config:   &entity.ChannelConfig{Username: "bob"},
		LogCh:    make(chan string, 20),
		UpdateCh: make(chan bool, 1),
	}
	MarkUploadInFlight(prev)
	sessionMergeMu.Lock()
	sessionMergeByUser["bob"] = &sessionMergeEntry{path: prev, endedContinuation: true, updatedAt: time.Now()}
	sessionMergeMu.Unlock()

	consumed := ch.trySessionMerge(final, "stream session expired (no new segments)")
	if consumed {
		t.Fatal("trySessionMerge consumed the file despite merge failure")
	}
	sessionMergeMu.Lock()
	_, held := sessionMergeByUser["bob"]
	sessionMergeMu.Unlock()
	if held {
		t.Error("failed merge left the group in the map")
	}
	if IsUploadInFlight(prev) {
		t.Error("prev.path still marked in-flight after failed merge — it will never be uploaded (the leak)")
	}
	// Both inputs survive: nothing is lost by a failed merge.
	if _, err := os.Stat(prev); err != nil {
		t.Errorf("prev vanished after failed merge: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("final vanished after failed merge: %v", err)
	}
}

// TestRecoverConsumedMergeInputRestoresStrandedInput locks the re-merge fix:
// when a re-merge fails to publish AND to restore its prior merged file, the
// cumulative recording is parked under "<stable video>.consumed".  The orphan
// scan must restore it to its original name (only when that name is free) so it
// is uploaded normally instead of sitting hidden on disk forever.
func TestRecoverConsumedMergeInputRestoresStrandedInput(t *testing.T) {
	dir := t.TempDir()
	starter := filepath.Join(dir, "alice_2025-01-01_12-00-00.mp4.merged.mp4")
	consumed := starter + ".consumed"
	if err := os.WriteFile(consumed, []byte("pretend-merged-content"), 0o666); err != nil {
		t.Fatalf("write consumed stray: %v", err)
	}
	past := time.Now().Add(-(orphanSettleWindow + time.Minute))
	if err := os.Chtimes(consumed, past, past); err != nil {
		t.Fatalf("age consumed stray: %v", err)
	}

	got := recoverConsumedMergeInput(dir, filepath.Base(consumed))
	if got != filepath.Base(starter) {
		t.Fatalf("recoverConsumedMergeInput = %q, want %q", got, filepath.Base(starter))
	}
	if _, err := os.Stat(consumed); err == nil {
		t.Error("stranded input still at .consumed path — not renamed back")
	}
	if _, err := os.Stat(starter); err != nil {
		t.Errorf("stranded input not restored to stable name: %v", err)
	}
	if b, err := os.ReadFile(starter); err != nil || string(b) != "pretend-merged-content" {
		t.Errorf("restored file content = %q, err %v", b, err)
	}
}

// TestRecoverConsumedMergeInputDeduplicatesSuperseded verifies a .consumed
// stray whose published merge already occupies the stable name is removed
// (never restored over it, which on POSIX would overwrite the fresh merge).
func TestRecoverConsumedMergeInputDeduplicatesSuperseded(t *testing.T) {
	dir := t.TempDir()
	merged := filepath.Join(dir, "bob_2025-01-01_12-00-00.mp4.merged.mp4")
	if err := os.WriteFile(merged, []byte("published-merge"), 0o666); err != nil {
		t.Fatalf("write published merge: %v", err)
	}
	consumed := merged + ".consumed"
	if err := os.WriteFile(consumed, []byte("superseded-old-content"), 0o666); err != nil {
		t.Fatalf("write consumed duplicate: %v", err)
	}
	past := time.Now().Add(-(orphanSettleWindow + time.Minute))
	if err := os.Chtimes(consumed, past, past); err != nil {
		t.Fatalf("age consumed duplicate: %v", err)
	}

	if got := recoverConsumedMergeInput(dir, filepath.Base(consumed)); got != "" {
		t.Fatalf("superseded duplicate returned name %q, want \"\" (dedup)", got)
	}
	if _, err := os.Stat(consumed); err == nil {
		t.Error("superseded .consumed duplicate was not removed")
	}
	if _, err := os.Stat(merged); err != nil {
		t.Errorf("published merge must never be touched: %v", err)
	}
}

// TestRecoverConsumedMergeInputDefersFreshStray verifies a .consumed file
// touched within the settle window is left alone — a live re-merge may be
// staging it right now and racing it could clobber the in-flight publish.
func TestRecoverConsumedMergeInputDefersFreshStray(t *testing.T) {
	dir := t.TempDir()
	consumed := filepath.Join(dir, "carol_2025-01-01_12-00-00.mp4.merged.mp4.consumed")
	if err := os.WriteFile(consumed, []byte("x"), 0o666); err != nil {
		t.Fatalf("write fresh stray: %v", err)
	}
	if got := recoverConsumedMergeInput(dir, filepath.Base(consumed)); got != "" {
		t.Fatalf("fresh stray returned name %q, want \"\" (deferred)", got)
	}
	if _, err := os.Stat(consumed); err != nil {
		t.Errorf("fresh stray must be left for a later scan: %v", err)
	}
}

// TestContinuationClassification locks the fix for ~20-minute fragmentation:
// Chaturbate's HLS token rotates every ~20 minutes, surfacing as a stall whose
// reason is "stream session expired (no new segments)" (or "...— reconnecting").
// Both must be treated as a continuation of the SAME live session (so fragments
// merge into one long recording), NOT as a session end. Definitive ends
// (offline, private show, max duration, handoff, unknown) must remain stops so a
// merge flushes instead of holding forever.
func TestContinuationClassification(t *testing.T) {
	continuations := []string{
		"stream session expired (no new segments)",
		"stream session expired (HLS session/token) — reconnecting",
		"stream session expired (HLS session/token) — reconnecting (site probe failed: 502)",
	}
	for _, r := range continuations {
		if !isContinuationReason(r) {
			t.Errorf("isContinuationReason(%q) = false, want true (fragment would be uploaded alone)", r)
		}
		if isDefinitiveStop(r) {
			t.Errorf("isDefinitiveStop(%q) = true, want false (would not merge with next cycle)", r)
		}
	}

	stops := []string{
		"channel went offline",
		"channel entered a private show",
		"max duration or filesize reached",
		"channel stopped (handoff)",
		"stream ended normally",
		"unknown",
	}
	for _, r := range stops {
		if isContinuationReason(r) {
			t.Errorf("isContinuationReason(%q) = true, want false", r)
		}
		if !isDefinitiveStop(r) {
			t.Errorf("isDefinitiveStop(%q) = false, want true (merge would never flush)", r)
		}
	}
}
