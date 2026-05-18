package channel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/teacat/chaturbate-dvr/chaturbate"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// Channel represents a channel instance.
type Channel struct {
	CancelFunc      context.CancelFunc
	PauseCancelFunc context.CancelFunc
	LogCh           chan string
	UpdateCh        chan bool

	IsOnline   bool
	RoomStatus string // public, private, group, away, offline
	StreamedAt int64
	Duration   float64 // Seconds
	Filesize   int     // Bytes
	Sequence   int

	RoomTitle  string   // captured from API at recording start
	Tags       []string // captured from API at recording start
	Viewers    int      // captured from API at recording start
	Resolution string   // actual stream resolution (e.g. "1920x1080")
	Framerate  int      // actual stream framerate (e.g. 30)

	Logs    []string
	logsMu  sync.Mutex

	File             *os.File
	AudioFile        *os.File
	Config           *entity.ChannelConfig
	CurrentFilename  string
	InitSegment      []byte // fMP4 video init segment for LL-HLS streams
	AudioInitSegment []byte // fMP4 audio init segment for LL-HLS streams
	HasSeparateAudio bool
	switchRequested  bool       // set by HandleSegment, consumed by OnPollComplete
	cleanupMu        sync.Mutex // serialises Cleanup() calls from concurrent goroutines
}

// New creates a new channel instance with the given manager and configuration.
func New(conf *entity.ChannelConfig) *Channel {
	ch := &Channel{
		LogCh:           make(chan string),
		UpdateCh:        make(chan bool),
		Config:          conf,
		CancelFunc:      func() {},
		PauseCancelFunc: func() {},
	}
	go ch.Publisher()

	return ch
}

// Publisher listens for log messages and updates from the channel
// and publishes once received.
func (ch *Channel) Publisher() {
	for {
		select {
		case v := <-ch.LogCh:
			ch.logsMu.Lock()
			ch.Logs = append(ch.Logs, v)
			if len(ch.Logs) > 100 {
				ch.Logs = ch.Logs[len(ch.Logs)-100:]
			}
			ch.logsMu.Unlock()
			server.Manager.Publish(entity.EventLog, ch.ExportInfo())

		case <-ch.UpdateCh:
			server.Manager.Publish(entity.EventUpdate, ch.ExportInfo())
		}
	}
}

// WithCancel creates a new context with a cancel function,
// then stores the cancel function in the channel's CancelFunc field.
//
// This is used to cancel the context when the channel is stopped or paused.
func (ch *Channel) WithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, ch.CancelFunc = context.WithCancel(ctx)
	return ctx, ch.CancelFunc
}

// Info logs an informational message.
func (ch *Channel) Info(format string, a ...any) {
	ch.LogCh <- fmt.Sprintf("%s [INFO] %s", time.Now().Format("15:04"), fmt.Sprintf(format, a...))
	log.Printf(" INFO [%s] %s", ch.Config.Username, fmt.Sprintf(format, a...))
}

// Error logs an error message.
func (ch *Channel) Error(format string, a ...any) {
	ch.LogCh <- fmt.Sprintf("%s [ERROR] %s", time.Now().Format("15:04"), fmt.Sprintf(format, a...))
	log.Printf("ERROR [%s] %s", ch.Config.Username, fmt.Sprintf(format, a...))
}

