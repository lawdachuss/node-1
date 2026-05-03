package main

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"os"

	"github.com/HeapOfChaos/goondvr/uploader"
)

func main() {
	// Get ImgBB API key from environment or use the one from settings
	apiKey := os.Getenv("IMGBB_API_KEY")
	if apiKey == "" {
		apiKey = "9f48b991edad5d980312c5f187c7ba7f" // From your settings.json
	}

	log.Printf("Testing ImgBB thumbnail upload with API key: %s...", apiKey[:10]+"...")

	// Create a simple test image (640x360 black image)
	testThumbnailPath := "test_thumbnail.jpg"
	
	log.Println("Creating test thumbnail image...")
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	
	// Fill with a gradient for visual verification
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			c := color.RGBA{
				R: uint8(x * 255 / 640),
				G: uint8(y * 255 / 360),
				B: 128,
				A: 255,
			}
			img.Set(x, y, c)
		}
	}
	
	// Save as JPEG
	file, err := os.Create(testThumbnailPath)
	if err != nil {
		log.Fatalf("Failed to create thumbnail file: %v", err)
	}
	
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 85}); err != nil {
		file.Close()
		log.Fatalf("Failed to encode JPEG: %v", err)
	}
	file.Close()
	defer os.Remove(testThumbnailPath)

	// Verify thumbnail was created
	fileInfo, err := os.Stat(testThumbnailPath)
	if err != nil {
		log.Fatalf("Thumbnail file not found: %v", err)
	}
	log.Printf("✓ Thumbnail created: %s (%.2f KB)", testThumbnailPath, float64(fileInfo.Size())/1024)

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
	fmt.Println("The image should show a colorful gradient (red-green-blue).")
}
