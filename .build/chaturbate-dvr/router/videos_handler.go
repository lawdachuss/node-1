package router

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

type RecordingEntry struct {
	Filename     string            `json:"filename"`
	Timestamp    string            `json:"timestamp"`
	RoomTitle    string            `json:"room_title"`
	Tags         []string          `json:"tags"`
	Viewers      int               `json:"viewers"`
	Resolution   string            `json:"resolution"`
	Framerate    int               `json:"framerate"`
	Links        map[string]string `json:"links"`
	ThumbnailURL string            `json:"thumbnail_url"`
	SpriteURL    string            `json:"sprite_url"`
	EmbedURL     string            `json:"embed_url"`
	Filesize     int64             `json:"filesize"`
}

type ChannelRecordings struct {
	Gender     string             `json:"gender"`
	Recordings []*RecordingEntry  `json:"recordings"`
}

type RecordingsDB struct {
	Version  int                          `json:"version"`
	Channels map[string]*ChannelRecordings `json:"channels"`
}

type VideoEntry struct {
	Username     string
	Filename     string
	FullPath     string
	Size         string
	ModTime      string
	ModTimeSort  string
	ThumbnailURL string
	SpriteURL    string
	IsOutputDir  bool
	Links       map[string]string
	Tags        []string
	RoomTitle   string
	Viewers     int
	Gender      string
	Resolution  string
	Framerate   int
}

type VideoGroup struct {
	Username      string
	Gender        string
	Videos        []*VideoEntry
	LatestModTime string
}

type VideosData struct {
	Config *entity.Config
	Videos []*VideoEntry
	Groups []VideoGroup
}

func Videos(c *gin.Context) {
	videos := scanVideos()
	groups := groupVideos(videos)

	c.HTML(200, "videos.html", &VideosData{
		Config: server.Config,
		Videos: videos,
		Groups: groups,
	})
}

func ChannelVideos(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.Redirect(http.StatusFound, "/videos")
		return
	}

	videos := scanVideos()
	filtered := make([]*VideoEntry, 0)
	for _, v := range videos {
		if strings.EqualFold(v.Username, name) {
			filtered = append(filtered, v)
		}
	}

	c.HTML(200, "channel.html", &VideosData{
		Config: server.Config,
		Videos: filtered,
		Groups: nil,
	})
}

var videoExts = map[string]bool{
	".ts":  true,
	".mp4": true,
	".mkv": true,
}

