package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teacat/chaturbate-dvr/channel"
	"github.com/teacat/chaturbate-dvr/entity"
)

// TestZeroByteReapDue pins the watchdog's threshold rule: only a file that has
// received NOTHING and has been empty for at least the grace window is a wedged
// recording. A file with data, or a freshly created one, must never be reaped.
func TestZeroByteReapDue(t *testing.T) {
	now := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		size int64
		mod  time.Time
		want bool
	}{
		{"empty past the grace window", 0, now.Add(-stuckRecordingGrace - time.Minute), true},
		{"empty exactly at the grace window", 0, now.Add(-stuckRecordingGrace), true},
		{"empty but still filling (fresh)", 0, now.Add(-30 * time.Second), false},
		{"empty just inside the grace window", 0, now.Add(-stuckRecordingGrace + time.Second), false},
		{"has data, however old", 4096, now.Add(-3 * time.Hour), false},
		{"has data and fresh", 1, now.Add(-time.Second), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := zeroByteReapDue(c.size, c.mod, now); got != c.want {
				t.Errorf("zeroByteReapDue(%d bytes, %s) = %v, want %v", c.size, now.Sub(c.mod), got, c.want)
			}
		})
	}
}

// TestReapCooldownElapsed verifies a reaped channel is left alone until the
// cooldown passes, so a stream that is genuinely gone falls back to the normal
// retry cadence instead of being restarted on every sweep.
func TestReapCooldownElapsed(t *testing.T) {
	now := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		last time.Time
		seen bool
		want bool
	}{
		{"never reaped", time.Time{}, false, true},
		{"reaped long ago", now.Add(-stuckRecordingCooldown - time.Minute), true, true},
		{"reaped exactly at the cooldown", now.Add(-stuckRecordingCooldown), true, true},
		{"reaped a minute ago", now.Add(-time.Minute), true, false},
		{"reaped just inside the cooldown", now.Add(-stuckRecordingCooldown + time.Second), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reapCooldownElapsed(c.last, now, c.seen); got != c.want {
				t.Errorf("reapCooldownElapsed(seen=%v, age=%s) = %v, want %v", c.seen, now.Sub(c.last), got, c.want)
			}
		})
	}
}

// TestReapStuckRecordingsEmptyManagerIsSafe guards the sweep against a manager
// with no channels (and against running before any channel exists).
func TestReapStuckRecordingsEmptyManagerIsSafe(t *testing.T) {
	m := &Manager{}
	if got := m.ReapStuckRecordings(); got != 0 {
		t.Errorf("ReapStuckRecordings on an empty manager = %d, want 0", got)
	}
}

// newSweepChannel returns a channel with a real, open, empty recording file aged
// past the grace window — the exact shape the watchdog exists to clear.
func newSweepChannel(t *testing.T, dir, username string) *channel.Channel {
	t.Helper()
	ch := channel.New(&entity.ChannelConfig{
		Username: username,
		Site:     "chaturbate",
		Pattern:  filepath.Join(dir, "{{.Username}}_{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}"),
	})
	// RecordStream sets FileExt from the playlist before calling NextFile; the
	// path accessor needs it to know the on-disk suffix.
	ch.FileExt = ".mp4"
	if err := ch.NextFile(ch.FileExt); err != nil {
		t.Fatalf("NextFile: %v", err)
	}
	path := ch.CurrentRecordingPath()
	old := time.Now().Add(-2 * stuckRecordingGrace)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// Close the handle before the test's TempDir cleanup runs: on Windows an
	// open file cannot be unlinked, which fails the test at teardown.
	t.Cleanup(func() { _ = ch.Cleanup(channel.CloseQueue) })
	return ch
}

// TestReapStuckRecordingsSeesWedgedFileButOnlyReapsRunningMonitors verifies the
// sweep's selection and its safety bound: it finds the aged zero-byte file, but
// a channel with no monitor running reports false from the reap primitive, so
// the sweep must not claim it reaped anything (and must not resurrect a
// torn-down channel).
func TestReapStuckRecordingsSeesWedgedFileButOnlyReapsRunningMonitors(t *testing.T) {
	ch := newSweepChannel(t, t.TempDir(), "alice")
	path := ch.CurrentRecordingPath()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if st.Size() != 0 {
		t.Fatalf("test fixture file is %d bytes, want 0", st.Size())
	}
	if !zeroByteReapDue(st.Size(), st.ModTime(), time.Now()) {
		t.Fatal("fixture is not past the reap grace window; the sweep could not reach the reap path")
	}

	m := &Manager{}
	m.Channels.Store("alice", ch)
	if got := m.ReapStuckRecordings(); got != 0 {
		t.Errorf("reaped = %d, want 0 while no monitor is running", got)
	}

	// The empty file must survive: only a real reap may delete it.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("sweep removed the file without a running monitor: %v", err)
	}
}

// TestReapStuckRecordingsSkipsPausedChannel verifies a paused channel is never
// touched, even with an aged empty file on disk.
func TestReapStuckRecordingsSkipsPausedChannel(t *testing.T) {
	ch := newSweepChannel(t, t.TempDir(), "bob")
	ch.Config.IsPaused.Store(true)

	m := &Manager{}
	m.Channels.Store("bob", ch)
	if got := m.ReapStuckRecordings(); got != 0 {
		t.Errorf("reaped = %d, want 0 for a paused channel", got)
	}
}
