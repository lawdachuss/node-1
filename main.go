package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"net/url"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/database"
	"github.com/teacat/chaturbate-dvr/coordinator"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/manager"
	"github.com/teacat/chaturbate-dvr/router"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/site"
	"github.com/teacat/chaturbate-dvr/stripchat"
	"github.com/urfave/cli/v2"
)

var tunnelCancel atomic.Value

// diskMonitorStop is closed during graceful shutdown to stop the background disk monitor.
var diskMonitorStop = make(chan struct{})

const logo = `
 ██████╗██╗  ██╗ █████╗ ████████╗██╗   ██╗██████╗ ██████╗  █████╗ ████████╗███████╗
██╔════╝██║  ██║██╔══██╗╚══██╔══╝██║   ██║██╔══██╗██╔══██╗██╔══██╗╚══██╔══╝██╔════╝
██║     ███████║███████║   ██║   ██║   ██║██████╔╝██████╔╝███████║   ██║   █████╗
██║     ██╔══██║██╔══██║   ██║   ██║   ██║██╔══██╗██╔══██╗██╔══██║   ██║   ██╔══╝
╚██████╗██║  ██║██║  ██║   ██║   ╚██████╔╝██║  ██║██████╔╝██║  ██║   ██║   ███████╗
 ╚═════╝╚═╝  ╚═╝╚═╝  ╚═╝   ╚═╝    ╚═════╝ ╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝   ╚═╝   ╚══════╝
██████╗ ██╗   ██╗██████╗
██╔══██╗██║   ██║██╔══██╗
██║  ██║██║   ██║██████╔╝
██║  ██║╚██╗ ██╔╝██╔══██╗
██████╔╝ ╚████╔╝ ██║  ██║
╚═════╝   ╚═══╝  ╚═╝  ╚═╝`

var version = "dev"

