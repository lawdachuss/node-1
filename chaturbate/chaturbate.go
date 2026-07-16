package chaturbate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/grafov/m3u8"
	"github.com/samber/lo"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

// Room status constants from the Chaturbate API.
const (
	StatusPublic  = "public"
	StatusPrivate = "private"
	StatusAway    = "away"
	StatusOffline = "offline"
)

// edgeRegionRegexp extracts edge region from URL like "edge14-sin.live.mmcdn.com"
var edgeRegionRegexp = regexp.MustCompile(`edge\d+-([a-z]+)`)

// edgeRegions is the list of CDN edge regions to try when geo-blocked
var edgeRegions = []string{"lax", "fra", "ams", "sin", "hnd"}

// APIResponse represents the response from /api/chatvideocontext/ and get_edge_hls_url_ajax/ endpoints.
// The POST endpoint returns the stream URL in the "url" field; the GET endpoint uses "hls_source".
type APIResponse struct {
	HLSSource         string   `json:"hls_source"`
	URL               string   `json:"url"`
	RoomStatus        string   `json:"room_status"`
	RoomTitle         string   `json:"room_title"`
	Tags              []string `json:"tags"`
	NumUsers          int      `json:"num_users"`
	BroadcasterGender string   `json:"broadcaster_gender"`
}

// StreamURL returns the HLS source URL, preferring hls_source and falling back to url.
func (r *APIResponse) StreamURL() string {
	if r.HLSSource != "" {
		return r.HLSSource
	}
	return r.URL
}

// Client represents an API client for interacting with Chaturbate.
type Client struct {
	Req            *internal.Req
	LastRoomStatus string   // cached from the most recent API call
	LastRoomTitle  string   // cached room metadata for recording entry
	LastTags       []string // cached room metadata for recording entry
	LastViewers    int      // cached room metadata for recording entry
	LastGender     string   // cached broadcaster_gender ("m", "f", "c", "t", …)
	SkipEdgeCheck  bool     // if true, skip HEAD validation in FetchStream (used after stream stall for fast reconnect)
}

// NewClient initializes and returns a new Client instance.
func NewClient() *Client {
	return &Client{
		Req: internal.NewReq(),
	}
}

// GetStream fetches the stream information for a given username.
// Room metadata (title, tags, viewers, gender) is cached on the Client for use
// when building the recording entry.
func (c *Client) GetStream(ctx context.Context, username string) (*Stream, error) {
	var roomInfo APIResponse
	// Use the internal helper so SkipEdgeCheck is passed through.
	stream, roomStatus, err := fetchStream(ctx, c.Req, username, &roomInfo, c.SkipEdgeCheck)
	c.SkipEdgeCheck = false // one-shot; reset after use
	c.LastRoomStatus = roomStatus
	c.LastRoomTitle = roomInfo.RoomTitle
	c.LastTags = roomInfo.Tags
	c.LastViewers = roomInfo.NumUsers
	c.LastGender = roomInfo.BroadcasterGender
	return stream, err
}

// GetRoomStatus returns the room status string (public, private, away, offline, etc.)
func (c *Client) GetRoomStatus(ctx context.Context, username string) (string, error) {
	resp, err := fetchAPIResponse(ctx, c.Req, username)
	if err != nil {
		return "", err
	}
	return resp.RoomStatus, nil
}