func scanVideos() []*VideoEntry {
	var entries []*VideoEntry
	seen := map[string]bool{}

	dirs := []string{"videos"}
	if server.Config != nil && server.Config.OutputDir != "" {
		dirs = append(dirs, server.Config.OutputDir)
	}

	for _, dir := range dirs {
		absDir, err := filepath.Abs(dir)
		if err != nil || seen[absDir] {
			continue
		}
		seen[absDir] = true
		entries = append(entries, walkDir(dir)...)
	}

	recordings := loadRecordings()
	linked := map[string]bool{}
	for _, e := range entries {
		linked[e.Filename] = true
	}
	for username, chanData := range recordings.Channels {
		for _, rec := range chanData.Recordings {
			filename := rec.Filename
			if linked[filename] {
				for _, e := range entries {
					if e.Filename == filename {
						e.Links = rec.Links
						e.Tags = rec.Tags
						e.RoomTitle = rec.RoomTitle
						e.Viewers = rec.Viewers
						e.Gender = chanData.Gender
						e.Resolution = rec.Resolution
						e.Framerate = rec.Framerate
						if rec.ThumbnailURL != "" {
							e.ThumbnailURL = rec.ThumbnailURL
						}
						if rec.SpriteURL != "" {
							e.SpriteURL = rec.SpriteURL
						}
					}
				}
				continue
			}
			fs := "uploaded"
			if rec.Filesize > 0 {
				fs = internal.FormatFilesize(int(rec.Filesize))
			}
			entries = append(entries, &VideoEntry{
				Username:     username,
				Filename:     filename,
				FullPath:     "",
				Size:         fs,
				ModTime:      rec.Timestamp,
				ModTimeSort:  rec.Timestamp,
				IsOutputDir:  false,
				Links:        rec.Links,
				Tags:         rec.Tags,
				RoomTitle:    rec.RoomTitle,
				Viewers:      rec.Viewers,
				Gender:       chanData.Gender,
				Resolution:   rec.Resolution,
				Framerate:    rec.Framerate,
				ThumbnailURL: rec.ThumbnailURL,
				SpriteURL:    rec.SpriteURL,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ModTime > entries[j].ModTime
	})
	return entries
}

func loadRecordings() *RecordingsDB {
	data, err := os.ReadFile("/database/recordings.json")
	if err != nil {
		return &RecordingsDB{Version: 2, Channels: map[string]*ChannelRecordings{}}
	}
	var db RecordingsDB
	if err := json.Unmarshal(data, &db); err != nil {
		return &RecordingsDB{Version: 2, Channels: map[string]*ChannelRecordings{}}
	}
	return &db
}

func walkDir(dir string) []*VideoEntry {
	var entries []*VideoEntry
	entries, _ = collectVideos(dir, entries)
	return entries
}

func collectVideos(dir string, entries []*VideoEntry) ([]*VideoEntry, error) {
	d, err := os.Open(dir)
	if err != nil {
		return entries, nil
	}
	defer d.Close()

	items, err := d.Readdir(-1)
	if err != nil {
		return entries, nil
	}

	for _, item := range items {
		full := filepath.Join(dir, item.Name())
		if item.IsDir() {
			entries, _ = collectVideos(full, entries)
			continue
		}
		ext := strings.ToLower(filepath.Ext(item.Name()))
		if !videoExts[ext] {
			continue
		}
		if strings.Contains(item.Name(), ".video.") || strings.Contains(item.Name(), ".audio.") {
			continue
		}

		username := extractUsername(item.Name())
		sizeStr := internal.FormatFilesize(int(item.Size()))
		if sizeStr == "" {
			sizeStr = "0 B"
		}
		modTime := item.ModTime().Format("2006-01-02 15:04")

		isOutput := false
		if server.Config != nil && server.Config.OutputDir != "" {
			absPath, _ := filepath.Abs(full)
			absOut, _ := filepath.Abs(server.Config.OutputDir)
			isOutput = strings.HasPrefix(absPath, absOut)
		}

		// Read thumbnail URL from .thumb sidecar
		thumbURL := ""
		if d, e := os.ReadFile(full + ".thumb"); e == nil {
			thumbURL = strings.TrimSpace(string(d))
		}
		// Read preview sprite URL from .sprite sidecar
		spriteURL := ""
		if d, e := os.ReadFile(full + ".sprite"); e == nil {
			spriteURL = strings.TrimSpace(string(d))
		}

		entries = append(entries, &VideoEntry{
			Username:     username,
			Filename:     item.Name(),
			FullPath:     full,
			Size:         sizeStr,
			ModTime:      modTime,
			ModTimeSort:  item.ModTime().Format(time.RFC3339),
			ThumbnailURL: thumbURL,
			SpriteURL:    spriteURL,
			IsOutputDir:  isOutput,
		})
	}
	return entries, nil
}

func extractUsername(filename string) string {
	if idx := strings.Index(filename, "_2"); idx > 0 {
		return filename[:idx]
	}
	if idx := strings.Index(filename, "_"); idx > 0 {
		return filename[:idx]
	}
	return "unknown"
}

func groupVideos(videos []*VideoEntry) []VideoGroup {
	byUser := map[string]*VideoGroup{}
	for _, v := range videos {
		g, ok := byUser[v.Username]
		if !ok {
			g = &VideoGroup{Username: v.Username, Gender: v.Gender}
			byUser[v.Username] = g
		}
		g.Videos = append(g.Videos, v)
		if v.ModTimeSort > g.LatestModTime {
			g.LatestModTime = v.ModTimeSort
		}
	}

	groups := make([]VideoGroup, 0, len(byUser))
	for _, g := range byUser {
		sort.Slice(g.Videos, func(i, j int) bool {
			return g.Videos[i].ModTimeSort > g.Videos[j].ModTimeSort
		})
		groups = append(groups, *g)
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].LatestModTime > groups[j].LatestModTime
	})
	return groups
}
