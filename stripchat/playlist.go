package stripchat

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/grafov/m3u8"
	"github.com/samber/lo"
	"github.com/teacat/chaturbate-dvr/internal"
)

// Playlist represents a Stripchat HLS playlist, similar to chaturbate.Playlist
// but with MOUFLON v2 support.
type Playlist struct {
	PlaylistURL      string
	AudioPlaylistURL string
	RootURL          string
	MasterURL        string // original master HLS source URL (for re-fetching)
	Resolution       int
	Framerate        int
	PKey             string // MOUFLON pkey from master playlist
	PDKey            string // MOUFLON v2 decryption key
	MasterBody       string // raw master playlist body (for pkey extraction)
}

type WatchHandler func(b []byte, duration float64) error
type InitHandler func(initData []byte) error
type PollCompleteHandler func() error

// FetchPlaylist fetches a Stripchat HLS master playlist, extracts the MOUFLON
// pkey, and picks the best variant.
func FetchPlaylist(ctx context.Context, client *internal.Req, hlsSource string, pdkey string, resolution, framerate int) (*Playlist, error) {
	if hlsSource == "" {
		return nil, errors.New("HLS source is empty")
	}

	masterBody, err := retry.DoWithData(
		func() (string, error) {
			return client.Get(ctx, hlsSource)
		},
		retry.Context(ctx),
		retry.Attempts(3),
		retry.Delay(500*time.Millisecond),
		retry.MaxDelay(3*time.Second),
		retry.DelayType(retry.BackOffDelay),
	)
	if err != nil {
		return nil, fmt.Errorf("stripchat: fetch master playlist: %w", err)
	}

	// If no explicit pdkey, try to extract pkey from master playlist.
	var pkey string
	if pdkey == "" {
		pkey = ParsePKeyFromMaster(masterBody)
		if pkey != "" {
			pdkey = resolvePDKey(pkey)
		}
	}

	decryptedMaster := masterBody
	if pdkey != "" {
		decryptedMaster = decryptMouflonPlaylist(masterBody, pdkey)
	}

	baseURL := resolveBaseURL(hlsSource)
	masterURL := hlsSource
	pl, err := pickPlaylist(decryptedMaster, baseURL, masterURL, pkey, resolution, framerate)
	if err != nil {
		return nil, err
	}
	pl.PDKey = pdkey
	pl.PKey = pkey
	pl.MasterBody = masterBody
	return pl, nil
}