func fetchAPIResponse(ctx context.Context, client *internal.Req, username string) (*APIResponse, error) {
	apiURL := fmt.Sprintf("%sapi/chatvideocontext/%s/", server.Config.Domain, username)

	// NOTE: No circuit breaker check here on purpose. The GET (chatvideocontext)
	// endpoint only needs cookies (no CSRF) and is the critical fallback when
	// the POST API fails. The circuit breaker only gates the POST API — it
	// must NOT block the fallback, or the node can never recover and record
	// live channels during transient auth issues.

	var body string
	err := retry.Do(func() error {
		if err := internal.WaitForChaturbateRateLimit(ctx); err != nil {
			return err
		}

		var e error
		body, e = client.Get(ctx, apiURL)
		if e != nil {
			internal.ReportChaturbateFailure()
			if errors.Is(e, internal.ErrPasswordRequired) {
				return retry.Unrecoverable(e)
			}
			if errors.Is(e, internal.ErrPrivateStream) {
				return retry.Unrecoverable(e)
			}
			return e
		}
		if body == "" {
			internal.ReportChaturbateFailure()
			return fmt.Errorf("empty response body")
		}
		// HTML 404 (offline/nonexistent channel) — stop retrying
		if strings.Contains(body, "<!DOCTYPE html") && strings.Contains(body, "404") {
			internal.ReportChaturbateFailure()
			return retry.Unrecoverable(fmt.Errorf("channel not found (HTTP 404)"))
		}
		internal.ReportChaturbateSuccess()
		return nil
	},
		retry.Context(ctx),
		retry.Attempts(10),
		retry.Delay(1*time.Second),
		retry.MaxDelay(30*time.Second),
		retry.DelayType(retry.BackOffDelay),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get API response: %w", err)
	}

	var resp APIResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse API response: %w", err)
	}

	return &resp, nil
}

// FetchStream retrieves the streaming data using the Chaturbate API.
// Returns the stream, the room status string, and any error.
// If roomInfo is non-nil, it is populated with room metadata (title, tags,
// viewers) from whichever API call succeeded.
func FetchStream(ctx context.Context, client *internal.Req, username string, roomInfo *APIResponse) (*Stream, string, error) {
	return fetchStream(ctx, client, username, roomInfo, false)
}

// fetchStream is the internal implementation of FetchStream.  When skipEdgeCheck
// is true, HEAD validation of the HLS edge URL is skipped — used after a stream
// stall to reconnect as fast as possible.
func fetchStream(ctx context.Context, client *internal.Req, username string, roomInfo *APIResponse, skipEdgeCheck bool) (*Stream, string, error) {
	// Try POST API first
	body, err := internal.PostChaturbateAPI(ctx, username)
	if err != nil {
		if errors.Is(err, internal.ErrPasswordRequired) {
			return nil, StatusPrivate, internal.ErrPasswordRequired
		}
		// POST failed — retry with backoff before falling back to GET.
		// The GET endpoint (chatvideocontext) often returns 403 when cookies
		// or proxy don't match, wasting ~30s on 10 retries. Retrying POST
		// is faster since transient issues (rate limit, CSRF) may clear.
		for retry := 0; retry < 3; retry++ {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(time.Duration(retry+2) * time.Second):
			}
			body, err = internal.PostChaturbateAPI(ctx, username)
			if err == nil {
				goto postSucceeded
			}
			if errors.Is(err, internal.ErrPasswordRequired) {
				return nil, StatusPrivate, internal.ErrPasswordRequired
			}
		}
		// Try the GET API as fallback
		resp, apiErr := fetchAPIResponse(ctx, client, username)
		if apiErr != nil {
			if errors.Is(apiErr, internal.ErrPasswordRequired) {
				return nil, StatusPrivate, internal.ErrPasswordRequired
			}
			return nil, "", apiErr
		}

		if roomInfo != nil {
			roomInfo.RoomTitle = resp.RoomTitle
			roomInfo.Tags = resp.Tags
			roomInfo.NumUsers = resp.NumUsers
			if resp.BroadcasterGender != "" {
			roomInfo.BroadcasterGender = resp.BroadcasterGender
			}
		}

	switch resp.RoomStatus {
		case StatusPrivate:
			return nil, resp.RoomStatus, internal.ErrPrivateStream
		case StatusAway, StatusOffline:
			return nil, resp.RoomStatus, internal.ErrChannelOffline
		}

		if resp.StreamURL() == "" {
			return nil, resp.RoomStatus, internal.ErrChannelOffline
		}

		workingURL := resp.StreamURL()
		if !skipEdgeCheck {
			url, edgeErr := findWorkingEdgeURL(ctx, client, workingURL)
			if edgeErr != nil {
				return nil, resp.RoomStatus, edgeErr
			}
			workingURL = url
		}

		return &Stream{HLSSource: workingURL}, resp.RoomStatus, nil
	}

