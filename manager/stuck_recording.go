package manager

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/channel"
)

// stuckRecordingGrace is how long a channel's open recording file may sit at
// zero bytes before the watchdog reaps the run.
//
// A file that never receives a byte is not "slow": the HLS poll is wedged (an
// edge/Cloudflare cut right after the session starts, or a fetch that never
// returns), so the normal stream-stall detection never fires and the retry loop
// never rotates the file.  Ten minutes is far longer than any legitimate gap
// between opening the output file and the first segment (a live playlist's head
// arrives within seconds) and far shorter than a session, so a wedged channel is
// recovered instead of holding a recording slot for the whole session.
const stuckRecordingGrace = 10 * time.Minute

// stuckRecordingInterval is how often the watchdog sweeps this node's channels.
const stuckRecordingInterval = 2 * time.Minute

// stuckRecordingCooldown is how long a reaped channel is left alone before it
// may be reaped again.  Each reap is a full stream re-resolve; a channel whose
// stream is genuinely gone must fall back to the normal offline interval rather
// than being restarted every sweep.
const stuckRecordingCooldown = 30 * time.Minute

var (
	stuckRecordingMu   sync.Mutex
	stuckRecordingLast = map[string]time.Time{}
)

// zeroByteReapDue reports whether an open recording file is a wedged run: it
// has received no data, and it has been empty for at least the grace window.
// Split out from the sweep so the threshold rule can be exercised without a
// live channel.
func zeroByteReapDue(size int64, mod, now time.Time) bool {
	return size == 0 && now.Sub(mod) >= stuckRecordingGrace
}

// reapCooldownElapsed reports whether a channel may be reaped again. A channel
// reaped recently is left to the normal retry cadence so a stream that is truly
// gone does not get restarted on every sweep.
func reapCooldownElapsed(last, now time.Time, seen bool) bool {
	return !seen || now.Sub(last) >= stuckRecordingCooldown
}

// startStuckRecordingWatchdog sweeps the node's channels on a fixed interval and
// reaps any recording that is wedged on a zero-byte output file.  It is
// independent of the orphan-cleanup ticker: the file that has to be cleared is
// owned by a live channel, so the orphan scan deliberately skips it and cannot
// fix this.
func startStuckRecordingWatchdog(m *Manager) {
	go func() {
		ticker := time.NewTicker(stuckRecordingInterval)
		defer ticker.Stop()
		for range ticker.C {
			m.ReapStuckRecordings()
		}
	}()
}

// ReapStuckRecordings restarts every channel whose current recording file has
// been zero bytes for longer than stuckRecordingGrace, clearing the empty file
// on the way out.  Returns the number of recordings reaped.
//
// Exported so the watchdog and an operator rescue action can share one path.
func (m *Manager) ReapStuckRecordings() int {
	now := time.Now()
	reaped := 0
	m.Channels.Range(func(_, value any) bool {
		ch, ok := value.(*channel.Channel)
		if !ok {
			return true
		}
		// A paused channel is not recording; never touch it.
		if ch.Config.IsPaused.Load() {
			return true
		}
		path := ch.CurrentRecordingPath()
		if path == "" {
			return true // no file open yet — nothing wedged
		}
		st, err := os.Stat(path)
		if err != nil || !zeroByteReapDue(st.Size(), st.ModTime(), now) {
			return true // no file, data is flowing, or still inside the grace window
		}
		age := now.Sub(st.ModTime())

		username := ch.Config.Username
		stuckRecordingMu.Lock()
		last, seen := stuckRecordingLast[username]
		stuckRecordingMu.Unlock()
		if !reapCooldownElapsed(last, now, seen) {
			return true // reaped recently; let the normal retry cadence handle it
		}

		if !ch.ReapStuckRecording("stuck recording reaped: stream never started (no data written)") {
			return true // monitor already gone
		}
		stuckRecordingMu.Lock()
		stuckRecordingLast[username] = now
		stuckRecordingMu.Unlock()
		reaped++
		ch.Warn("reaping stuck recording: %s held a 0-byte file for %s with no stream data — restarting to clear it",
			filepath.Base(path), age.Round(time.Second))
		return true
	})

	if reaped > 0 {
		log.Printf("[stuck-recording] reaped %d wedged recording(s) holding a zero-byte file for > %s", reaped, stuckRecordingGrace)
	}
	return reaped
}
