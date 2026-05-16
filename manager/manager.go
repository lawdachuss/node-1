package manager

import (
        "bytes"
        "encoding/json"
        "fmt"
        "net/http"
        "os"
        "path/filepath"
        "sort"
        "strings"
        "sync"

        "github.com/r3labs/sse/v2"
        "github.com/teacat/chaturbate-dvr/channel"
        "github.com/teacat/chaturbate-dvr/entity"
        "github.com/teacat/chaturbate-dvr/router/view"
)

// Manager is responsible for managing channels and their states.
type Manager struct {
        Channels sync.Map
        SSE      *sse.Server
}

// New initializes a new Manager instance with an SSE server.
func New() (*Manager, error) {

        server := sse.New()
        server.SplitData = true

        updateStream := server.CreateStream("updates")
        updateStream.AutoReplay = false

        return &Manager{
                SSE: server,
        }, nil
}

// SaveConfig saves the current channels and state to a JSON file.
func (m *Manager) SaveConfig() error {
        var config []*entity.ChannelConfig

        m.Channels.Range(func(key, value any) bool {
                config = append(config, value.(*channel.Channel).Config)
                return true
        })

        b, err := json.MarshalIndent(config, "", "  ")
        if err != nil {
                return fmt.Errorf("marshal: %w", err)
        }
        if err := os.MkdirAll("./conf", 0777); err != nil {
                return fmt.Errorf("mkdir all conf: %w", err)
        }
        if err := os.WriteFile("./conf/channels.json", b, 0777); err != nil {
                return fmt.Errorf("write file: %w", err)
        }
        return nil
}

// LoadConfig loads the channels from JSON and starts them.
// All channels are automatically resumed on startup, regardless of their paused state.
func (m *Manager) LoadConfig() error {
        b, err := os.ReadFile("./conf/channels.json")
        if os.IsNotExist(err) {
                return nil
        }
        if err != nil {
                return fmt.Errorf("read file: %w", err)
        }

        var config []*entity.ChannelConfig
        if err := json.Unmarshal(b, &config); err != nil {
                return fmt.Errorf("unmarshal: %w", err)
        }

        seq := 0
        for _, conf := range config {
                ch := channel.New(conf)
                m.Channels.Store(conf.Username, ch)

                // Automatically resume all channels on startup
                if ch.Config.IsPaused {
                        ch.Info("channel was paused, automatically resuming on startup")
                        ch.Config.IsPaused = false
                }
                go ch.Resume(seq)
                seq++
        }
        
        // Save the updated config to persist the resumed state
        if err := m.SaveConfig(); err != nil {
                return fmt.Errorf("save config after auto-resume: %w", err)
        }

        // Generate thumbnails for any existing videos that don't have one yet
        go m.ScanThumbnails()

        return nil
}

// ScanThumbnails walks the videos directory and generates thumbnails for any
// video file that is missing a .thumb sidecar file.
func (m *Manager) ScanThumbnails() {
        videoExts := map[string]bool{".mp4": true, ".mkv": true}
        dirs := []string{"videos"}

        for _, dir := range dirs {
                filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
                        if err != nil || info == nil || info.IsDir() {
                                return nil
                        }
                        ext := strings.ToLower(filepath.Ext(path))
                        if !videoExts[ext] {
                                return nil
                        }
                        // Skip segment sidecars
                        if strings.Contains(info.Name(), ".video.") || strings.Contains(info.Name(), ".audio.") {
                                return nil
                        }
                        // Only process files that are missing a .thumb sidecar
                        if _, statErr := os.Stat(path + ".thumb"); !os.IsNotExist(statErr) {
                                return nil
                        }
                        channel.GenerateThumbnailForFile(path)
                        return nil
                })
        }
}

// CreateChannel starts monitoring an M3U8 stream
func (m *Manager) CreateChannel(conf *entity.ChannelConfig, shouldSave bool) error {
        conf.Sanitize()
        ch := channel.New(conf)

        // prevent duplicate channels
        _, ok := m.Channels.Load(conf.Username)
        if ok {
                return fmt.Errorf("channel %s already exists", conf.Username)
        }
        m.Channels.Store(conf.Username, ch)

        go ch.Resume(0)

        if shouldSave {
                if err := m.SaveConfig(); err != nil {
                        return fmt.Errorf("save config: %w", err)
                }
        }
        return nil
}

// StopChannel stops the channel.
func (m *Manager) StopChannel(username string) error {
        thing, ok := m.Channels.Load(username)
        if !ok {
                return nil
        }
        thing.(*channel.Channel).Stop()
        m.Channels.Delete(username)

        if err := m.SaveConfig(); err != nil {
                return fmt.Errorf("save config: %w", err)
        }
        return nil
}

// PauseChannel pauses the channel.
func (m *Manager) PauseChannel(username string) error {
        thing, ok := m.Channels.Load(username)
        if !ok {
                return nil
        }
        thing.(*channel.Channel).Pause()

        if err := m.SaveConfig(); err != nil {
                return fmt.Errorf("save config: %w", err)
        }
        return nil
}

// ResumeChannel resumes the channel.
func (m *Manager) ResumeChannel(username string) error {
        thing, ok := m.Channels.Load(username)
        if !ok {
                return nil
        }
        thing.(*channel.Channel).Resume(0)

        if err := m.SaveConfig(); err != nil {
                return fmt.Errorf("save config: %w", err)
        }
        return nil
}

// ChannelInfo returns a list of channel information for the web UI.
func (m *Manager) ChannelInfo() []*entity.ChannelInfo {
        var channels []*entity.ChannelInfo

        // Iterate over the channels and append their information to the slice
        m.Channels.Range(func(key, value any) bool {
                channels = append(channels, value.(*channel.Channel).ExportInfo())
                return true
        })

        sort.Slice(channels, func(i, j int) bool {
                // Paused channels always sort to the bottom.
                getPriority := func(c *entity.ChannelInfo) int {
                        switch {
                        case !c.IsPaused && c.IsOnline:
                                return 0 // Recording
                        case !c.IsPaused:
                                return 1 // Offline, actively watching
                        case c.IsOnline:
                                return 2 // Paused, currently online
                        default:
                                return 3 // Paused, offline
                        }
                }

                pi, pj := getPriority(channels[i]), getPriority(channels[j])
                if pi != pj {
                        return pi < pj
                }
                // Same priority: sort by username alphabetically
                return strings.ToLower(channels[i].Username) < strings.ToLower(channels[j].Username)
        })

        return channels
}

// Publish sends an SSE event to the specified channel.
func (m *Manager) Publish(evt entity.Event, info *entity.ChannelInfo) {
        switch evt {
        case entity.EventUpdate:
                var b bytes.Buffer
                if err := view.InfoTpl.ExecuteTemplate(&b, "channel_info", info); err != nil {
                        fmt.Println("Error executing template:", err)
                        return
                }
                m.SSE.Publish("updates", &sse.Event{
                        Event: []byte(info.Username + "-info"),
                        Data:  b.Bytes(),
                })
        case entity.EventLog:
                if len(info.Logs) > 0 {
                        m.SSE.Publish("updates", &sse.Event{
                                Event: []byte(info.Username + "-log"),
                                Data:  []byte(info.Logs[len(info.Logs)-1]),
                        })
                }
        }
}

// Subscriber handles SSE subscriptions for the specified channel.
func (m *Manager) Subscriber(w http.ResponseWriter, r *http.Request) {
        m.SSE.ServeHTTP(w, r)
}