postSucceeded:
	var getResp *APIResponse
	var getErr error

	// Parse POST API response
	var resp APIResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, "", fmt.Errorf("failed to parse POST API response: %w", err)
	}

	if roomInfo != nil {
		roomInfo.RoomTitle = resp.RoomTitle
		roomInfo.Tags = resp.Tags
		roomInfo.NumUsers = resp.NumUsers
		if resp.BroadcasterGender != "" {
			roomInfo.BroadcasterGender = resp.BroadcasterGender
		}
	}

	// Debug logging for troubleshooting
	fmt.Printf("[DEBUG] %s POST API response: status=%s url=%s\n", username, resp.RoomStatus, resp.StreamURL())

	// Enrich metadata from the GET API (chatvideocontext) which reliably
	// returns tags, room_title, num_users, and broadcaster_gender even when
	// the POST endpoint only returns the HLS URL.
	if getResp, getErr = fetchAPIResponse(ctx, client, username); getErr == nil {
		if roomInfo != nil {
			if getResp.RoomTitle != "" {
				roomInfo.RoomTitle = getResp.RoomTitle
			}
			if len(getResp.Tags) > 0 {
				roomInfo.Tags = getResp.Tags
			}
			if getResp.NumUsers > 0 {
				roomInfo.NumUsers = getResp.NumUsers
			}
			if getResp.BroadcasterGender != "" {
				roomInfo.BroadcasterGender = getResp.BroadcasterGender
			}
		}
		if resp.RoomStatus == "" && getResp.RoomStatus != "" {
			resp.RoomStatus = getResp.RoomStatus
		}
	}

	// The POST endpoint (get_edge_hls_url_ajax) sometimes returns a false
	// "offline"/"away" when the proxy serves a stale response or the CSRF
	// token/cookies are momentarily flagged. The GET chatvideocontext endpoint
	// is more lenient (cookies only, no CSRF) and reports the true room status.
	// When POST says offline/away but GET says the channel is actually
	// broadcasting, trust GET so we don't skip a live channel.
	if (resp.RoomStatus == StatusAway || resp.RoomStatus == StatusOffline) && getErr == nil &&
		getResp.RoomStatus != "" && getResp.RoomStatus != StatusAway && getResp.RoomStatus != StatusOffline {
		resp.RoomStatus = getResp.RoomStatus
		if resp.StreamURL() == "" && getResp.StreamURL() != "" {
			resp.HLSSource = getResp.HLSSource
			resp.URL = getResp.URL
		}
	}

	switch resp.RoomStatus {
	case StatusPrivate:
		return nil, resp.RoomStatus, internal.ErrPrivateStream
	case StatusAway, StatusOffline:
		return nil, resp.RoomStatus, internal.ErrChannelOffline
	}

	// If POST API returned a public room but no HLS source, fall back to GET API.
	if resp.StreamURL() == "" {
		fmt.Printf("[WARN] %s: POST API returned empty URL, trying GET API fallback (check cookies if this persists)\n", username)
		getResp, apiErr := fetchAPIResponse(ctx, client, username)
		if apiErr == nil && getResp.StreamURL() != "" {
			resp = *getResp
			if roomInfo != nil {
				roomInfo.RoomTitle = getResp.RoomTitle
				roomInfo.Tags = getResp.Tags
				roomInfo.NumUsers = getResp.NumUsers
				if getResp.BroadcasterGender != "" {
					roomInfo.BroadcasterGender = getResp.BroadcasterGender
				}
			}
		} else {
			if apiErr == nil {
				switch getResp.RoomStatus {
				case StatusPrivate:
					return nil, getResp.RoomStatus, internal.ErrPrivateStream
				default:
					return nil, getResp.RoomStatus, internal.ErrChannelOffline
				}
			}
			if errors.Is(apiErr, internal.ErrPasswordRequired) {
				return nil, StatusPrivate, internal.ErrPasswordRequired
			}
			return nil, resp.RoomStatus, internal.ErrChannelOffline
		}
	}

	workingURL := resp.StreamURL()
	if !skipEdgeCheck {
		url, edgeErr := findWorkingEdgeURL(ctx, client, workingURL)
		if edgeErr != nil {
			return nil, resp.RoomStatus, edgeErr
		}
		workingURL = url
	}

	return &Stream{HLSSource: workingURL}, resp.RoomStatus, nil
}