// WatchAVSegments continuously fetches and processes video segments with MOUFLON support.
func (p *Playlist) WatchAVSegments(ctx context.Context, handler WatchHandler, initHandler InitHandler, audioHandler WatchHandler, audioInitHandler InitHandler, pollComplete PollCompleteHandler) error {
	var (
		client           = internal.NewReq()
		lastSeq          = -1
		initWritten      = false
		audioLastSeq     = -1
		audioInitWritten = false
		stalledPolls     = 0
		maxStalledPolls  = 2
	)

	for {
		prevLastSeq := lastSeq

		isVOD, pollInterval, err := p.processMediaPlaylist(ctx, client, p.PlaylistURL, handler, initHandler, &lastSeq, &initWritten)
		if err != nil {
			return fmt.Errorf("video: %w", err)
		}
		if p.AudioPlaylistURL != "" {
			audioVOD, audioInterval, err := p.processMediaPlaylist(ctx, client, p.AudioPlaylistURL, audioHandler, audioInitHandler, &audioLastSeq, &audioInitWritten)
			if err != nil {
				return fmt.Errorf("audio: %w", err)
			}
			isVOD = isVOD || audioVOD
			pollInterval = pickPollInterval(pollInterval, audioInterval)
		}

		if pollComplete != nil {
			if err := pollComplete(); err != nil {
				return fmt.Errorf("poll complete: %w", err)
			}
		}

		// Stripchat CDN serves VOD-style playlists (ENDLIST) that need to be
		// re-fetched immediately — treat a VOD playlist that's been fully
		// consumed as a "stall" so watchLoopSC fetches a fresh playlist.
		if isVOD && lastSeq == prevLastSeq {
			return internal.ErrStreamStalled
		}

		if lastSeq >= 0 && lastSeq == prevLastSeq {
			stalledPolls++
			if stalledPolls >= maxStalledPolls {
				return internal.ErrStreamStalled
			}
		} else {
			stalledPolls = 0
		}

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

func (p *Playlist) processMediaPlaylist(ctx context.Context, client *internal.Req, playlistURL string, handler WatchHandler, initHandler InitHandler, lastSeq *int, initWritten *bool) (isVOD bool, pollInterval time.Duration, err error) {
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
		return false, 0, fmt.Errorf("get playlist after 3 retries: %w", err)
	}

	// Decrypt MOUFLON URIs in the playlist body.
	if p.PDKey != "" {
		resp = decryptMouflonPlaylist(resp, p.PDKey)
	} else if strings.Contains(resp, "#EXT-X-MOUFLON:") {
		return false, 0, fmt.Errorf("media playlist contains MOUFLON-encrypted segments but pdkey is empty (pkey=%q)", p.PKey)
	}

	// Use non-strict parsing: Stripchat's LL-HLS playlists omit the trailing
	// comma on #EXTINF lines (e.g. #EXTINF:1.985 vs #EXTINF:1.985,), which
	// causes grafov/m3u8's strict mode to reject them.
	pl, _, err := m3u8.DecodeFrom(strings.NewReader(resp), false)
	if err != nil {
		return false, 0, fmt.Errorf("decode from: %w", err)
	}
	playlist, ok := pl.(*m3u8.MediaPlaylist)
	if !ok {
		return false, 0, fmt.Errorf("cast to media playlist")
	}

	if !*initWritten && playlist.Map != nil && playlist.Map.URI != "" {
		initURL := appendPKey(resolveURL(playlistURL, playlist.Map.URI), p.PKey)
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
			return false, 0, fmt.Errorf("fetch init segment: %w", initErr)
		}
		if initHandler != nil {
			if err := initHandler(initData); err != nil {
				return false, 0, fmt.Errorf("handler init: %w", err)
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

		segmentURL := appendPKey(resolveURL(playlistURL, v.URI), p.PKey)
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
			return false, 0, fmt.Errorf("segment seq=%d: %w", seq, err)
		}
		if handler != nil {
			if err := handler(resp, v.Duration); err != nil {
				return false, 0, fmt.Errorf("handler: %w", err)
			}
		}
		*lastSeq = seq
	}

	return playlist.Closed, time.Duration(playlist.TargetDuration) * time.Second, nil
}

func pickPlaylist(masterBody, baseURL, masterURL, pkey string, resolution, framerate int) (*Playlist, error) {
	p, _, err := m3u8.DecodeFrom(strings.NewReader(masterBody), true)
	if err != nil {
		return nil, fmt.Errorf("stripchat: decode master playlist: %w", err)
	}

	masterPlaylist, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		return nil, errors.New("stripchat: invalid master playlist format")
	}

	resolutions := map[int]*m3u8Resolution{}
	for _, v := range masterPlaylist.Variants {
		parts := strings.Split(v.Resolution, "x")
		if len(parts) != 2 {
			continue
		}
		width, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("stripchat: parse resolution: %w", err)
		}
		framerateVal := 30
		if v.FrameRate >= 59.0 || strings.Contains(v.Name, "FPS:60.0") {
			framerateVal = 60
		}
		if _, exists := resolutions[width]; !exists {
			resolutions[width] = &m3u8Resolution{Framerate: map[int]string{}, Width: width, Alternatives: v.Alternatives}
		}
		resolutions[width].Framerate[framerateVal] = v.URI
	}

	variant, exists := resolutions[resolution]
	if !exists {
		candidates := lo.Filter(lo.Values(resolutions), func(r *m3u8Resolution, _ int) bool {
			return r.Width < resolution
		})
		variant = lo.MaxBy(candidates, func(a, b *m3u8Resolution) bool {
			return a.Width > b.Width
		})
	}
	if variant == nil {
		return nil, fmt.Errorf("stripchat: resolution not found")
	}

	var (
		finalResolution = variant.Width
		finalFramerate  = framerate
		audioPlaylist   string
	)
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

	playlistURL = resolveURL(baseURL, playlistURL)
	if pkey != "" {
		u, err := url.Parse(playlistURL)
		if err == nil {
			q := u.Query()
			q.Set("psch", "v2")
			q.Set("pkey", pkey)
			u.RawQuery = q.Encode()
			playlistURL = u.String()
		}
	}

	return &Playlist{
		PlaylistURL:      playlistURL,
		AudioPlaylistURL: audioPlaylist,
		RootURL:          baseURL,
		MasterURL:        masterURL,
		Resolution:       finalResolution,
		Framerate:        finalFramerate,
	}, nil
}

