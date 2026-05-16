package channel

import (
	"path/filepath"

	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

// uploadFile uploads the given file to all configured hosts.
// It uses the channel's logging so upload events appear in the UI logs.
// GoFile always uploads (no API key needed).
// Other services upload only if their API key is configured.
func (ch *Channel) uploadFile(filePath string) {
	cfg := server.Config
	if cfg == nil {
		return
	}

	ch.Info("upload: starting upload of %s", filepath.Base(filePath))

	// Create the uploader with the channel as its logger
	upl := uploader.NewMultiHostUploader(
		cfg.TurboViPlayAPIKey,
		cfg.VoeSXAPIKey,
		cfg.StreamtapeLogin,
		cfg.StreamtapeAPIKey,
		cfg.SendCMAPIKey,
		cfg.ByseAPIKey,
		ch, // Channel implements uploader.Logger
	)

	results := upl.UploadToAll(filePath)
	success := uploader.GetSuccessfulUploads(results)
	if len(results) > 0 {
		ch.Info("upload: finished — %d/%d successful", len(success), len(results))
		if len(success) == 0 {
			ch.Error("upload: all hosts failed for %s", filepath.Base(filePath))
		}
	}
}

// Ensure Channel implements uploader.Logger.
var _ uploader.Logger = (*Channel)(nil)