// findWorkingEdgeURL validates the HLS URL and tries alternative edge regions if geo-blocked.
func findWorkingEdgeURL(ctx context.Context, client *internal.Req, hlsSource string) (string, error) {
	// LL-HLS URLs use token-based sessions; HEAD requests consume the token
	// and cause subsequent GET requests to fail with "session_duplicated".
	// Skip HEAD validation for these URLs.
	if strings.Contains(hlsSource, "llhls.m3u8") {
		return hlsSource, nil
	}

	// 1. Validate original URL
	statusCode, err := client.Head(ctx, hlsSource)
	if err == nil && statusCode == 200 {
		return hlsSource, nil
	}
	fmt.Printf("[DEBUG] findWorkingEdgeURL: original HEAD -> status=%d err=%v\n", statusCode, err)

	// 2. Extract current region from URL
	matches := edgeRegionRegexp.FindStringSubmatch(hlsSource)
	if len(matches) < 2 {
		fmt.Printf("[DEBUG] findWorkingEdgeURL: no edge region in URL, returning original\n")
		return hlsSource, nil
	}
	currentRegion := matches[1]

	// 3. Try alternative edge regions: lax, fra, ams, sin, hnd
	for _, region := range edgeRegions {
		if region == currentRegion {
			continue
		}
		altURL := strings.Replace(hlsSource, "-"+currentRegion+".", "-"+region+".", 1)

		statusCode, err := client.Head(ctx, altURL)
		if err == nil && statusCode == 200 {
			return altURL, nil
		}
		fmt.Printf("[DEBUG] findWorkingEdgeURL: alt region %s -> status=%d err=%v\n", region, statusCode, err)
	}

	// 4. If we couldn't validate any edge, return the original URL anyway
	//    so the recorder can try GETing it directly (HEAD may be blocked by CDN).
	fmt.Printf("[DEBUG] findWorkingEdgeURL: all regions failed, returning original URL\n")
	return hlsSource, nil
}

// Stream represents an HLS stream source.
type Stream struct {
	HLSSource string
}

// GetPlaylist retrieves the playlist corresponding to the given resolution and framerate.
func (s *Stream) GetPlaylist(ctx context.Context, resolution, framerate int) (*Playlist, error) {
	return FetchPlaylist(ctx, s.HLSSource, resolution, framerate)
}

// FetchPlaylist fetches and decodes the HLS playlist file.
func FetchPlaylist(ctx context.Context, hlsSource string, resolution, framerate int) (*Playlist, error) {
	if hlsSource == "" {
		return nil, errors.New("HLS source is empty")
	}

	resp, playlistSource, err := fetchPlaylistSource(ctx, hlsSource)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch HLS source: %w", err)
	}

	return ParsePlaylist(resp, playlistSource, resolution, framerate)
}

func fetchPlaylistSource(ctx context.Context, hlsSource string) (string, string, error) {
	client := internal.NewReq()
	resp, err := retry.DoWithData(
		func() (string, error) {
			return client.Get(ctx, hlsSource)
		},
		retry.Context(ctx),
		retry.Attempts(3),
		retry.Delay(500*time.Millisecond),
		retry.MaxDelay(3*time.Second),
		retry.DelayType(retry.BackOffDelay),
	)
	if err == nil {
		return resp, hlsSource, nil
	}

	// LL-HLS session URLs should not be probed with HEAD because that can
	// consume the token. If the first GET cannot connect to the assigned
	// CDN edge, try the same URL on alternate regions using GET only.
	if !strings.Contains(hlsSource, "llhls.m3u8") {
		return "", hlsSource, err
	}
	for _, altURL := range alternateEdgeURLs(hlsSource) {
		altResp, altErr := client.Get(ctx, altURL)
		if altErr == nil {
			fmt.Printf("[DEBUG] FetchPlaylist: recovered via alternate edge %s\n", edgeHostForLog(altURL))
			return altResp, altURL, nil
		}
		fmt.Printf("[DEBUG] FetchPlaylist: alternate edge %s failed: %v\n", edgeHostForLog(altURL), altErr)
	}

	return "", hlsSource, err
}

