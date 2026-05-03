package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

// Test file extension validation logic
func main() {
	log.Println("=== Testing File Extension Validation ===")
	
	testCases := []struct {
		filename string
		expected bool // true = should upload, false = should reject
	}{
		{"video.mp4", true},
		{"video.mkv", true},
		{"video.ts", false},
		{"video.flv", false},
		{"video.avi", false},
		{"recording.MP4", true}, // case-insensitive
		{"stream.MKV", true},
	}
	
	for _, tc := range testCases {
		ext := strings.ToLower(filepath.Ext(tc.filename))
		shouldUpload := (ext == ".mp4" || ext == ".mkv")
		
		status := "✓"
		if shouldUpload != tc.expected {
			status = "✗"
		}
		
		action := "UPLOAD"
		if !shouldUpload {
			action = "REJECT"
		}
		
		log.Printf("%s %s -> %s (expected: %v, got: %v)", 
			status, tc.filename, action, tc.expected, shouldUpload)
	}
	
	fmt.Println("\n=== Testing API Key Status Logging ===")
	
	// Simulate API key status
	apiKeys := map[string]string{
		"TurboViPlay": "xizpCCPcnb",
		"VOE.sx":      "AF1YD2ExCqHrCSUjnwp9fPabywVRU1hwgFf8aKBzkx9cS7ZAuWcN1",
		"Streamtape":  "", // Not configured
	}
	
	log.Println("🔑 API key status:")
	log.Println("  • GoFile: always enabled (no key required)")
	
	for host, key := range apiKeys {
		if key != "" {
			// Show only first 10 chars
			preview := key
			if len(key) > 10 {
				preview = key[:10] + "..."
			}
			log.Printf("  • %s: ✓ configured (key: %s)", host, preview)
		} else {
			log.Printf("  • %s: ✗ not configured", host)
		}
	}
	
	fmt.Println("\n=== Test Completed ===")
}
