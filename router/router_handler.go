package router

import (
        "encoding/json"
        "fmt"
        "html/template"
        "io"
        "mime"
        "net/http"
        "net/url"
        "os"
        "path/filepath"
        "sort"
        "strconv"
        "strings"
        "sync"
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

type hostPlayer struct {
        Host     string `json:"host"`
        Link     string `json:"link"`
        EmbedURL string `json:"embedUrl,omitempty"`
        VideoURL string `json:"videoUrl,omitempty"`
}

var (
        byseEmbedDomainMu        sync.RWMutex
        byseEmbedDomain          string
        byseEmbedDomainFetchedAt time.Time
)

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

	var lastErr error
	for _, username := range strings.Split(req.Username, ",") {
		if err := server.Manager.CreateChannel(&entity.ChannelConfig{
			IsPaused:    false,
			Username:    username,
			Framerate:   req.Framerate,
			Resolution:  req.Resolution,
			Pattern:     req.Pattern,
			MaxDuration: req.MaxDuration,
			MaxFilesize: req.MaxFilesize,
			Compress:    req.Compress,
			CreatedAt:   time.Now().Unix(),
		}, true); err != nil {
			lastErr = err
			fmt.Printf("[ERROR] create channel %s: %v\n", username, err)
		}
	}
	if lastErr != nil {
		c.String(http.StatusInternalServerError, "Failed to save channel config: %v", lastErr)
		return
	}
	c.Redirect(http.StatusFound, "/")
}

// StopChannel stops a channel.
func StopChannel(c *gin.Context) {
	if err := server.Manager.StopChannel(c.Param("username")); err != nil {
		fmt.Printf("[ERROR] stop channel %s: %v\n", c.Param("username"), err)
	}

	c.Redirect(http.StatusFound, "/")
}

// PauseChannel pauses a channel.
func PauseChannel(c *gin.Context) {
	if err := server.Manager.PauseChannel(c.Param("username")); err != nil {
		fmt.Printf("[ERROR] pause channel %s: %v\n", c.Param("username"), err)
	}

	c.Redirect(http.StatusFound, "/")
}

// ResumeChannel resumes a paused channel.
func ResumeChannel(c *gin.Context) {
	if err := server.Manager.ResumeChannel(c.Param("username")); err != nil {
		fmt.Printf("[ERROR] resume channel %s: %v\n", c.Param("username"), err)
	}

	c.Redirect(http.StatusFound, "/")
}

// Updates handles the SSE connection for updates.
func Updates(c *gin.Context) {
        server.Manager.Subscriber(c.Writer, c.Request)
}

// UpdateConfigRequest represents the request body for updating configuration.
type UpdateConfigRequest struct {
        Cookies   string `json:"cookies" form:"cookies"`
        UserAgent string `json:"user_agent" form:"user_agent"`
        ByparrURL string `json:"byparr_url" form:"byparr_url"`
}