func alternateEdgeURLs(hlsSource string) []string {
	matches := edgeRegionRegexp.FindStringSubmatch(hlsSource)
	if len(matches) < 2 {
		return nil
	}
	currentRegion := matches[1]
	urls := make([]string, 0, len(edgeRegions)-1)
	for _, region := range edgeRegions {
		if region == currentRegion {
			continue
		}
		urls = append(urls, strings.Replace(hlsSource, "-"+currentRegion+".", "-"+region+".", 1))
	}
	return urls
}

func edgeHostForLog(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Host
}

// ParsePlaylist decodes the M3U8 playlist and extracts the variant streams.
func ParsePlaylist(resp, hlsSource string, resolution, framerate int) (*Playlist, error) {
	p, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
	if err != nil {
		return nil, fmt.Errorf("failed to decode m3u8 playlist: %w", err)
	}

	masterPlaylist, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		return nil, errors.New("invalid master playlist format")
	}

	return PickPlaylist(masterPlaylist, hlsSource, resolution, framerate)
}

// Playlist represents an HLS playlist containing variant streams.
type Playlist struct {
	PlaylistURL      string
	AudioPlaylistURL string
	RootURL          string
	Resolution       int
	Framerate        int
}

// Resolution represents a video resolution and its corresponding framerate.
type Resolution struct {
	Framerate    map[int]string // [framerate]url
	Width        int
	Alternatives []*m3u8.Alternative
}

// PickPlaylist selects the best matching variant stream based on resolution and framerate.
func PickPlaylist(masterPlaylist *m3u8.MasterPlaylist, baseURL string, resolution, framerate int) (*Playlist, error) {
	resolutions := map[int]*Resolution{}

	// Extract available resolutions and framerates from the master playlist
	for _, v := range masterPlaylist.Variants {
		parts := strings.Split(v.Resolution, "x")
		if len(parts) != 2 {
			continue
		}
		width, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("parse resolution: %w", err)
		}
		framerateVal := 30
		if v.FrameRate >= 59.0 || strings.Contains(v.Name, "FPS:60.0") {
			framerateVal = 60
		}
		if _, exists := resolutions[width]; !exists {
			resolutions[width] = &Resolution{Framerate: map[int]string{}, Width: width, Alternatives: v.Alternatives}
		}
		resolutions[width].Framerate[framerateVal] = v.URI
	}

	// Find exact match for requested resolution
	variant, exists := resolutions[resolution]
	if !exists {
		// Filter resolutions below the requested resolution
		candidates := lo.Filter(lo.Values(resolutions), func(r *Resolution, _ int) bool {
			return r.Width < resolution
		})
		// Pick the highest resolution among the candidates
		variant = lo.MaxBy(candidates, func(a, b *Resolution) bool {
			return a.Width > b.Width
		})
	}
	if variant == nil {
		return nil, fmt.Errorf("resolution not found")
	}

	var (
		finalResolution = variant.Width
		finalFramerate  = framerate
		audioPlaylist   string
	)
	// Select the desired framerate, or fallback to the first available framerate
	playlistURL, exists := variant.Framerate[framerate]
	if !exists {
		for fr, url := range variant.Framerate {
			playlistURL = url
			finalFramerate = fr
			break
		}
	}

	for _, alt := range variant.Alternatives {
		if alt == nil || alt.Type != "AUDIO" || alt.URI == "" {
			continue
		}
		audioPlaylist = resolveURL(baseURL, alt.URI)
		if alt.Default {
			break
		}
	}

	return &Playlist{
		PlaylistURL:      resolveURL(baseURL, playlistURL),
		AudioPlaylistURL: audioPlaylist,
		RootURL:          baseURL,
		Resolution:       finalResolution,
		Framerate:        finalFramerate,
	}, nil
}

// resolveURL resolves a potentially relative or absolute URI against a base URL.
func resolveURL(baseURL, ref string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(refURL).String()
}

