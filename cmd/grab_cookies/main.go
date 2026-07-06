package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sardanioss/httpcloak"
	"github.com/teacat/chaturbate-dvr/internal/proxy"
)

func main() {
	exitCode := 0
	defer func() { os.Exit(exitCode) }()

	loadDotEnv(".env")

	userAgent := os.Getenv("USER_AGENT")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	}
	fmt.Println("=== Cookie Grabber ===")
	fmt.Printf("User-Agent: %s\n", userAgent)
	fmt.Println()

	// Use env PROXY_URL first (already tested by workflow), fall back to dynamic fetch
	envProxies := getProxyURLs()
	var proxies []proxy.ProxyResult
	if len(envProxies) > 0 {
		fmt.Printf("Using %d proxies from PROXY_URL\n", len(envProxies))
		for _, p := range envProxies {
			proxies = append(proxies, proxy.ProxyResult{URL: p, OK: true})
		}
	} else {
		fmt.Println("Fetching SOCKS5 proxies from public lists...")
		ctx := context.Background()
		var err error
		proxies, err = proxy.FetchProxies(ctx, 5)
		if err != nil {
			fmt.Printf("  [FAIL] No dynamic proxies found: %v\n", err)
			exitCode = 1
			return
		}
		fmt.Printf("Using %d dynamically discovered proxies\n", len(proxies))
	}
	for i, p := range proxies {
		fmt.Printf("  [%d] %s [%s]\n", i+1, p.URL, p.Country)
	}
	fmt.Println()

	// Build working proxy URL list
	var workingURLs []string
	for _, p := range proxies {
		if p.OK {
			workingURLs = append(workingURLs, p.URL)
		}
	}
	if len(workingURLs) == 0 {
		fmt.Println("\n[FAIL] No working proxies found — cannot bypass face-id verification")
		exitCode = 1
		return
	}

	// Test all proxies in parallel. Stop on first success — we just need
	// fresh __cf_bm; old cf_clearance is preserved from .env.
	fmt.Println("[1/1] Fetching cookies (parallel, stop on first success)...")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := atomic.Bool{}
	var wg sync.WaitGroup
	var result atomic.Value // stores map[string]string
	for pi, p := range workingURLs {
		wg.Add(1)
		go func(idx int, proxyURL string) {
			defer wg.Done()
			if done.Load() {
				return
			}
			fmt.Printf("  Proxy [%d/%d]: %s\n", idx+1, len(workingURLs), proxyURL)
			cookies := tryHTTPCloak(ctx, proxyURL, userAgent)
			if cookies == nil || done.Load() {
				return
			}
			if result.Load() == nil {
				result.Store(cookies)
			}
			fmt.Println("  Got cookies — saving and exiting")
			done.Store(true)
			cancel()
			saveAndExit(cookies, userAgent)
		}(pi, p)
	}
	wg.Wait()

	if result.Load() == nil {
		fmt.Println("\n[FAIL] Could not obtain cookies from any method")
		exitCode = 1
	}
}