// loadDotEnv loads KEY=VALUE pairs from a .env file into the process environment,
// but does NOT overwrite existing environment variables.
// It tries the given path first, then falls back to the executable's directory.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		// Try relative to executable directory
		exe, err2 := os.Executable()
		if err2 == nil {
			f, err = os.Open(filepath.Join(filepath.Dir(exe), path))
		}
		if err != nil {
			return
		}
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		v = strings.Trim(v, `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func pipeReader(r io.ReadCloser, buf *server.LogBuffer, orig io.WriteCloser) {
	br := bufio.NewReaderSize(r, 4096)
	for {
		chunk := make([]byte, 4096)
		n, err := br.Read(chunk)
		if n > 0 {
			data := chunk[:n]
			orig.Write(data)
			buf.Write(data)
		}
		if err != nil {
			return
		}
	}
}

func main() {
	loadDotEnv(".env")
	app := &cli.App{
		Name:    "chaturbate-dvr",
		Version: version,
		Usage:   "Record your favorite Chaturbate streams automatically. 😎🫵",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "username",
				Aliases: []string{"u"},
				Usage:   "The username of the channel to record",
			},
			&cli.StringFlag{
				Name:  "site",
				Usage: "Site to record from: chaturbate or stripchat (default: chaturbate)",
				Value: "chaturbate",
			},
			&cli.StringFlag{
				Name:  "admin-username",
				Usage: "Username for web authentication (optional)",
				Value: "",
			},
			&cli.StringFlag{
				Name:  "admin-password",
				Usage: "Password for web authentication (optional)",
				Value: "",
			},
			&cli.IntFlag{
				Name:  "framerate",
				Usage: "Desired framerate (FPS)",
				Value: 60,
			},
			&cli.IntFlag{
				Name:  "resolution",
				Usage: "Desired resolution (e.g., 2160 for 4K)",
				Value: 2160,
			},
			&cli.StringFlag{
				Name:  "pattern",
				Usage: "Template for naming recorded videos",
				Value: "videos/{{.Username}}_{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}{{if .Sequence}}_{{.Sequence}}{{end}}",
			},
			&cli.IntFlag{
				Name:  "max-duration",
				Usage: "Split video into segments every N minutes ('0' to disable)",
				Value: 60,
			},
			&cli.IntFlag{
				Name:  "max-filesize",
				Usage: "Split video into segments every N MB ('0' to disable)",
				Value: 0,
			},
			&cli.StringFlag{
				Name:    "port",
				Aliases: []string{"p"},
				Usage:   "Port for the web interface and API",
				Value:   "8080",
			},
			&cli.IntFlag{
				Name:  "interval",
				Usage: "Check if the channel is online every N minutes",
				Value: 1,
			},
			&cli.StringFlag{
				Name:    "cookies",
				Usage:   "Cookies to use in the request (format: key=value; key2=value2)",
				EnvVars: []string{"COOKIES"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "user-agent",
				Usage:   "Custom User-Agent for the request",
				EnvVars: []string{"USER_AGENT"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:  "domain",
				Usage: "Chaturbate domain to use",
				Value: "https://chaturbate.com/",
			},
			&cli.StringFlag{
				Name:    "proxy-url",
				Usage:   "HTTP/SOCKS5 proxy URL for Chaturbate requests",
				EnvVars: []string{"PROXY_URL", "PROXY_SERVER", "ALL_PROXY", "all_proxy", "SOCKS_PROXY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "proxy-username",
				Usage:   "Proxy username",
				EnvVars: []string{"PROXY_USERNAME"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "proxy-password",
				Usage:   "Proxy password",
				EnvVars: []string{"PROXY_PASSWORD"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "ffmpeg-path",
				Usage:   "Path to ffmpeg executable (e.g. C:\\ffmpeg\\bin\\ffmpeg.exe). If not set, PATH is used.",
				EnvVars: []string{"FFMPEG_PATH"},
				Value:   "",
			},
			&cli.BoolFlag{
				Name:  "no-tunnel",
				Usage: "Skip automatic Cloudflare tunnel startup (useful when script manages it separately)",
				Value: false,
			},
			&cli.BoolFlag{
				Name:  "compress",
				Usage: "Compress recorded files (.ts or .mp4) to .mkv using ffmpeg after recording",
				Value: false,
			},
			&cli.StringFlag{
				Name:    "output-dir",
				Usage:   "Directory to move completed recordings to (empty = keep in place)",
				EnvVars: []string{"OUTPUT_DIR"},
				Value:   "",
			},
			&cli.BoolFlag{
				Name:    "per-model-folder",
				Usage:   "Create a subdirectory per model inside --output-dir",
				EnvVars: []string{"PER_MODEL_FOLDER"},
				Value:   false,
			},
			&cli.BoolFlag{
				Name:    "delete-local-after-upload",
				Usage:   "Delete local recordings and preview files after successful remote upload",
				EnvVars: []string{"DELETE_LOCAL_AFTER_UPLOAD"},
				Value:   true,
			},
			&cli.IntFlag{
				Name:    "orphan-cleanup-interval",
				Usage:   "Minutes between periodic orphan file cleanup and thumbnail scans (0 = disabled, run once at startup)",
				EnvVars: []string{"ORPHAN_CLEANUP_INTERVAL"},
				Value:   0,
			},
			&cli.IntFlag{
				Name:    "disk-warning-percent",
				Usage:   "Log warning when disk usage exceeds this percentage (0 = disabled)",
				EnvVars: []string{"DISK_WARNING_PERCENT"},
				Value:   80,
			},
			&cli.IntFlag{
				Name:    "disk-critical-percent",
				Usage:   "Auto-delete oldest local recordings when disk usage exceeds this percentage (0 = disabled)",
				EnvVars: []string{"DISK_CRITICAL_PERCENT"},
				Value:   90,
			},
			&cli.StringFlag{
				Name:    "session-duration",
				Usage:   "Recording session length (e.g. \"5h20m0s\"); after this elapses the system stops, processes all pending files, then restarts (empty = continuous recording)",
				EnvVars: []string{"SESSION_DURATION"},
				Value:   "",
			},
			&cli.IntFlag{
				Name:    "max-local-age-days",
				Usage:   "Delete local recordings older than this many days if already uploaded (0 = disabled)",
				EnvVars: []string{"MAX_LOCAL_AGE_DAYS"},
				Value:   0,
			},
			&cli.IntFlag{
				Name:    "min-duration-before-upload",
				Usage:   "Minimum video duration in seconds before uploading; shorter videos wait and merge with the next recording (0 = disabled)",
				EnvVars: []string{"MIN_DURATION_BEFORE_UPLOAD"},
				Value:   1200,
			},
			&cli.StringFlag{
				Name:    "voesx-api-key",
				Usage:   "API key for VOE.sx uploads",
				EnvVars: []string{"VOESX_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "streamtape-login",
				Usage:   "Login username for Streamtape uploads",
				EnvVars: []string{"STREAMTAPE_LOGIN"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "streamtape-key",
				Usage:   "API key for Streamtape uploads",
				EnvVars: []string{"STREAMTAPE_KEY", "STREAMTAPE_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "mixdrop-email",
				Usage:   "Email for Mixdrop uploads",
				EnvVars: []string{"MIXDROP_EMAIL"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "mixdrop-token",
				Usage:   "API token for Mixdrop uploads",
				EnvVars: []string{"MIXDROP_TOKEN", "MIXDROP_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "seekstreaming-key",
				Usage:   "API key for SeekStreaming uploads",
				EnvVars: []string{"SEEKSTREAMING_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "vidhide-api-key",
				Usage:   "API key for VidHide uploads",
				EnvVars: []string{"VIDHIDE_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "streamwish-api-key",
				Usage:   "API key for StreamWish uploads",
				EnvVars: []string{"STREAMWISH_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "upnshare-key",
				Usage:   "API key for UPnShare uploads",
				EnvVars: []string{"UPNSHARE_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "supabase-url",
				Usage:   "Supabase project URL for remote data persistence (REST API fallback)",
				EnvVars: []string{"SUPABASE_URL"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "supabase-api-key",
				Usage:   "Supabase anon/public API key for REST API fallback",
				EnvVars: []string{"SUPABASE_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "stripchat-pdkey",
				Usage:   "MOUFLON v2 decryption key for Stripchat HLS streams",
				EnvVars: []string{"STRIPCHAT_PDKEY"},
				Value:   "",
			},

			// ── Distributed shards/nodes ────────────────────────────────────
			&cli.StringFlag{
				Name:    "channel-pool-mode",
				Usage:   "Channel distribution mode: 'isolated' (default) or 'pooled'",
				EnvVars: []string{"CHANNEL_POOL_MODE"},
				Value:   "isolated",
			},
			&cli.StringFlag{
				Name:    "node-id",
				Usage:   "Unique node identifier for distributed mode (auto-detected if unset)",
				EnvVars: []string{"NODE_ID"},
				Value:   "",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "clean-previews",
				Usage: "Clear slow/stale preview URLs from Supabase (preview_images + recordings)",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "supabase-url",
						Usage:   "Supabase project URL",
						EnvVars: []string{"SUPABASE_URL"},
					},
					&cli.StringFlag{
						Name:    "supabase-api-key",
						Usage:   "Supabase anon/public API key",
						EnvVars: []string{"SUPABASE_API_KEY"},
					},
					&cli.IntFlag{
						Name:  "days",
						Usage: "Only clear previews uploaded within the last N days (default 2)",
						Value: 2,
					},
					&cli.BoolFlag{
						Name:  "all",
						Usage: "Clear ALL preview URLs regardless of age (ignores --days)",
					},
					&cli.BoolFlag{
						Name:  "dry-run",
						Usage: "Show how many rows match without actually clearing them",
					},
				},
				Action: cleanPreviews,
			},
		},
		Action: start,
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

// cleanPreviews clears preview URLs from Supabase for recent (or all) rows.
func cleanPreviews(c *cli.Context) error {
	url := c.String("supabase-url")
	key := c.String("supabase-api-key")
	if url == "" || key == "" {
		return fmt.Errorf("SUPABASE_URL and SUPABASE_API_KEY must be set (env or flags)")
	}

	client := database.NewClient(url, key)

	var before string
	if !c.Bool("all") {
		before = time.Now().AddDate(0, 0, -c.Int("days")).UTC().Format(time.RFC3339)
	}

	if c.Bool("dry-run") {
		piCount, err := countPreviewImages(client, before)
		if err != nil {
			return fmt.Errorf("count preview_images: %w", err)
		}
		recCount, err := countRecordings(client, before)
		if err != nil {
			return fmt.Errorf("count recordings: %w", err)
		}
		fmt.Printf("[dry-run] Would clear %d preview_images rows and %d recordings rows (before=%s)\n", piCount, recCount, before)
		return nil
	}

	if err := client.ClearPreviewURLs(before); err != nil {
		return err
	}
	fmt.Printf("Cleared preview URLs (before=%s) from preview_images + recordings tables.\n", before)
	return nil
}

func countPreviewImages(client *database.Client, before string) (int, error) {
	path := "/preview_images?preview_url=not.is.null&select=filename"
	if before != "" {
		path += fmt.Sprintf("&uploaded_at=lt.%s", url.QueryEscape(before))
	}
	var rows []map[string]interface{}
	if err := client.Get(path, &rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

func countRecordings(client *database.Client, before string) (int, error) {
	path := "/recordings?preview_url=not.is.null&select=filename"
	if before != "" {
		path += fmt.Sprintf("&timestamp=lt.%s", url.QueryEscape(before))
	}
	var rows []map[string]interface{}
	if err := client.Get(path, &rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

func start(c *cli.Context) error {
	started := time.Now()

	logBuf := server.GetLogBuffer()
	origStdout, origStderr := os.Stdout, os.Stderr
	stdoutR, stdoutW, _ := os.Pipe()
	stderrR, stderrW, _ := os.Pipe()
	os.Stdout = stdoutW
	os.Stderr = stderrW
	log.SetOutput(os.Stderr)
	gin.DefaultWriter = os.Stdout
	gin.DefaultErrorWriter = os.Stderr
	go pipeReader(stdoutR, logBuf, origStdout)
	go pipeReader(stderrR, logBuf, origStderr)

	server.LoadWorkflowLogs("workflow-setup.log")

	fmt.Println(logo)

	var err error
	server.Config, err = config.New(c)
	if err != nil {
		return fmt.Errorf("new config: %w", err)
	}
	fmt.Printf("[startup] config loaded in %v\n", time.Since(started).Round(time.Millisecond))
	fmt.Printf("[startup] upload keys — streamwish=%d vidhide=%d upnshare=%d\n",
		len(server.Config.StreamWishAPIKeys), len(server.Config.VidHideAPIKeys),
		len(server.Config.UpnshareKeys))

	// ── Startup self-checks: fail LOUD, never silently ───────────────────
	// These catch the two classes of "works on some nodes, silently degrades on
	// others" problems at boot instead of mid-recording:
	//   1. ffmpeg/ffprobe missing -> mux/probe/normalize/thumbnail/preview fail.
	//   2. channel_logs schema gap (unapplied migrate.sql) -> node_id always
	//      NULL and the error-log query times out.
	if err := config.ValidateFFmpeg(); err != nil {
		fmt.Printf("⚠️  [startup] FFMPEG/FFPROBE CHECK FAILED: %v\n", err)
		fmt.Println("   Thumbnails, sprite sheets, previews, A/V muxing and duration")
		fmt.Println("   probing will FAIL. Install ffmpeg/ffprobe (e.g. `winget install ffmpeg`)")
		fmt.Println("   or set FFMPEG_PATH in .env to a valid binary path.")
	}
	if dbClient := server.GetDBClient(); dbClient != nil {
		if problems := dbClient.CheckChannelLogsSchema(); len(problems) > 0 {
			for _, p := range problems {
				fmt.Printf("⚠️  [startup] DATABASE SCHEMA PROBLEM: %s\n", p)
			}
		}
	}

	// Load cookies from Supabase if available (overrides .env)
	if server.Config.SupabaseURL != "" && server.Config.SupabaseAPIKey != "" {
		fmt.Println("📦 Loading cookies from Supabase...")
		if err := server.LoadSettings(); err != nil {
			fmt.Printf("⚠️  Failed to load cookies from Supabase: %v\n", err)
			fmt.Println("   Falling back to .env cookies")
		} else {
			fmt.Println("✅ Cookies loaded from Supabase")
		}
		// Persist the merged config (env + Supabase) back to Supabase so that
		// upload credentials set in .env survive on subsequent runs where .env
		// is absent (e.g. GitHub Actions). Best-effort — a failure here is not fatal.
		if err := server.SaveSettings(); err != nil {
			fmt.Printf("⚠️  Failed to persist merged settings to Supabase: %v\n", err)
		}
	}

	if server.Config.Cookies == "" || server.Config.UserAgent == "" {
		fmt.Println("⚠️  Chaturbate API requests may fail — COOKIES and USER_AGENT not set in .env or Supabase")
		fmt.Println("   Open .env and fill in your browser cookies from chaturbate.com:")
		fmt.Println("   Chrome 146: F12 → Application → Cookies → chaturbate.com → copy all as string")
		fmt.Println("   OR update cookies in Supabase via the web UI")
		fmt.Println("   IMPORTANT: Use Chrome 146+ on Windows for cookie collection so the TLS")
		fmt.Println("   fingerprint matches the httpcloak preset.")
		fmt.Println()
	}

	// Persist every log line to Supabase channel_logs (with parsed level +
	// username + node ID) so errors are queryable across all nodes and survive
	// restarts. Best-effort and non-blocking: the buffer drops lines on DB
	// backpressure and never blocks the logging hot path.
	if server.GetDBClient() != nil {
		nodeID := server.NodeID()
		server.SetLogSink(func(level, username, message string) {
			client := server.GetDBClient()
			if client == nil {
				return
			}
			_ = client.SaveLogRobust(&database.ChannelLog{
				NodeID:   nodeID,
				Username: username,
				LogLevel: level,
				Message:  message,
			})
		})
	}

	// Warm up TLS sessions with Cloudflare in the background so server
	// startup is not delayed by slow/unreachable SOCKS5 proxies.
	// The httpcloak pool-level dial doesn't propagate the context deadline
	// during the SOCKS5 handshake, causing it to block for the OS TCP
	// timeout (~30-120s) instead of the desired 10s context deadline.
	go func() {
		warmupT := time.Now()
		warmupCtx, warmupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		internal.WarmupChaturbate(warmupCtx)
		warmupCancel()
		warmupCtx, warmupCancel = context.WithTimeout(context.Background(), 10*time.Second)
		internal.WarmupStripchat(warmupCtx)
		warmupCancel()
		fmt.Printf("[startup] TLS warmup completed in %v\n", time.Since(warmupT).Round(time.Millisecond))
	}()

	server.Manager, err = manager.New()
	if err != nil {
		return fmt.Errorf("new manager: %w", err)
	}
	fmt.Printf("[startup] manager created in %v\n", time.Since(started).Round(time.Millisecond))

	// ── Distributed coordinator ──────────────────────────────────────────
	var coord *coordinator.Coordinator
	var mgr *manager.Manager
	if m, ok := server.Manager.(*manager.Manager); ok {
		mgr = m
	}
	if server.ChannelPoolMode() == entity.PoolModePooled && mgr != nil {
		dbClient := server.GetDBClient()
		if dbClient != nil {
			coord = coordinator.New(dbClient, mgr)
			coord.LiveCheck = &liveChecker{}
			mgr.Coordinator = coord
			fmt.Printf("[startup] coordinator created for node %q (pooled mode)\n", coord.NodeID)
		} else {
			fmt.Println("[WARN] Supabase not configured — pooled mode requires Supabase")
		}
	}

	// Graceful shutdown: catch SIGTERM/SIGINT, stop all recording
	// channels first (so their Cleanup() runs and queues files), then
	// wait for post-processing + uploads + Supabase
	// saves to finish before exiting.  A progress ticker logs every
	// 30 s so the GitHub Actions log shows the process is still alive.
	go func() {
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh

		server.SetShuttingDown()

		channels := server.Manager.ChannelInfo()
		fmt.Printf("\n[SHUTDOWN] received %v - stopping %d channel(s)...\n", sig, len(channels))
		for _, ch := range channels {
			fmt.Printf("[SHUTDOWN]   stopping %s\n", ch.Username)
		}

		// Listen for a second Ctrl+C to force exit immediately
		go func() {
			<-sigCh
			fmt.Println("\n[SHUTDOWN] received second interrupt - forcing immediate exit")
			os.Exit(1)
		}()

		// In pooled mode: start draining so other nodes stop assigning to us
		if coord != nil {
			coord.StartDraining()
		}

		server.Manager.StopSession()
		server.Manager.StopAllChannels()
		fmt.Println("[SHUTDOWN] all channels stopped - waiting for mux/thumbnail/upload/Supabase to finish...")

		shutdownDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			elapsed := 30
			for {
				select {
				case <-ticker.C:
					fmt.Printf("[SHUTDOWN] still finalizing... (%ds elapsed)\n", elapsed)
					elapsed += 30
				case <-shutdownDone:
					return
				}
			}
		}()

		done := make(chan struct{}, 1)
		go func() {
			server.Manager.WaitForAllChannels()
			fmt.Println("[SHUTDOWN] all recordings finalized - waiting for uploads and Supabase saves...")
			server.Manager.WaitForUploads()

			// In pooled mode: release channels after all uploads complete
			if coord != nil {
				fmt.Println("[SHUTDOWN] releasing channel assignments...")
				coord.Stop()
			}

			close(shutdownDone)
			fmt.Println("[SHUTDOWN] all uploads and Supabase saves complete - exiting cleanly")
			close(diskMonitorStop)
			if c, ok := tunnelCancel.Load().(context.CancelFunc); ok && c != nil {
				c()
			}
			done <- struct{}{}
		}()

		select {
		case <-done:
			os.Exit(0)
		case <-time.After(55 * time.Minute):
			fmt.Println("[SHUTDOWN] timeout (55 min) - forcing exit")
			os.Exit(1)
		}
	}()

	// init web interface if username is not provided
	if server.Config.Username == "" {
		fmt.Printf("👋 Visit http://localhost:%s to use the Web UI\n", c.String("port"))
		if !c.Bool("no-tunnel") {
			go startTunnel(c.String("port"))
		} else {
			fmt.Println("🚇 Tunnel disabled (--no-tunnel) — script will manage it separately")
		}

		loadT := time.Now()
		if server.IsPooledMode() {
			if mgr == nil {
				return fmt.Errorf("pooled mode requires manager (unexpected nil)")
			}
			if err := mgr.LoadPooledConfig(); err != nil {
				return fmt.Errorf("load pooled config: %w", err)
			}
			if coord != nil {
				coord.Start(context.Background())
			}
		} else {
			if err := server.Manager.LoadConfig(); err != nil {
				return fmt.Errorf("load config: %w", err)
			}
		}
		fmt.Printf("[startup] LoadConfig completed in %v\n", time.Since(loadT).Round(time.Millisecond))

		server.Manager.StartSession(server.Config.SessionDurationParsed)

		// Start background disk monitor
		go server.StartDiskMonitor(diskMonitorStop)

		bindT := time.Now()
		err := router.SetupRouter().Run(":" + c.String("port"))
		fmt.Printf("[startup] HTTP server listened for %v before returning\n", time.Since(bindT).Round(time.Millisecond))
		return err
	}

	// else create a channel with the provided username
	go server.StartDiskMonitor(diskMonitorStop)

	if err := server.Manager.CreateChannel(&entity.ChannelConfig{
		Site:                    c.String("site"),
		Username:                c.String("username"),
		Framerate:               c.Int("framerate"),
		Resolution:              c.Int("resolution"),
		Pattern:                 c.String("pattern"),
		MaxDuration:             c.Int("max-duration"),
		MaxFilesize:             c.Int("max-filesize"),
		Compress:                c.Bool("compress"),
		MinDurationBeforeUpload: c.Int("min-duration-before-upload"),
	}, false); err != nil {
		return fmt.Errorf("create channel: %w", err)
	}

	server.Manager.StartSession(server.Config.SessionDurationParsed)

	// block forever
	select {}
}

// liveChecker implements coordinator.LivenessChecker using the site adapters.
type liveChecker struct{}

func (l *liveChecker) IsLive(ctx context.Context, siteName, username string) bool {
	var siteImpl site.Site
	switch siteName {
	case "stripchat":
		siteImpl = stripchat.NewStripchatSite()
	default:
		siteImpl = site.NewChaturbateSite()
	}

	// Use a no-proxy request for liveness checks. The chatvideocontext GET
	// endpoint does not need the Netherlands SOCKS5 proxy — we're just checking
	// room status, not fetching HLS streams. Bypassing the proxy means the
	// liveness check still works when the proxy pool is temporarily empty.
	status, err := siteImpl.GetRoomStatus(ctx, internal.NewNoProxyReq(), username)
	if err != nil {
		return false
	}

	return status == site.StatusPublic || status == site.StatusPrivate
}

func startTunnel(port string) {
	cloudflaredPath, err := exec.LookPath("cloudflared")
	if err != nil {
		fmt.Println("💡 Install cloudflared (winget install Cloudflare.cloudflared) for a public tunnel URL")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	tunnelCancel.Store(cancel)

	cmd := exec.CommandContext(ctx, cloudflaredPath, "tunnel", "--url", "http://localhost:"+port, "--protocol", "http2")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Printf("⚠️  tunnel pipe: %v\n", err)
		return
	}

	if err := cmd.Start(); err != nil {
		fmt.Printf("⚠️  tunnel: %v\n", err)
		tunnelCancel.Store(context.CancelFunc(nil))
		return
	}

	tunnelURLCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		re := regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
		for scanner.Scan() {
			if m := re.FindString(scanner.Text()); m != "" {
				tunnelURLCh <- m
				return
			}
		}
	}()

	select {
	case tunnelURL := <-tunnelURLCh:
		fmt.Printf("🌍 Public: %s\n\n", tunnelURL)
		fmt.Printf("::notice title=Web UI::%s\n", tunnelURL)
	case <-time.After(30 * time.Second):
		fmt.Println("⚠️  Tunnel URL not obtained within 30s")
	}

	cmd.Wait()
}
