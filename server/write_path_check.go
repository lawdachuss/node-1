package server

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/database"
)

// LinkPathWriteAlert mirrors DiskAlert: main wires it to notifier.Notify so this
// package stays free of the circular import (notifier imports server).
// Signature: (key, title, message) — the key carries the notifier's cooldown.
var LinkPathWriteAlert func(key, title, message string)

// LinkPathWriteRecovered clears that cooldown, so a write path that breaks again
// after being repaired is reported immediately instead of staying silent for the
// remainder of the previous alert's cooldown.
var LinkPathWriteRecovered func(key string)

const (
	// keyUploadLinkWriteFailure is the alert/cooldown key.  Kept in sync with
	// notifier.KeyUploadLinkWriteFailure by hand — same as the disk keys, because
	// server cannot import notifier.
	keyUploadLinkWriteFailure = "upload_link_write_failure"
	// probeUploadLinkHost is the reserved host value of the canary's row.  Nothing
	// else ever writes it, so the row is always identifiable and removable.
	probeUploadLinkHost = "__write_path_probe__"
	// probeUploadLinkRecordingID is the canary's recording id.  All-zeroes on
	// purpose: it can never collide with a real recording, so the site's
	// AFTER INSERT notification trigger takes its "recording not found" early
	// return and the probe never fabricates a user notification.
	probeUploadLinkRecordingID = "00000000-0000-0000-0000-000000000000"
)

var (
	writePathMu        sync.Mutex
	writePathErr       error
	writePathCheckedAt time.Time
)

// WritePathStatus is the outcome of the most recent canary run.
type WritePathStatus struct {
	Checked bool
	OK      bool
	Detail  string
	At      time.Time
}

// VerifyUploadLinkWritePath proves this node can persist an upload link, by
// writing one synthetic upload_links row and removing it again.
//
// It guards a failure mode that is otherwise invisible.  An upload link is
// written by a background pipeline step whose failure is a log line nobody reads,
// while the recording keeps the row it was given at ENQUEUE time — so the fleet
// looks healthy, the files really are on the hosts, and every recording made in
// the meantime is stored with zero hosts.  On 2026-09-23 an AFTER INSERT trigger
// on public.upload_links whose function could not even be planned
// (`operator does not exist: uuid = text`) rejected EVERY insert with 42883 for
// most of a day; the fleet recorded and uploaded throughout, and the admin panel
// only counted recordings with no host after the fact.
//
// Only a real INSERT catches that class.  A read-only health check, the PostgREST
// schema cache and `select 1` all stay green while the write path is closed.
func VerifyUploadLinkWritePath() error {
	client := GetDBClient()
	if client == nil {
		return fmt.Errorf("supabase not configured")
	}

	probe := &database.UploadLink{
		RecordingID: probeUploadLinkRecordingID,
		Host:        probeUploadLinkHost,
		URL:         "https://example.invalid/upload-link-write-path-probe",
		InstanceID:  DBInstanceID(),
	}
	if err := client.SaveUploadLink(probe); err != nil {
		return fmt.Errorf("upload_links insert rejected: %w", err)
	}

	// Delete by host, not by the probe's id: that also clears a probe row left
	// behind by a run killed between its insert and its delete.
	if err := client.DeleteUploadLinksByHost(probeUploadLinkHost); err != nil {
		return fmt.Errorf("probe row could not be removed: %w", err)
	}
	return nil
}

// CheckUploadLinkWritePath runs the canary, caches the outcome for the admin
// panel and alerts the fleet when the write path is closed.  A success clears the
// alert cooldown, so a recurrence is reported at once.
func CheckUploadLinkWritePath() error {
	err := VerifyUploadLinkWritePath()

	writePathMu.Lock()
	writePathErr, writePathCheckedAt = err, time.Now()
	writePathMu.Unlock()

	if err == nil {
		if LinkPathWriteRecovered != nil {
			LinkPathWriteRecovered(keyUploadLinkWriteFailure)
		}
		return nil
	}

	alertUploadLinkWriteFailure(
		"The DVR fleet cannot persist upload_links rows, so every new recording will be stored with no host "+
			"(check triggers/functions on public.upload_links and that migration 20260923010000 is applied)", err)
	return err
}

// writePathPreflightMaxAge is how long a canary result is trusted before the
// sweep insists on a fresh one.  It exists so a startup check or the maintenance
// ticker pre-warms the answer and the sweep that follows reuses it instead of
// writing a second probe row.
const writePathPreflightMaxAge = 5 * time.Minute

// uploadLinkWritePathReady reports whether links can be written right now, running
// the canary only when no recent check answers it.
func uploadLinkWritePathReady() bool {
	if st := UploadLinkWritePathStatus(); st.Checked && st.OK && time.Since(st.At) < writePathPreflightMaxAge {
		return true
	}
	return CheckUploadLinkWritePath() == nil
}

// UploadLinkWritePathStatus reports the last canary outcome for the admin panel.
// Checked is false until the first run, so an unreported path is never shown as
// healthy.
func UploadLinkWritePathStatus() WritePathStatus {
	writePathMu.Lock()
	defer writePathMu.Unlock()
	st := WritePathStatus{Checked: !writePathCheckedAt.IsZero(), At: writePathCheckedAt}
	if writePathErr != nil {
		st.Detail = writePathErr.Error()
		return st
	}
	st.OK = st.Checked
	return st
}

// alertUploadLinkWriteFailure logs and dispatches a write-path alert.  Split out
// so the reconcile sweep can report the same condition under the same cooldown
// key instead of inventing a second alert stream.
func alertUploadLinkWriteFailure(reason string, err error) {
	log.Printf("[ALERT] %s: %v", reason, err)
	if LinkPathWriteAlert != nil {
		LinkPathWriteAlert(keyUploadLinkWriteFailure, "🚨 Upload links cannot be saved",
			fmt.Sprintf("%s — %v", reason, err))
	}
}
