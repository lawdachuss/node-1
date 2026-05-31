package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/manager"
	"github.com/teacat/chaturbate-dvr/router"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/urfave/cli/v2"
)

var tunnelCancel context.CancelFunc

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
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
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
				Value:   "",
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
				Value: 30,
			},
			&cli.IntFlag{
				Name:  "resolution",
				Usage: "Desired resolution (e.g., 1080 for 1080p)",
				Value: 1080,
			},
			&cli.StringFlag{
				Name:  "pattern",
				Usage: "Template for naming recorded videos",
				Value: "videos/{{.Username}}_{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}{{if .Sequence}}_{{.Sequence}}{{end}}",
			},
			&cli.IntFlag{
				Name:  "max-duration",
				Usage: "Split video into segments every N minutes ('0' to disable)",
				Value: 0,
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
				EnvVars: []string{"PROXY_URL", "PROXY_SERVER"},
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
				Usage: "Compress recorded files (.ts or .mp4) to .mkv using ffmpeg after recording (auto-enabled if ffmpeg is installed)",
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
			&cli.StringFlag{
				Name:    "turboviplay-api-key",
				Usage:   "API key for TurboViPlay uploads",
				EnvVars: []string{"TURBOVIPLAY_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "voesx-api-key",
				Usage:   "API key for VOE.sx uploads",
				EnvVars: []string{"VOESX_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "sendcm-api-key",
				Usage:   "API key for SendCM uploads (optional, guest upload if empty)",
				EnvVars: []string{"SENDCM_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "byse-api-key",
				Usage:   "API key for Byse uploads",
				EnvVars: []string{"BYSE_API_KEY"},
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
				Name:    "pixeldrain-token",
				Usage:   "API token for PixelDrain uploads",
				EnvVars: []string{"PIXELDRAIN_TOKEN", "PIXELDRAIN_API_KEY"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "github-token",
				Usage:   "GitHub Personal Access Token for preview/sprite image uploads",
				EnvVars: []string{"GITHUB_TOKEN"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "github-repo",
				Usage:   "GitHub repository for preview images (owner/repo)",
				EnvVars: []string{"GITHUB_REPO"},
				Value:   "",
			},
			&cli.StringFlag{
				Name:    "github-branch",
				Usage:   "GitHub branch for preview images (default: main)",
				EnvVars: []string{"GITHUB_BRANCH"},
				Value:   "main",
			},
			&cli.StringFlag{
				Name:    "github-preview-path",
				Usage:   "Path in GitHub repo for preview images (default: previews)",
				EnvVars: []string{"GITHUB_PREVIEW_PATH"},
				Value:   "previews",
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
		},
		Action: start,
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func start(c *cli.Context) error {
	fmt.Println(logo)

	var err error
	server.Config, err = config.New(c)
	if err != nil {
		return fmt.Errorf("new config: %w", err)
	}
	if server.Config.Cookies == "" || server.Config.UserAgent == "" {
		fmt.Println("⚠️  Chaturbate API requests may fail — COOKIES and USER_AGENT not set in .env")
		fmt.Println("   Open .env and fill in your browser cookies from chaturbate.com:")
		fmt.Println("   Chrome 146: F12 → Application → Cookies → chaturbate.com → copy all as string")
		fmt.Println("   IMPORTANT: Use Chrome 146+ on Windows for cookie collection so the TLS")
		fmt.Println("   fingerprint matches the httpcloak preset.")
		fmt.Println()
	}

	// Warm up TLS session with Cloudflare before any API calls.
	// This establishes TLS session tickets via a HEAD request to chaturbate.com,
	// so subsequent API calls use TLS resumption (like a returning browser).
	ctx, warmupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	internal.WarmupChaturbate(ctx)
	warmupCancel()

	server.Manager, err = manager.New()
	if err != nil {
		return fmt.Errorf("new manager: %w", err)
	}

	// Graceful shutdown: catch SIGTERM/SIGINT, stop all recording
	// channels first (so their Cleanup() runs and queues files into
	// UploadWg), then wait for post-processing + uploads + Supabase
	// saves to finish before exiting.  A progress ticker logs every
	// 30 s so the GitHub Actions log shows the process is still alive.
	go func() {
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh

		channels := server.Manager.ChannelInfo()
		fmt.Printf("\n[SHUTDOWN] received %v — stopping %d channel(s)...\n", sig, len(channels))
		for _, ch := range channels {
			fmt.Printf("[SHUTDOWN]   stopping %s\n", ch.Username)
		}

		// Listen for a second Ctrl+C to force exit immediately
		go func() {
			<-sigCh
			fmt.Println("\n[SHUTDOWN] received second interrupt — forcing immediate exit")
			os.Exit(1)
		}()

		server.Manager.StopAllChannels()
		fmt.Println("[SHUTDOWN] all channels stopped — waiting for mux/thumbnail/upload/Supabase to finish...")

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
			fmt.Println("[SHUTDOWN] all recordings finalized — waiting for uploads and Supabase saves...")
			server.Manager.WaitForUploads()
			close(shutdownDone)
			fmt.Println("[SHUTDOWN] all uploads and Supabase saves complete — exiting cleanly")
			if tunnelCancel != nil {
				tunnelCancel()
			}
			done <- struct{}{}
		}()

		select {
		case <-done:
			os.Exit(0)
		case <-time.After(5 * time.Minute):
			fmt.Println("[SHUTDOWN] timeout (5 min) — forcing exit")
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

		if err := server.Manager.LoadConfig(); err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		return router.SetupRouter().Run(":" + c.String("port"))
	}

	// else create a channel with the provided username
	if err := server.Manager.CreateChannel(&entity.ChannelConfig{
		Username:    c.String("username"),
		Framerate:   c.Int("framerate"),
		Resolution:  c.Int("resolution"),
		Pattern:     c.String("pattern"),
		MaxDuration: c.Int("max-duration"),
		MaxFilesize: c.Int("max-filesize"),
		Compress:    c.Bool("compress"),
	}, false); err != nil {
		return fmt.Errorf("create channel: %w", err)
	}

	// block forever
	select {}
}

func startTunnel(port string) {
	cloudflaredPath, err := exec.LookPath("cloudflared")
	if err != nil {
		fmt.Println("💡 Install cloudflared (winget install Cloudflare.cloudflared) for a public tunnel URL")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	tunnelCancel = cancel

	cmd := exec.CommandContext(ctx, cloudflaredPath, "tunnel", "--url", "http://localhost:"+port, "--protocol", "http2")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Printf("⚠️  tunnel pipe: %v\n", err)
		return
	}

	if err := cmd.Start(); err != nil {
		fmt.Printf("⚠️  tunnel: %v\n", err)
		tunnelCancel = nil
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
	case <-time.After(30 * time.Second):
		fmt.Println("⚠️  Tunnel URL not obtained within 30s")
	}

	cmd.Wait()
}