// ExportInfo exports the channel information as a ChannelInfo struct.
func (ch *Channel) ExportInfo() *entity.ChannelInfo {
	var filename string
	if ch.CurrentFilename != "" && ch.HasSeparateAudio {
		filename = ch.CurrentFilename + ".mp4"
	} else if ch.File != nil {
		filename = ch.File.Name()
	}
	var streamedAt string
	if ch.StreamedAt != 0 {
		streamedAt = time.Unix(ch.StreamedAt, 0).Format("2006-01-02 15:04 AM")
	}
	ch.logsMu.Lock()
	logsCopy := make([]string, len(ch.Logs))
	copy(logsCopy, ch.Logs)
	ch.logsMu.Unlock()

	return &entity.ChannelInfo{
		IsOnline:     ch.IsOnline,
		IsPaused:     ch.Config.IsPaused,
		RoomStatus:   ch.RoomStatus,
		Username:     ch.Config.Username,
		MaxDuration:  internal.FormatDuration(float64(ch.Config.MaxDuration * 60)),
		MaxFilesize:  internal.FormatFilesize(ch.Config.MaxFilesize * 1024 * 1024),
		StreamedAt:   streamedAt,
		CreatedAt:    ch.Config.CreatedAt,
		Duration:     internal.FormatDuration(ch.Duration),
		Filesize:     internal.FormatFilesize(ch.Filesize),
		Filename:     filename,
		Logs:         logsCopy,
		GlobalConfig: server.Config,
	}
}

// Pause pauses the channel and cancels the context.
func (ch *Channel) Pause() {
	// Stop the monitoring loop, this also updates `ch.IsOnline` to false
	// `context.Canceled` → `ch.Monitor()` → `onRetry` → `ch.UpdateOnlineStatus(false)`.
	ch.CancelFunc()

	ch.Config.IsPaused = true
	ch.Update()
	ch.Info("channel paused")

	// Finalize any in-progress files immediately so they can be uploaded
	// and removed when `DeleteLocalAfterUpload` is enabled.
	go func() {
		if err := ch.Cleanup(); err != nil {
			ch.Error("cleanup on pause: %s", err.Error())
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	ch.PauseCancelFunc = cancel
	go ch.CheckOnlineWhilePaused(ctx, 0)
}

// Stop stops the channel and cancels the context.
func (ch *Channel) Stop() {
	ch.CancelFunc()
	ch.PauseCancelFunc()
	ch.Info("channel stopped")
}

// Resume resumes the channel monitoring.
//
// `startSeq` is used to prevent all channels from starting at the same time, preventing TooManyRequests errors.
// It's only be used when program starting and trying to resume all channels at once.
func (ch *Channel) Resume(startSeq int) {
	ch.PauseCancelFunc()
	ch.Config.IsPaused = false

	ch.Update()
	ch.Info("channel resumed")

	<-time.After(time.Duration(startSeq) * time.Second)
	go ch.Monitor()
}

// UpdateOnlineStatus updates the online status of the channel.
func (ch *Channel) UpdateOnlineStatus(isOnline bool) {
	ch.IsOnline = isOnline
	ch.Update()
}

// CheckOnlineWhilePaused periodically refreshes room status for paused channels
// so the UI can still distinguish online/private/offline states.
func (ch *Channel) CheckOnlineWhilePaused(ctx context.Context, startSeq int) {
	client := chaturbate.NewClient()
	baseIntervalMinutes := max(server.Config.Interval, 15)
	cfBlockCount := 0

	initialDelay := time.Duration(startSeq*5) * time.Second
	if initialDelay > 0 {
		timer := time.NewTimer(initialDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}

	for {
		waitInterval := time.Duration(baseIntervalMinutes) * time.Minute

		status, err := client.GetRoomStatus(ctx, ch.Config.Username)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			if isCFBlock(err) {
				cfBlockCount++
				delayMinutes := cfBackoffMinutes(cfBlockCount, baseIntervalMinutes)
				waitInterval = time.Duration(delayMinutes) * time.Minute
				ch.Info("paused status check blocked by Cloudflare (attempt %d); retry in %d min(s)", cfBlockCount, delayMinutes)
			} else {
				cfBlockCount = 0
			}
		} else if status != "" {
			cfBlockCount = 0
			isOnline := status != chaturbate.StatusAway && status != chaturbate.StatusOffline
			if ch.IsOnline != isOnline || ch.RoomStatus != status {
				ch.IsOnline = isOnline
				ch.RoomStatus = status
				ch.Info("channel status: %s (paused)", status)
				ch.Update()
			}
		}

		timer := time.NewTimer(waitInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