type m3u8Resolution struct {
	Framerate    map[int]string
	Width        int
	Alternatives []*m3u8.Alternative
}

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

func appendPKey(u, pkey string) string {
	if pkey == "" || u == "" {
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := parsed.Query()
	q.Set("psch", "v2")
	q.Set("pkey", pkey)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func resolveBaseURL(hlsSource string) string {
	u, err := url.Parse(hlsSource)
	if err != nil {
		return hlsSource
	}
	u.RawQuery = ""
	return u.String()
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

// decryptMouflonPlaylist decrypts all MOUFLON-encrypted URIs in a playlist body.
// Handles two formats:
//
//	Format A: URL line directly contains an encrypted token between numbers:
//	  https://.../segment_NUM_TOKEN_NUM_partN.mp4
//	Format B: #EXT-X-MOUFLON:URI: line with encrypted token, followed by
//	  a URL line containing "media.mp4" which is replaced with the decrypted URI.
//
// The MOUFLON tag lines are stripped from the output since the m3u8 parser
// does not recognize them and may crash on unknown tags.
func decryptMouflonPlaylist(body, pdkey string) string {
	lines := strings.Split(body, "\n")
	result := make([]string, 0, len(lines))
	var pendingDecryptedURI string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Strip all #EXT-X-MOUFLON:* tag lines — the m3u8 parser does not
		// recognize them and may panic on unknown tags.
		if strings.HasPrefix(trimmed, "#EXT-X-MOUFLON:") {
			// Format B: #EXT-X-MOUFLON:URI:... — decrypt the URI and store it
			// so the following "media.mp4" placeholder can be replaced.
			if strings.HasPrefix(trimmed, "#EXT-X-MOUFLON:URI:") {
				uri := strings.TrimPrefix(trimmed, "#EXT-X-MOUFLON:URI:")
				decrypted, err := DecryptMouflonURI(uri, pdkey)
				if err == nil && decrypted != uri {
					pendingDecryptedURI = decrypted
				}
			}
			// Skip all MOUFLON tag lines (not written to output).
			continue
		}

		// Format B: replace "media.mp4" placeholder line with decrypted URI.
		// Only match non-comment lines (no "#" prefix) so we don't corrupt
		// #EXT-X-MAP:URI="...media.mp4" or other tag lines.
		if pendingDecryptedURI != "" && !strings.HasPrefix(trimmed, "#") && strings.Contains(trimmed, "media.mp4") {
			result = append(result, pendingDecryptedURI)
			pendingDecryptedURI = ""
			continue
		}

		// Format A: non-comment, non-empty line — try inline decryption.
		if !strings.HasPrefix(trimmed, "#") && trimmed != "" {
			decrypted, err := DecryptMouflonURI(trimmed, pdkey)
			if err == nil && decrypted != trimmed {
				result = append(result, decrypted)
				continue
			}
		}

		result = append(result, line)
		// If a decrypted URI was pending but this line is not media.mp4,
		// discard it — the tag was likely a false positive.
		pendingDecryptedURI = ""
	}

	return strings.Join(result, "\n") + "\n"
}

var (
	pdkeyMu   sync.Mutex
	pdkeyMap  map[string]string
	pdkeyOnce sync.Once
)

// knownPDKeys returns the full pdkey map, lazily populated from player JS.
func knownPDKeys() map[string]string {
	pdkeyOnce.Do(func() {
		pdkeyMu.Lock()
		m := map[string]string{
			"Ook7quaiNgiyuhai": "EQueeGh2kaewa3ch",
			"Zeechoej4aleeshi": "ubahjae7goPoodi6",
			"Zokee2OhPh9kugh4": "Quean4cai9boJa5a",
			"1Dzcc6OjP73LKbtI": "Y64UVwX5RrIWnOLp",
		}
		// Attempt to load additional keys from Stripchat's player JS.
		if extra := loadPDKeysFromPlayerJS(); extra != nil {
			for k, v := range extra {
				m[k] = v
			}
		}
		pdkeyMap = m
		pdkeyMu.Unlock()
	})
	pdkeyMu.Lock()
	defer pdkeyMu.Unlock()
	return pdkeyMap
}

// loadPDKeysFromPlayerJS fetches Stripchat's main page, finds the player JS
// bundle, and extracts pkey→pdkey mappings embedded in it.
func loadPDKeysFromPlayerJS() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := internal.NewReq()

	mainPage, err := client.Get(ctx, "https://stripchat.com/")
	if err != nil {
		fmt.Printf("[stripchat] fetch main page for pdkey extraction: %v\n", err)
		return nil
	}

	// Look for script tags that reference JS bundles.
	reScript := regexp.MustCompile(`<script[^>]+src\s*=\s*['"]([^'"]+\.js[^'"]*)['"]`)
	matches := reScript.FindAllStringSubmatch(mainPage, -1)
	var jsURLs []string
	for _, m := range matches {
		src := m[1]
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		} else if strings.HasPrefix(src, "/") {
			src = "https://stripchat.com" + src
		} else if !strings.HasPrefix(src, "http") {
			src = "https://stripchat.com/" + src
		}
		jsURLs = append(jsURLs, src)
	}

	// Also search inline scripts (no src attribute) for pdkey patterns.
	reInline := regexp.MustCompile(`<script[^>]*>([\s\S]*?)</script>`)
	inlineMatches := reInline.FindAllStringSubmatch(mainPage, -1)

	// The player JS embeds pdkey mappings as object entries:
	// {"pkey":"pdkey"} or {pkey:"pdkey"} or {'pkey':'pdkey'}
	// Match both quoted and unquoted keys.
	// Match potential pdkey entries. Real pdkeys look like random base64
	// (14-18 chars, mixed case + digits, often with +/).
	rePDKey := regexp.MustCompile(`["']?([A-Za-z0-9+/]{14,18})["']?\s*:\s*["']([A-Za-z0-9+/]{14,18})["']`)
	extra := make(map[string]string)

	// Filter: a real pdkey has both a letter and a digit (filters out i18n/camelCase keys).
	isLikelyPDKey := func(s string) bool {
		hasLetter := false
		hasDigit := false
		for _, r := range s {
			if r >= '0' && r <= '9' {
				hasDigit = true
			} else if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				hasLetter = true
			}
		}
		return hasLetter && hasDigit
	}

	addMatches := func(body string) {
		jsMatches := rePDKey.FindAllStringSubmatch(body, -1)
		for _, pm := range jsMatches {
			k, v := pm[1], pm[2]
			if isLikelyPDKey(k) && isLikelyPDKey(v) {
				extra[k] = v
			}
		}
	}

	// Search inline scripts first.
	for _, m := range inlineMatches {
		addMatches(m[1])
	}

	if len(jsURLs) > 0 {
		for _, jsURL := range jsURLs {
			jsBody, err := client.Get(ctx, jsURL)
			if err != nil {
				fmt.Printf("[stripchat] fetch JS bundle %s: %v\n", jsURL, err)
				continue
			}
			addMatches(jsBody)
		}
	}

	if len(extra) > 0 {
		fmt.Printf("[stripchat] extracted %d pdkey mappings from page\n", len(extra))
	} else if len(jsURLs) > 0 {
		fmt.Printf("[stripchat] no pdkey mappings found in %d JS bundle(s)\n", len(jsURLs))
	} else {
		fmt.Printf("[stripchat] no script tags with .js src found in page\n")
	}
	return extra
}

// resolvePDKey resolves a MOUFLON pkey to a pdkey.
func resolvePDKey(pkey string) string {
	m := knownPDKeys()
	if pdkey, ok := m[pkey]; ok {
		return pdkey
	}
	fmt.Printf("[stripchat] unknown pkey %q — segment decryption will likely fail\n", pkey)
	return ""
}
