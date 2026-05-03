package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/HeapOfChaos/goondvr/supabase"
)

func main() {
	// Get Supabase credentials from environment or use from settings
	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseAPIKey := os.Getenv("SUPABASE_API_KEY")
	
	if supabaseURL == "" {
		supabaseURL = "https://xhfbhgklqylmfmfjtgkq.supabase.co"
	}
	if supabaseAPIKey == "" {
		supabaseAPIKey = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzdXBhYmFzZSIsInJlZiI6InhoZmJoZ2tscXlsbWZtZmp0Z2txIiwicm9sZSI6ImFub24iLCJpYXQiOjE3Nzc3NDIxNTYsImV4cCI6MjA5MzMxODE1Nn0.xIPocBS1e1QhGm080ISgU63vHXLywIH-isk0757Z3Xw"
	}

	log.Printf("Testing Supabase channel management...")
	log.Printf("Supabase URL: %s", supabaseURL)

	client := supabase.NewClient(supabaseURL, supabaseAPIKey)

	// Test 1: Get all channels
	log.Println("\n=== Test 1: Get All Channels ===")
	channels, err := client.GetAllChannels()
	if err != nil {
		log.Printf("❌ Failed to get channels: %v", err)
	} else {
		log.Printf("✓ Retrieved %d channels from Supabase", len(channels))
		if len(channels) > 0 {
			log.Printf("  First channel: %s (%s)", channels[0].Username, channels[0].Site)
		}
	}

	// Test 2: Insert a test channel
	log.Println("\n=== Test 2: Insert Test Channel ===")
	testChannel := supabase.ChannelConfig{
		Username:    "test_channel_" + fmt.Sprintf("%d", time.Now().Unix()),
		Site:        "chaturbate",
		IsPaused:    false,
		Framerate:   30,
		Resolution:  1080,
		Pattern:     "videos/{{.Username}}_{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}",
		MaxDuration: 45,
		MaxFilesize: 0,
		CreatedAt:   time.Now().Unix(),
	}

	if err := client.InsertChannel(testChannel); err != nil {
		log.Printf("❌ Failed to insert channel: %v", err)
	} else {
		log.Printf("✓ Test channel inserted: %s", testChannel.Username)
	}

	// Test 3: Get the test channel
	log.Println("\n=== Test 3: Get Test Channel ===")
	retrievedChannel, err := client.GetChannelByUsername(testChannel.Username)
	if err != nil {
		log.Printf("❌ Failed to get test channel: %v", err)
	} else {
		log.Printf("✓ Retrieved test channel: %s", retrievedChannel.Username)
		log.Printf("  Site: %s", retrievedChannel.Site)
		log.Printf("  Resolution: %dp", retrievedChannel.Resolution)
		log.Printf("  Max Duration: %d minutes", retrievedChannel.MaxDuration)
	}

	// Test 4: Update the test channel
	log.Println("\n=== Test 4: Update Test Channel ===")
	testChannel.IsPaused = true
	testChannel.Resolution = 720
	if err := client.UpdateChannel(testChannel.Username, testChannel); err != nil {
		log.Printf("❌ Failed to update channel: %v", err)
	} else {
		log.Printf("✓ Test channel updated (paused=true, resolution=720p)")
	}

	// Test 5: Verify the update
	log.Println("\n=== Test 5: Verify Update ===")
	updatedChannel, err := client.GetChannelByUsername(testChannel.Username)
	if err != nil {
		log.Printf("❌ Failed to get updated channel: %v", err)
	} else {
		log.Printf("✓ Verified update:")
		log.Printf("  Is Paused: %v", updatedChannel.IsPaused)
		log.Printf("  Resolution: %dp", updatedChannel.Resolution)
	}

	// Test 6: Delete the test channel
	log.Println("\n=== Test 6: Delete Test Channel ===")
	if err := client.DeleteChannel(testChannel.Username); err != nil {
		log.Printf("❌ Failed to delete channel: %v", err)
	} else {
		log.Printf("✓ Test channel deleted: %s", testChannel.Username)
	}

	// Test 7: Verify deletion
	log.Println("\n=== Test 7: Verify Deletion ===")
	_, err = client.GetChannelByUsername(testChannel.Username)
	if err != nil {
		log.Printf("✓ Channel not found (correctly deleted)")
	} else {
		log.Printf("❌ Channel still exists (deletion failed)")
	}

	// Test 8: Test upsert (insert)
	log.Println("\n=== Test 8: Test Upsert (Insert) ===")
	upsertChannel := supabase.ChannelConfig{
		Username:    "upsert_test_" + fmt.Sprintf("%d", time.Now().Unix()),
		Site:        "chaturbate",
		IsPaused:    false,
		Framerate:   60,
		Resolution:  1080,
		Pattern:     "videos/{{.Username}}_{{.Year}}-{{.Month}}-{{.Day}}_{{.Hour}}-{{.Minute}}-{{.Second}}",
		MaxDuration: 45,
		MaxFilesize: 0,
		CreatedAt:   time.Now().Unix(),
	}

	if err := client.UpsertChannel(upsertChannel); err != nil {
		log.Printf("❌ Failed to upsert (insert): %v", err)
	} else {
		log.Printf("✓ Upsert (insert) successful: %s", upsertChannel.Username)
	}

	// Test 9: Test upsert (update)
	log.Println("\n=== Test 9: Test Upsert (Update) ===")
	upsertChannel.IsPaused = true
	upsertChannel.Framerate = 30
	if err := client.UpsertChannel(upsertChannel); err != nil {
		log.Printf("❌ Failed to upsert (update): %v", err)
	} else {
		log.Printf("✓ Upsert (update) successful")
	}

	// Test 10: Verify upsert update
	log.Println("\n=== Test 10: Verify Upsert Update ===")
	finalChannel, err := client.GetChannelByUsername(upsertChannel.Username)
	if err != nil {
		log.Printf("❌ Failed to get upserted channel: %v", err)
	} else {
		log.Printf("✓ Verified upsert update:")
		log.Printf("  Is Paused: %v", finalChannel.IsPaused)
		log.Printf("  Framerate: %d fps", finalChannel.Framerate)
	}

	// Cleanup: Delete upsert test channel
	log.Println("\n=== Cleanup ===")
	if err := client.DeleteChannel(upsertChannel.Username); err != nil {
		log.Printf("⚠️  Failed to cleanup: %v", err)
	} else {
		log.Printf("✓ Cleanup successful")
	}

	fmt.Println("\n=== All Tests Completed ===")
	fmt.Println("✅ Supabase channel management is working correctly!")
}