func saveAndExit(cookies map[string]string, userAgent string) {
	var parts []string
	for k, v := range cookies {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	cookieStr := strings.Join(parts, "; ")

	updateEnvFile(".env", "COOKIES", cookieStr)
	if userAgent != "" {
		updateEnvFile(".env", "USER_AGENT", userAgent)
	}

	fmt.Println("\n=== COOKIES UPDATED ===")
	if v, ok := cookies["cf_clearance"]; ok {
		fmt.Printf("cf_clearance: fresh! (timestamp: %s)\n", extractTimestamp(v))
	}
	if v, ok := cookies["__cf_bm"]; ok {
		fmt.Printf("__cf_bm: fresh! (timestamp: %s)\n", extractTimestamp(v))
	}
	fmt.Printf("\nTotal cookies: %d\n", len(cookies))
}

func tryHTTPCloak(ctx context.Context, proxyURL, userAgent string) map[string]string {
	opts := []httpcloak.Option{
		httpcloak.WithTimeout(15 * time.Second),
	}
	if proxyURL != "" {
		opts = append(opts, httpcloak.WithProxy(proxyURL))
	} else {
		fmt.Println("  No proxy configured, trying direct...")
	}

	client := httpcloak.New("chrome-146-windows", opts...)
	if c, ok := interface{}(client).(interface{ Close() error }); ok {
		defer c.Close()
	}

	headers := map[string][]string{
		"User-Agent": {userAgent},
		"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
	}

	resp, err := client.Do(ctx, &httpcloak.Request{
		Method:  "GET",
		URL:     "https://chaturbate.com",
		Headers: headers,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return nil
	}
	fmt.Printf("  Status: %d\n", resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	cookies := extractCookies(resp.Headers)
	if len(cookies) == 0 {
		return nil
	}

	fmt.Printf("  Got %d cookies\n", len(cookies))
	for k, v := range cookies {
		if k == "cf_clearance" || k == "__cf_bm" {
			fmt.Printf("    %s = ...%s (ts: %s)\n", k, truncate(v, 20), extractTimestamp(v))
		}
	}
	return cookies
}

func extractCookies(headers map[string][]string) map[string]string {
	cookies := make(map[string]string)
	for key, vals := range headers {
		if strings.EqualFold(key, "Set-Cookie") {
			for _, setCookie := range vals {
				if idx := strings.Index(setCookie, "="); idx > 0 {
					name := setCookie[:idx]
					rest := setCookie[idx+1:]
					if idx2 := strings.Index(rest, ";"); idx2 > 0 {
						cookies[name] = rest[:idx2]
					} else {
						cookies[name] = rest
					}
				}
			}
		}
	}
	return cookies
}

// getProxyURLs returns all proxy URLs from the environment.
// Supports PROXY_URL (comma-separated for failover), falling back to ALL_PROXY.
func getProxyURLs() []string {
	raw := os.Getenv("PROXY_URL")
	if raw == "" {
		raw = os.Getenv("ALL_PROXY")
	}
	if raw == "" {
		return nil
	}
	var urls []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			urls = append(urls, part)
		}
	}
	return urls
}

// ─── helpers ───────────────────────────────────────────────

func parseCookies(s string) map[string]string {
	m := make(map[string]string)
	for _, pair := range strings.Split(s, ";") {
		pair = strings.TrimSpace(pair)
		if idx := strings.Index(pair, "="); idx > 0 {
			m[pair[:idx]] = pair[idx+1:]
		}
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func extractTimestamp(cfClearance string) string {
	idx := strings.Index(cfClearance, "-")
	if idx < 0 {
		return "unknown"
	}
	tsStr := cfClearance[idx+1:]
	if idx2 := strings.Index(tsStr, "-"); idx2 >= 0 {
		tsStr = tsStr[:idx2]
	}
	return fmt.Sprintf("%s (%s)", tsStr, time.Unix(parseInt64(tsStr), 0).Format(time.RFC3339))
}

func parseInt64(s string) int64 {
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int64(s[i]-'0')
		} else {
			break
		}
	}
	return n
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
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

func updateEnvFile(path, key, value string) {
	data, err := os.ReadFile(path)
	if err != nil {
		entry := fmt.Sprintf("%s=\"%s\"\n", key, value)
		os.WriteFile(path, []byte(entry), 0644)
		fmt.Printf("  [OK] Created %s in %s\n", key, path)
		return
	}

	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == key {
			lines[i] = fmt.Sprintf("%s=\"%s\"", key, value)
			found = true
			break
		}
	}

	if !found {
		lines = append(lines, fmt.Sprintf("%s=\"%s\"", key, value))
	}

	output := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(output), 0644); err != nil {
		fmt.Printf("  [WARN] Failed to write %s: %v\n", path, err)
		return
	}
	fmt.Printf("  [OK] Updated %s in %s\n", key, path)
}
