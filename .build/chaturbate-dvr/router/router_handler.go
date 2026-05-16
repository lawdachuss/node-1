package router

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// IndexData represents the data structure for the index page.
type IndexData struct {
	Config   *entity.Config
	Channels []*entity.ChannelInfo
	Disk     *entity.DiskInfo
}

// Index renders the index page with channel information.
func Index(c *gin.Context) {
	c.HTML(200, "index.html", &IndexData{
		Config:   server.Config,
		Channels: server.Manager.ChannelInfo(),
		Disk:     server.GetDiskInfo(),
	})
}

// CreateChannelRequest represents the request body for creating a channel.
type CreateChannelRequest struct {
	Username    string `form:"username" binding:"required"`
	Framerate   int    `form:"framerate" binding:"required"`
	Resolution  int    `form:"resolution" binding:"required"`
	Pattern     string `form:"pattern" binding:"required"`
	MaxDuration int    `form:"max_duration"`
	MaxFilesize int    `form:"max_filesize"`
	Compress    bool   `form:"compress"`
}

// CreateChannel creates a new channel.
func CreateChannel(c *gin.Context) {
	var req *CreateChannelRequest
	if err := c.Bind(&req); err != nil {
		c.AbortWithError(http.StatusBadRequest, fmt.Errorf("bind: %w", err))
		return
	}

	for _, username := range strings.Split(req.Username, ",") {
		server.Manager.CreateChannel(&entity.ChannelConfig{
			IsPaused:    false,
			Username:    username,
			Framerate:   req.Framerate,
			Resolution:  req.Resolution,
			Pattern:     req.Pattern,
			MaxDuration: req.MaxDuration,
			MaxFilesize: req.MaxFilesize,
			Compress:    req.Compress,
			CreatedAt:   time.Now().Unix(),
		}, true)
	}
	c.Redirect(http.StatusFound, "/")
}

// StopChannel stops a channel.
func StopChannel(c *gin.Context) {
	server.Manager.StopChannel(c.Param("username"))

	c.Redirect(http.StatusFound, "/")
}

// PauseChannel pauses a channel.
func PauseChannel(c *gin.Context) {
	server.Manager.PauseChannel(c.Param("username"))

	c.Redirect(http.StatusFound, "/")
}

// ResumeChannel resumes a paused channel.
func ResumeChannel(c *gin.Context) {
	server.Manager.ResumeChannel(c.Param("username"))

	c.Redirect(http.StatusFound, "/")
}

// Updates handles the SSE connection for updates.
func Updates(c *gin.Context) {
	server.Manager.Subscriber(c.Writer, c.Request)
}

// UpdateConfigRequest represents the request body for updating configuration.
type UpdateConfigRequest struct {
	Cookies   string `form:"cookies"`
	UserAgent string `form:"user_agent"`
}

// UpdateConfig updates the server configuration.
func UpdateConfig(c *gin.Context) {
	var req *UpdateConfigRequest
	if err := c.Bind(&req); err != nil {
		c.AbortWithError(http.StatusBadRequest, fmt.Errorf("bind: %w", err))
		return
	}

	server.Config.Cookies = req.Cookies
	server.Config.UserAgent = req.UserAgent
	c.Redirect(http.StatusFound, "/")
}

// Download serves a video file for download.
func Download(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	// Basic path traversal prevention
	abs, err := filepath.Abs(path)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	c.FileAttachment(abs, filepath.Base(abs))
}

// DeleteVideo removes a video file from disk.
func DeleteVideo(c *gin.Context) {
	path := c.PostForm("path")
	if path == "" {
		c.Redirect(http.StatusFound, "/videos")
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		c.Redirect(http.StatusFound, "/videos")
		return
	}
	// Prevent deleting files outside allowed directories
	if server.Config != nil {
		allowed := false
		// Check videos/ directory
		videosAbs, _ := filepath.Abs("videos")
		if videosAbs != "" && strings.HasPrefix(abs, videosAbs) {
			allowed = true
		}
		// Check OutputDir
		if !allowed && server.Config.OutputDir != "" {
			outAbs, _ := filepath.Abs(server.Config.OutputDir)
			if outAbs != "" && strings.HasPrefix(abs, outAbs) {
				allowed = true
			}
		}
		if !allowed {
			c.Redirect(http.StatusFound, "/videos")
			return
		}
	}
	os.Remove(abs)
	c.Redirect(http.StatusFound, "/videos")
}