// WatchHandler is a function type that processes video segments.
type WatchHandler func(b []byte, duration float64) error

// InitHandler is called once when an init segment (fMP4 moov atom) is detected.
type InitHandler func(initData []byte) error

// PollCompleteHandler is called once per poll cycle after both video and
// audio playlists have been processed. Used to coordinate side effects that
// must not interleave with segment processing (e.g. file rotation).
type PollCompleteHandler func() error

// WatchAVSegments continuously fetches and processes video segments, and optional separate audio segments.
func (p *Playlist) WatchAVSegments(ctx context.Context, handler WatchHandler, initHandler InitHandler, audioHandler WatchHandler, audioInitHandler InitHandler, pollComplete PollCompleteHandler) error {
	var (
		client           = internal.NewReq()
		lastSeq          = -1
		initWritten      = false
		audioLastSeq     = -1
		audioInitWritten = false

		// Stall detection: count consecutive poll cycles where lastSeq does not
		// advance.  After maxStalledPolls cycles we return ErrStreamStalled so the
		// caller (Monitor) can finalise the current file and re-fetch a fresh HLS
		// URL with a new CDN session token.  Only start counting once we have
		// recorded at least one segment (lastSeq >= 0) so we don't trigger on the
		// very first poll before any segments are available on the live edge.
		// Reduced from 5 to 2 for faster CDN token expiry detection.
		stalledPolls    = 0
		maxStalledPolls = 2
	)

	for {
		prevLastSeq := lastSeq

		pollInterval, err := p.processMediaPlaylist(ctx, client, p.PlaylistURL, handler, initHandler, &lastSeq, &initWritten)
		if err != nil {
			return fmt.Errorf("video: %w", err)
		}
		if p.AudioPlaylistURL != "" {
			audioInterval, err := p.processMediaPlaylist(ctx, client, p.AudioPlaylistURL, audioHandler, audioInitHandler, &audioLastSeq, &audioInitWritten)
			if err != nil {
				return fmt.Errorf("audio: %w", err)
			}
			pollInterval = pickPollInterval(pollInterval, audioInterval)
		}

		if pollComplete != nil {
			if err := pollComplete(); err != nil {
				return fmt.Errorf("poll complete: %w", err)
			}
		}

		// Stall detection: if we have started recording (lastSeq >= 0) but
		// made no progress this cycle, increment the stall counter.
		if lastSeq >= 0 && lastSeq == prevLastSeq {
			stalledPolls++
			if stalledPolls >= maxStalledPolls {
				return internal.ErrStreamStalled
			}
		} else {
			stalledPolls = 0
		}

		// Use the playlist's target duration as the polling interval (minimum 2s)
		// with random jitter to avoid synchronized requests across channels.
		if pollInterval < 2*time.Second {
			pollInterval = 2 * time.Second
		}
		jitter := time.Duration(rand.Intn(500)) * time.Millisecond
		timer := time.NewTimer(pollInterval + jitter)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func pickPollInterval(current, candidate time.Duration) time.Duration {
	if current <= 0 {
		return candidate
	}
	if candidate <= 0 {
		return current
	}
	if candidate < current {
		return candidate
	}
	return current
}

func (p *Playlist) processMediaPlaylist(ctx context.Context, client *internal.Req, playlistURL string, handler WatchHandler, initHandler InitHandler, lastSeq *int, initWritten *bool) (time.Duration, error) {
	originalURL := playlistURL
	resp, err := retry.DoWithData(
		func() (string, error) {
			return client.Get(ctx, playlistURL)
		},
		retry.Context(ctx),
		retry.Attempts(3),
		retry.Delay(500*time.Millisecond),
		retry.MaxDelay(3*time.Second),
		retry.DelayType(retry.BackOffDelay),
	)
	if err != nil {
		// All retries on the original edge failed (e.g. SOCKS5 proxy
		// cannot route to that specific edge).  Try alternate CDN edge
		// regions before giving up, since a different datacentre may
		// be reachable through the same proxy.
		for _, altURL := range alternateEdgeURLs(playlistURL) {
			altResp, altErr := client.Get(ctx, altURL)
			if altErr == nil {
				playlistURL = altURL
				resp = altResp
				err = nil
				break
			}
		}
		if err != nil {
			return 0, fmt.Errorf("get playlist after 3 retries: %w", err)
		}
		// Persist the working alternate edge for subsequent poll cycles.
		if p.PlaylistURL == originalURL {
			p.PlaylistURL = playlistURL
		} else if p.AudioPlaylistURL == originalURL {
			p.AudioPlaylistURL = playlistURL
		}
	}
	pl, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
	if err != nil {
		return 0, fmt.Errorf("decode from: %w", err)
	}
	playlist, ok := pl.(*m3u8.MediaPlaylist)
	if !ok {
		return 0, fmt.Errorf("cast to media playlist")
	}

	if !*initWritten && playlist.Map != nil && playlist.Map.URI != "" {
		initURL := resolveURL(playlistURL, playlist.Map.URI)
		initData, initErr := retry.DoWithData(
			func() ([]byte, error) {
				data, err := client.GetBytesWithTimeout(ctx, initURL, 30*time.Second)
				if err != nil {
					if strings.Contains(err.Error(), "read body: unexpected EOF") {
						data, err = client.GetBytesWithTimeout(ctx, initURL, 30*time.Second)
					}
					if err != nil {
						if strings.Contains(err.Error(), "unexpected HTTP 404") ||
							strings.Contains(err.Error(), "unexpected HTTP 403") {
							return nil, retry.Unrecoverable(err)
						}
					}
				}
				return data, err
			},
			retry.Context(ctx),
			retry.Attempts(3),
			retry.Delay(500*time.Millisecond),
			retry.MaxDelay(5*time.Second),
			retry.DelayType(retry.BackOffDelay),
		)
		if initErr != nil {
			return 0, fmt.Errorf("fetch init segment: %w", initErr)
		}
		if initHandler != nil {
			if err := initHandler(initData); err != nil {
				return 0, fmt.Errorf("handler init: %w", err)
			}
		}
		*initWritten = true
	}

	for _, v := range playlist.Segments {
		if v == nil {
			continue
		}
		seq := internal.SegmentSeq(v.URI)
		if seq == -1 || seq <= *lastSeq {
			continue
		}

		segmentURL := resolveURL(playlistURL, v.URI)
		resp, err := retry.DoWithData(
			func() ([]byte, error) {
				data, err := client.GetBytesWithTimeout(ctx, segmentURL, 30*time.Second)
				if err != nil {
					if strings.Contains(err.Error(), "read body: unexpected EOF") {
						data, err = client.GetBytesWithTimeout(ctx, segmentURL, 30*time.Second)
					}
					if err != nil {
						if strings.Contains(err.Error(), "unexpected HTTP 404") ||
							strings.Contains(err.Error(), "unexpected HTTP 403") {
							return nil, retry.Unrecoverable(err)
						}
					}
				}
				return data, err
			},
			retry.Context(ctx),
			retry.Attempts(3),
			retry.Delay(500*time.Millisecond),
			retry.MaxDelay(5*time.Second),
			retry.DelayType(retry.BackOffDelay),
		)
		if err != nil {
			// Return the error instead of silently breaking.
			//
			// The old `break` caused recordings to freeze permanently:
			// when CDN session tokens expire (typically every few minutes
			// on LL-HLS streams) every segment download fails, the break
			// exited the inner loop, processMediaPlaylist returned nil,
			// and WatchAVSegments looped forever writing nothing — producing
			// only small 10-200 MB recordings for long-running streams.
			//
			// Returning the error propagates it through WatchAVSegments →
			// RecordStream → Monitor.onRetry, which finalises the current
			// file and re-fetches a fresh HLS URL (new session token) so
			// recording resumes.
			return 0, fmt.Errorf("segment seq=%d: %w", seq, err)
		}
		if handler != nil {
			if err := handler(resp, v.Duration); err != nil {
				return 0, fmt.Errorf("handler: %w", err)
			}
		}
		*lastSeq = seq
	}

	return time.Duration(playlist.TargetDuration) * time.Second, nil
}