// UpdateConfig updates the server configuration (form from Web UI or JSON from cookie-refresher).
func UpdateConfig(c *gin.Context) {
	var req UpdateConfigRequest
	if err := c.ShouldBind(&req); err != nil {
		c.AbortWithError(http.StatusBadRequest, fmt.Errorf("bind: %w", err))
		return
	}

	if req.Cookies != "" || req.UserAgent != "" {
		server.UpdateByparrCredentials(req.Cookies, req.UserAgent)
	}
	if req.ByparrURL != "" {
		server.Config.ByparrURL = req.ByparrURL
	}

	if err := server.SaveSettings(); err != nil {
		fmt.Printf("[WARN] could not save settings: %v\n", err)
	}

	if c.ContentType() == "application/json" {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
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

	// Fall back to preview_links table if sidecars don't exist
	if thumbURL == "" || spriteURL == "" {
		dbThumb, dbSprite := server.LoadPreviewLinks(filename)
		if thumbURL == "" && dbThumb != "" {
			thumbURL = dbThumb
		}
		if spriteURL == "" && dbSprite != "" {
			spriteURL = dbSprite
		}
	}

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
                                        if strings.Contains(strings.ToLower(embedURL), "byse.sx/e/") {
                                                embedURL = ""
                                        }
                                        dbThumbnailURL = rec.ThumbnailURL
                                        dbSpriteURL = rec.SpriteURL
                                        timestamp = rec.Timestamp
                                        resolution = rec.Resolution
                                        framerate = rec.Framerate
                                        break
                                }
                        }
                }
                // Get smart recommendations based on multiple factors
                allVideos := scanVideos()
                currentVideo := &VideoEntry{
                        Username:    username,
                        Filename:    filename,
                        Tags:        tags,
                        Gender:      gender,
                        Resolution:  resolution,
                        Framerate:   framerate,
                        ModTimeSort: timestamp,
                }
                recommendations := getRecommendations(currentVideo, allVideos, 8)
                // Convert VideoEntry to RecordingEntry for template
                for _, v := range recommendations {
                        related = append(related, RecordingEntry{
                                Filename:     v.Filename,
                                Timestamp:    v.ModTime,
                                RoomTitle:    v.RoomTitle,
                                Tags:         v.Tags,
                                Viewers:      v.Viewers,
                                Resolution:   v.Resolution,
                                Framerate:    v.Framerate,
                                ThumbnailURL: v.ThumbnailURL,
                                SpriteURL:    v.SpriteURL,
                        })
                }
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

        byseAPIKey := ""
        if server.Config != nil {
                byseAPIKey = server.Config.ByseAPIKey
        }
        hostPlayers := buildHostPlayers(links, byseAPIKey)

        // If embed URL is empty, try to generate one from upload links
        if embedURL == "" {
                for _, player := range hostPlayers {
                        if player.EmbedURL != "" {
                                embedURL = player.EmbedURL
                                break
                        }
                }
        }

        hostPlayersJSON, _ := json.Marshal(hostPlayers)

        // Find a direct video URL from upload links (for native player fallback).
        videoURL := ""
        if embedURL == "" {
                for _, player := range hostPlayers {
                        if player.VideoURL != "" {
                                videoURL = player.VideoURL
                                break
                        }
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
                "Config":          server.Config,
                "Filename":        filename,
                "FullPath":        fullPath,
                "VideoURL":        videoURL,
                "Size":            size,
                "ModTime":         modTime,
                "Username":        username,
                "ThumbnailURL":    thumbURL,
                "SpriteURL":       spriteURL,
                "MimeType":        mimeType,
                "Links":           links,
                "HostPlayers":     hostPlayers,
                "HostPlayersJSON": template.JS(hostPlayersJSON),
                "Tags":            tags,
                "RoomTitle":       roomTitle,
                "Viewers":         viewers,
                "Gender":          gender,
                "Resolution":      resolution,
                "Framerate":       framerate,
                "Related":         related,
                "EmbedURL":        embedURL,
        })
}

func buildHostPlayers(links map[string]string, byseAPIKey string) []hostPlayer {
        if len(links) == 0 {
                return nil
        }

        hosts := make([]string, 0, len(links))
        for host := range links {
                hosts = append(hosts, host)
        }
        sort.Strings(hosts)

        players := make([]hostPlayer, 0, len(hosts))
        for _, host := range hosts {
                link := links[host]
                players = append(players, hostPlayer{
                        Host:     host,
                        Link:     link,
                        EmbedURL: embedURLForHostLink(host, link, byseAPIKey),
                        VideoURL: videoURLForHostLink(host, link),
                })
        }
        return players
}

func embedURLForHostLink(host, link, byseAPIKey string) string {
        if link == "" {
                return ""
        }
        normalizedHost := strings.ToLower(host)
        normalizedLink := strings.ToLower(link)

        if strings.Contains(normalizedHost, "streamtape") || strings.Contains(normalizedLink, "streamtape.com/") {
                if code := extractStreamtapeCode(link); code != "" {
                        return "https://streamtape.com/e/" + code
                }
        }
        // Handle both Byse download URLs (/d/) and embed URLs (/e/)
        if strings.Contains(normalizedHost, "byse") || 
           strings.Contains(normalizedLink, "byse.sx/d/") || 
           strings.Contains(normalizedLink, "byse.sx/e/") ||
           strings.Contains(normalizedLink, "api.byse.sx/e/") {
                if code := extractFileCode(link); code != "" {
                        return byseEmbedURL(code, byseAPIKey)
                }
        }
        if strings.Contains(normalizedHost, "sendcm") || strings.Contains(normalizedLink, "send.now/") {
                return link
        }
        if strings.Contains(normalizedHost, "voe") || strings.Contains(normalizedLink, "voe.sx/") {
                if code := extractFileCode(link); code != "" {
                        return "https://voe.sx/e/" + code
                }
        }
        return ""
}

func byseEmbedURL(fileCode, apiKey string) string {
        if domain := byseEmbedDomainForKey(apiKey); domain != "" {
                return "https://" + strings.Trim(domain, "/") + "/e/" + fileCode
        }
        // Fallback to filemoon.sx (the old Byse domain) if API call fails
        // Note: api.byse.sx is for API calls, not for embedding videos
        return "https://filemoon.sx/e/" + fileCode
}