// Play streams a video file with Range header support for seeking.
func Play(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	file, err := os.Open(abs)
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	fileSize := stat.Size()

	// Detect MIME type from extension
	mimeType := detectVideoMIME(abs)
	rangeHeader := c.GetHeader("Range")
	c.Header("Accept-Ranges", "bytes")
	c.Header("Cache-Control", "no-cache")
	c.Header("Content-Type", mimeType)

	// Handle HEAD requests
	if c.Request.Method == http.MethodHead {
		c.Header("Content-Length", strconv.FormatInt(fileSize, 10))
		c.Status(http.StatusOK)
		return
	}

	if rangeHeader == "" {
		c.Header("Content-Length", strconv.FormatInt(fileSize, 10))
		c.Status(http.StatusOK)
		io.Copy(c.Writer, file)
		return
	}

	// Parse Range header: "bytes=start-end" or "bytes=start-"
	var start, end int64
	parsed := false
	if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err == nil {
		parsed = true
	} else if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-", &start); err == nil {
		parsed = true
		end = fileSize - 1
	}
	if !parsed {
		c.AbortWithStatus(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if start < 0 {
		start = 0
	}
	if end >= fileSize {
		end = fileSize - 1
	}
	if start > end {
		c.AbortWithStatus(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	contentLength := end - start + 1
	c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
	c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	c.Status(http.StatusPartialContent)

	file.Seek(start, 0)
	io.CopyN(c.Writer, file, contentLength)
}

func detectVideoMIME(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".ts":
		return "video/MP2T"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	default:
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
		return "video/mp4"
	}
}

// VideoDetail renders the video detail page with an embedded player.
func VideoDetail(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.Redirect(http.StatusFound, "/videos")
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		c.Redirect(http.StatusFound, "/videos")
		return
	}

	filename := filepath.Base(abs)
	username := extractUsername(filename)

	// Check if file still exists on disk
	stat, statErr := os.Stat(abs)
	fileOnDisk := statErr == nil

	// Read sidecar data (only for disk files)
	thumbURL := readSidecar(abs + ".thumb")
	spriteURL := readSidecar(abs + ".sprite")

	// Look up recording metadata from recordings DB
	db := loadRecordings()
	links := map[string]string{}
	tags := []string{}
	roomTitle := ""
	viewers := 0
	gender := ""
	filesize := int64(0)
	embedURL := ""
	dbThumbnailURL := ""
	dbSpriteURL := ""
	timestamp := ""
	resolution := ""
	framerate := 0
	var related []RecordingEntry
	foundInDB := false
	if db != nil {
		for _, chanData := range db.Channels {
			for _, rec := range chanData.Recordings {
				if rec.Filename == filename {
					foundInDB = true
					if rec.Links != nil {
						links = rec.Links
					}
					tags = rec.Tags
					roomTitle = rec.RoomTitle
					viewers = rec.Viewers
					gender = chanData.Gender
					filesize = rec.Filesize
					embedURL = rec.EmbedURL
					dbThumbnailURL = rec.ThumbnailURL
					dbSpriteURL = rec.SpriteURL
					timestamp = rec.Timestamp
					resolution = rec.Resolution
					framerate = rec.Framerate
					break
				}
			}
		}
		// Related: other recordings from the same channel
		if chanData, ok := db.Channels[username]; ok {
			for _, rec := range chanData.Recordings {
				if rec.Filename == filename {
					continue
				}
				if len(related) >= 4 {
					break
				}
				related = append(related, *rec)
			}
		}
		sort.Slice(related, func(i, j int) bool {
			return related[i].Timestamp > related[j].Timestamp
		})
	}

	// If file is not on disk AND not in DB, redirect
	if !fileOnDisk && !foundInDB {
		c.Redirect(http.StatusFound, "/videos")
		return
	}

	// Use DB thumbnail/sprite if sidecar files don't exist
	if thumbURL == "" && dbThumbnailURL != "" {
		thumbURL = dbThumbnailURL
	}
	if spriteURL == "" && dbSpriteURL != "" {
		spriteURL = dbSpriteURL
	}

	// If embed URL is empty, try to generate one from upload links
	if embedURL == "" {
		for _, link := range links {
			if strings.Contains(link, "byse.sx/") {
				if code := extractFileCode(link); code != "" {
					embedURL = "https://byse.sx/e/" + code
					break
				}
			}
			if strings.Contains(link, "streamtape.com/v/") {
				if code := extractStreamtapeCode(link); code != "" {
					embedURL = "https://streamtape.com/e/" + code
					break
				}
			}
			if strings.Contains(link, "voe.sx/") {
				if code := extractFileCode(link); code != "" {
					embedURL = "https://voe.sx/e/" + code
					break
				}
			}
		}
	}

	// Find a direct video URL from upload links (for native player)
	videoURL := ""
	if embedURL != "" {
		videoURL = embedURL
		// For byse, the /d/{code} URL may serve the video directly
		if strings.Contains(embedURL, "byse.sx/e/") {
			videoURL = strings.Replace(embedURL, "/e/", "/d/", 1)
		}
	}

	// Build template vars
	fullPath := ""
	size := ""
	modTime := ""
	mimeType := "video/mp4"
	if fileOnDisk {
		fullPath = abs
		size = internal.FormatFilesize(int(stat.Size()))
		modTime = stat.ModTime().Format("2006-01-02 15:04")
		mimeType = detectVideoMIME(abs)
	} else if foundInDB {
		if filesize > 0 {
			size = internal.FormatFilesize(int(filesize))
		} else {
			size = "uploaded"
		}
		if timestamp != "" {
			if t, err := time.Parse("2006-01-02T15:04:05Z", timestamp); err == nil {
				modTime = t.Format("2006-01-02 15:04")
			} else {
				modTime = timestamp
			}
		}
	}

	c.HTML(200, "video.html", gin.H{
		"Config":       server.Config,
		"Filename":     filename,
		"FullPath":     fullPath,
		"VideoURL":     videoURL,
		"Size":         size,
		"ModTime":      modTime,
		"Username":     username,
		"ThumbnailURL": thumbURL,
		"SpriteURL":    spriteURL,
		"MimeType":     mimeType,
		"Links":        links,
		"Tags":         tags,
		"RoomTitle":    roomTitle,
		"Viewers":      viewers,
		"Gender":       gender,
		"Resolution":   resolution,
		"Framerate":    framerate,
		"Related":      related,
		"EmbedURL":     embedURL,
	})
}

func readSidecar(path string) string {
	if d, e := os.ReadFile(path); e == nil {
		return strings.TrimSpace(string(d))
	}
	return ""
}

func extractFileCode(link string) string {
	parts := strings.Split(strings.TrimRight(link, "/"), "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return ""
}

func extractStreamtapeCode(link string) string {
	idx := strings.Index(link, "/v/")
	if idx < 0 {
		return ""
	}
	afterV := link[idx+3:]
	parts := strings.SplitN(afterV, "/", 2)
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return ""
}
