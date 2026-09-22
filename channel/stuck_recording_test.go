package channel

import (
	"testing"

	"github.com/teacat/chaturbate-dvr/entity"
)

// TestReapStuckRecordingRequestsRestartAndCancels pins the reaper contract: a
// wedged run is unwound by cancel (which is what makes the monitor's deferred
// Cleanup close and delete the zero-byte file), and the restart must be
// requested BEFORE the cancel so finishMonitor spawns a fresh monitor instead
// of leaving the channel stopped.
func TestReapStuckRecordingRequestsRestartAndCancels(t *testing.T) {
	cancelled := 0
	ch := &Channel{
		Config:     &entity.ChannelConfig{Username: "alice"},
		LogCh:      make(chan string, 8),
		UpdateCh:   make(chan bool, 1),
		CancelFunc: func() { cancelled++ },
	}
	ch.monitorMu.Lock()
	ch.monitorRunning = true
	ch.monitorMu.Unlock()

	const reason = "stuck recording reaped: stream never started (no data written)"
	if !ch.ReapStuckRecording(reason) {
		t.Fatal("ReapStuckRecording = false, want true while a monitor is running")
	}

	ch.monitorMu.Lock()
	restart := ch.monitorRestartRequested
	ch.monitorMu.Unlock()
	if !restart {
		t.Error("monitorRestartRequested = false; finishMonitor would leave the channel stopped")
	}
	if cancelled != 1 {
		t.Errorf("CancelFunc calls = %d, want 1 (the wedged run must be unwound)", cancelled)
	}
	if got := ch.closeReason; got != reason {
		t.Errorf("closeReason = %q, want %q", got, reason)
	}
	// A reap is not a pause: the channel must still be eligible to record.
	if ch.Config.IsPaused.Load() {
		t.Error("reap must not pause the channel")
	}
}

// TestReapStuckRecordingNoMonitorIsNoop verifies a channel that is not running a
// monitor is left alone — the watchdog must never resurrect a torn-down channel.
func TestReapStuckRecordingNoMonitorIsNoop(t *testing.T) {
	cancelled := 0
	ch := &Channel{
		Config:     &entity.ChannelConfig{Username: "bob"},
		LogCh:      make(chan string, 8),
		UpdateCh:   make(chan bool, 1),
		CancelFunc: func() { cancelled++ },
	}

	if ch.ReapStuckRecording("whatever") {
		t.Error("ReapStuckRecording = true with no monitor running, want false")
	}
	if cancelled != 0 {
		t.Errorf("CancelFunc calls = %d, want 0 when no monitor is running", cancelled)
	}
	ch.monitorMu.Lock()
	restart := ch.monitorRestartRequested
	ch.monitorMu.Unlock()
	if restart {
		t.Error("a no-op reap must not request a restart")
	}
}

// TestCurrentRecordingPathRequiresOpenFile documents the gate the watchdog uses:
// no open file means nothing is recording, so nothing can be wedged.
func TestCurrentRecordingPathRequiresOpenFile(t *testing.T) {
	ch := &Channel{Config: &entity.ChannelConfig{Username: "carol"}}
	if got := ch.CurrentRecordingPath(); got != "" {
		t.Errorf("CurrentRecordingPath with no open file = %q, want \"\"", got)
	}
}
