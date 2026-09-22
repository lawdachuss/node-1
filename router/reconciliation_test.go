package router

import (
	"testing"
	"time"
)

// TestIsZeroByteStuck pins the rule that makes a wedged recording visible: a
// zero-byte file with no cloud metadata counts as stuck once it is past the
// threshold AND no channel is writing to it.  Counting these is what turns the
// node verdict critical instead of the HEALTHY reading that let 25 dead files
// accumulate unnoticed.
func TestIsZeroByteStuck(t *testing.T) {
	now := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		filename   string
		mod        time.Time
		activeRecs map[string]bool
		want       bool
	}{
		{
			name:     "empty and past the threshold",
			filename: "diane_fox_2026-09-22_05-01-07.mp4",
			mod:      now.Add(-zeroByteStuckThreshold - time.Minute),
			want:     true,
		},
		{
			name:     "empty but a channel is writing to it right now",
			filename: "live_2026-09-22_05-01-07.mp4",
			mod:      now.Add(-time.Hour),
			activeRecs: map[string]bool{
				"live_2026-09-22_05-01-07.mp4": true,
			},
			want: false,
		},
		{
			name:     "empty but still inside the threshold",
			filename: "fresh_2026-09-22_04-59-00.mp4",
			mod:      now.Add(-time.Minute),
			want:     false,
		},
		{
			name:     "empty just inside the threshold",
			filename: "edge_2026-09-22_04-59-00.mp4",
			mod:      now.Add(-zeroByteStuckThreshold + time.Second),
			want:     false,
		},
		{
			name:     "empty exactly at the threshold",
			filename: "edge2_2026-09-22_04-59-00.mp4",
			mod:      now.Add(-zeroByteStuckThreshold),
			want:     false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isZeroByteStuck(c.filename, c.mod, now, c.activeRecs); got != c.want {
				t.Errorf("isZeroByteStuck(%q, age=%s) = %v, want %v", c.filename, now.Sub(c.mod), got, c.want)
			}
		})
	}
}

// TestIsZeroByteStuckNilActiveSet ensures the walk cannot panic when the manager
// is not wired up (early startup, isolated mode without a node manager).
func TestIsZeroByteStuckNilActiveSet(t *testing.T) {
	now := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	if !isZeroByteStuck("x.mp4", now.Add(-time.Hour), now, nil) {
		t.Error("isZeroByteStuck with a nil active set = false, want true")
	}
}
