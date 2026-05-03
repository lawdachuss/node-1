package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/HeapOfChaos/goondvr/uploader"
)

func main() {
	// Get ImgBB API key from environment or use the one from settings
	apiKey := os.Getenv("IMGBB_API_KEY")
	if apiKey == "" {
		apiKey = "9f48b991edad5d980312c5f187c7ba7f" // From your settings.json
	}

	log.Printf("Testing ImgBB thumbnail upload with API key: %s...", apiKey[:10]+"...")

	// Create a test thumbnail
	testVideoPath := "test_video.mp4"
	testThumbnailPath := "test_thumbnail.jpg"

	// Create a dummy video file for testing (1 second black video)
	log.Println("Creating test video...")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=black:s=1280x720:d=1", "-c:v", "libx264", "-t", "1", testVideoPath)
	if err := cmd.Run(); err != nil {
		log.Fatalf("Failed to create test video: %v", err)
	}
	defer os.Remove(testVideoPath)

	// Generate thumbnail from test video
	log.Println("Generating thumbnail from test video...")
	cmd = exec.Command("ffmpeg", "-y", "-i", testVideoPath, "-vframes", "1", "-vf", "scale=640:-2", "-q:v", "2", testThumbnailPath)
	if err := cmd.Run(); err != nil {
		log.Fatalf("Failed to generate thumbnail: %v", err)
	}
	defer os.Remove(testThumbnailPath)

	// Verify thumbnail was created
	fileInfo, err := os.Stat(testThumbnailPath)
	if err != nil {
		log.Fatalf("Thumbnail file not found: %v", err)
	}
	log.Printf("Thumbnail created: %s (%.2f KB)", testThumbnailPath, float64(fileInfo.Size())/1024)

	// Test ImgBB upload
	log.Println("\n=== Testing ImgBB Upload ===")
	thumbnailUploader := uploader.NewThumbnailUploader(apiKey)
	uploadedURL, err := thumbnailUploader.Upload(testThumbnailPath)
	if err != nil {
		log.Fatalf("❌ ImgBB upload failed: %v", err)
	}

	log.Printf("✅ ImgBB upload successful!")
	log.Printf("📸 Thumbnail URL: %s", uploadedURL)
	fmt.Println("\n=== Test Completed Successfully ===")
	fmt.Printf("Thumbnail uploaded to: %s\n", uploadedURL)
	fmt.Println("\nYou can open this URL in your browser to verify the thumbnail is accessible.")
}