func byseEmbedDomainForKey(apiKey string) string {
        if apiKey == "" {
                return ""
        }

        byseEmbedDomainMu.RLock()
        if byseEmbedDomain != "" && time.Since(byseEmbedDomainFetchedAt) < time.Hour {
                domain := byseEmbedDomain
                byseEmbedDomainMu.RUnlock()
                return domain
        }
        byseEmbedDomainMu.RUnlock()

        byseEmbedDomainMu.Lock()
        defer byseEmbedDomainMu.Unlock()
        if byseEmbedDomain != "" && time.Since(byseEmbedDomainFetchedAt) < time.Hour {
                return byseEmbedDomain
        }

        reqURL := "https://api.byse.sx/get/domain?key=" + url.QueryEscape(apiKey)
        client := &http.Client{Timeout: 5 * time.Second}
        resp, err := client.Get(reqURL)
        if err != nil {
                fmt.Printf("Byse: failed to fetch embed domain: %v\n", err)
                return ""
        }
        defer resp.Body.Close()
        if resp.StatusCode != http.StatusOK {
                fmt.Printf("Byse: embed domain API returned status %d\n", resp.StatusCode)
                return ""
        }

        var data struct {
                NewDomain string `json:"new_domain"`
                OldDomain string `json:"old_domain"`
                Status    int    `json:"status"`
        }
        if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
                fmt.Printf("Byse: failed to decode embed domain response: %v\n", err)
                return ""
        }
        if data.Status != http.StatusOK {
                fmt.Printf("Byse: embed domain API returned status code %d in response\n", data.Status)
                return ""
        }
        if data.NewDomain == "" {
                fmt.Printf("Byse: embed domain API returned empty new_domain (old_domain: %s)\n", data.OldDomain)
                return ""
        }

        byseEmbedDomain = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(data.NewDomain), "https://"), "http://")
        byseEmbedDomainFetchedAt = time.Now()
        fmt.Printf("Byse: using embed domain: %s\n", byseEmbedDomain)
        return byseEmbedDomain
}

func videoURLForHostLink(host, link string) string {
        if link == "" {
                return ""
        }

        normalizedHost := strings.ToLower(host)
        normalizedLink := strings.ToLower(link)

        switch {
        case strings.Contains(normalizedHost, "streamtape") || strings.Contains(normalizedLink, "streamtape.com/"):
                if code := extractStreamtapeCode(link); code != "" {
                        return "https://streamtape.com/e/" + code
                }
                return link
        case strings.Contains(normalizedHost, "byse") || strings.Contains(normalizedLink, "byse.sx/") || strings.Contains(normalizedLink, "filemoon.sx/"):
                if code := extractFileCode(link); code != "" {
                        return "https://filemoon.sx/e/" + code
                }
                return link
        case strings.Contains(normalizedHost, "voe") || strings.Contains(normalizedLink, "voe.sx/"):
                if code := extractFileCode(link); code != "" {
                        return "https://voe.sx/e/" + code
                }
                return link
        case strings.Contains(normalizedHost, "sendcm") || strings.Contains(normalizedLink, "send.now/"):
                return link
		case strings.Contains(normalizedHost, "turboviplay") || strings.Contains(normalizedLink, "emturbovid.com/") || strings.Contains(normalizedLink, "turboviplay.com/"):
                return link
        default:
                return ""
        }
}

func readSidecar(path string) string {
        if d, e := os.ReadFile(path); e == nil {
                return strings.TrimSpace(string(d))
        }
        return ""
}

// ServeThumb serves the locally-stored .thumb.jpg file for a recording.
// Usage: GET /thumb?path=videos/foo.mkv
func ServeThumb(c *gin.Context) {
        serveLocalImage(c, ".thumb.jpg")
}

// ServeSprite serves the locally-stored .sprite.jpg file for a recording.
// Usage: GET /sprite?path=videos/foo.mkv
func ServeSprite(c *gin.Context) {
        serveLocalImage(c, ".sprite.jpg")
}

func serveLocalImage(c *gin.Context, suffix string) {
        path := c.Query("path")
        if path == "" {
                c.Status(http.StatusBadRequest)
                return
        }
        abs, err := filepath.Abs(path + suffix)
        if err != nil {
                c.Status(http.StatusNotFound)
                return
        }
        wd, _ := os.Getwd()
        if !strings.HasPrefix(abs, wd) {
                c.Status(http.StatusForbidden)
                return
        }
        if _, err := os.Stat(abs); err != nil {
                c.Status(http.StatusNotFound)
                return
        }
        c.Header("Cache-Control", "public, max-age=604800")
        c.File(abs)
}

// ─── Tunnel API ──────────────────────────────────────────────────────────────

type tunnelRequest struct {
	URL   string `json:"url" form:"url"`
	RunID int    `json:"run_id" form:"run_id"`
}

// UpdateTunnel saves a tunnel URL to Supabase.
func UpdateTunnel(c *gin.Context) {
	var req tunnelRequest
	if err := c.ShouldBind(&req); err != nil {
		c.AbortWithError(http.StatusBadRequest, fmt.Errorf("bind: %w", err))
		return
	}
	if req.URL == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if err := server.SaveTunnelToDB(req.URL, req.RunID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// GetTunnel returns the current active tunnel URL.
func GetTunnel(c *gin.Context) {
	url, err := server.LoadCurrentTunnel()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
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
