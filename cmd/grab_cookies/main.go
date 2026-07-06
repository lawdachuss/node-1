package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	exitCode := 0
	defer func() { os.Exit(exitCode) }()

	loadDotEnv(".env")

	userAgent := os.Getenv("USER_AGENT")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	}

	fmt.Println("=== Cookie Grabber (Scrapling) ===")
	fmt.Println()

	// Get proxy — use PROXY_URL from env first
	proxyURL := ""
	envProxies := getProxyURLs()
	if len(envProxies) > 0 {
		proxyURL = envProxies[0]
		fmt.Printf("Proxy: %s\n", proxyURL)
	} else {
		fmt.Println("No PROXY_URL set")
		exitCode = 1
		return
	}

	// Write Python script to temp file
	tmpFile, err := os.CreateTemp("", "grab_cookies_*.py")
	if err != nil {
		fmt.Printf("[FAIL] Cannot create temp script: %v\n", err)
		exitCode = 1
		return
	}
	tmpPath := tmpFile.Name()
	tmpFile.WriteString(pythonScript)
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// Run Scrapling
	fmt.Println("Running Scrapling browser (solving Cloudflare challenge)...")
	cmd := exec.Command("python", tmpPath, proxyURL)
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if stderr.Len() > 0 {
		for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				fmt.Printf("  %s\n", line)
			}
		}
	}
	outStr := stdout.String()

	// Find JSON in output (Scrapling may print log lines before the JSON)
	jsonStart := strings.Index(outStr, "{")
	if jsonStart >= 0 {
		outStr = outStr[jsonStart:]
	} else {
		jsonStart = strings.Index(outStr, "[")
		if jsonStart >= 0 {
			// Might be an error array
			outStr = outStr[jsonStart:]
		}
	}

	var result struct {
		Success bool              `json:"success"`
		Cookies map[string]string `json:"cookies"`
		Status  int               `json:"status"`
	}
	if json.Unmarshal([]byte(outStr), &result) != nil || !result.Success {
		fmt.Printf("[FAIL] Scrapling failed\n  Raw: %s\n", strings.TrimSpace(outStr))
		exitCode = 1
		return
	}

	if !result.Success || len(result.Cookies) == 0 {
		fmt.Println("[FAIL] Scrapling returned no cookies")
		exitCode = 1
		return
	}

	fmt.Printf("Status: %d\n", result.Status)
	fmt.Printf("Got %d cookies\n", len(result.Cookies))
	if v, ok := result.Cookies["cf_clearance"]; ok {
		fmt.Printf("cf_clearance: fresh! (length: %d)\n", len(v))
	}
	if v, ok := result.Cookies["__cf_bm"]; ok {
		fmt.Printf("__cf_bm: fresh! (length: %d)\n", len(v))
	}

	// Must have cf_clearance — exit 1 if missing so workflow restores old secret
	if v, ok := result.Cookies["cf_clearance"]; !ok || v == "" {
		fmt.Println("[FAIL] No cf_clearance — preserving old cookies from secret")
		exitCode = 1
		return
	}

	saveAndExit(result.Cookies, userAgent)
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
	fmt.Printf("Total cookies: %d\n", len(cookies))
}

// getProxyURLs returns all proxy URLs from the environment.
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

const pythonScript = `"""Grab cookies from chaturbate.com using Scrapling's StealthyFetcher."""
import json,sys,os,logging
logging.getLogger().setLevel(logging.CRITICAL)
from scrapling.fetchers import StealthyFetcher
proxy=sys.argv[1] if len(sys.argv)>1 else None
try:
 resp=StealthyFetcher.fetch("https://chaturbate.com",headless=True,network_idle=True,solve_cloudflare=True,timeout=90000,proxy=proxy,load_dom=True)
 cookies={}
 if isinstance(resp.cookies,tuple):
  for c in resp.cookies:
   if isinstance(c,dict)and"name"in c:
    cookies[c["name"]]=c["value"]
   elif isinstance(c,dict):
    cookies.update(c)
 elif isinstance(resp.cookies,dict):
  cookies=dict(resp.cookies)
 print(json.dumps({"success":True,"cookies":cookies,"status":resp.status}))
except Exception as e:
 print(json.dumps({"success":False,"error":str(e)}))
 sys.exit(1)
`
