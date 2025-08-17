package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ash-xyz/spotify/client"
	"github.com/ash-xyz/spotify/internal"
	chi "github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
)

type SpotifyInfo struct {
	TopArtists       *client.TopArtists           `json:"top_artists"`
	TopSongs         *client.TopTracks            `json:"top_tracks"`
	CurrentlyPlaying *client.CurrentlyPlaying     `json:"currently_playing"`
	RecentlyPlayed   *client.RecentlyPlayedTracks `json:"recently_played"`
}

var (
	cache      []byte
	cacheMutex sync.Mutex
	cacheTime  time.Time
)

func getSpotifyDataAsJSON(client *client.SpotifyClient, ctx context.Context) ([]byte, error) {
	cacheMutex.Lock()

	if cache != nil && time.Since(cacheTime) < 3*time.Minute {
		result := make([]byte, len(cache))
		copy(result, cache)
		cacheMutex.Unlock()
		return result, nil
	}

	cacheMutex.Unlock()

	var wg sync.WaitGroup

	spotifyInfo := SpotifyInfo{}
	wg.Add(4)

	errorChannel := make(chan error, 4)

	go func() {
		defer wg.Done()
		currentlyPlaying, err := client.GetCurrentlyPlaying(ctx)
		if err != nil {
			spotifyInfo.CurrentlyPlaying = nil
			log.Printf("Error fetching currently playing: %v", err)
			errorChannel <- fmt.Errorf("failed to fetch currently playing: %w", err)
		} else {
			spotifyInfo.CurrentlyPlaying = currentlyPlaying
		}
	}()

	go func() {
		defer wg.Done()
		topArtists, err := client.GetTopArtists(ctx)
		if err != nil {
			spotifyInfo.TopArtists = nil
			log.Printf("Error fetching top artists: %v", err)
			errorChannel <- fmt.Errorf("failed to fetch top artists: %w", err)
		} else {
			spotifyInfo.TopArtists = topArtists
		}
	}()

	go func() {
		defer wg.Done()
		topTracks, err := client.GetTopTracks(ctx)
		if err != nil {
			spotifyInfo.TopSongs = nil
			log.Printf("Error fetching top tracks: %v", err)
			errorChannel <- fmt.Errorf("failed to fetch top tracks: %w", err)
		} else {
			spotifyInfo.TopSongs = topTracks
		}
	}()

	go func() {
		defer wg.Done()
		recentlyPlayed, err := client.GetRecentlyPlayed(ctx)
		if err != nil {
			spotifyInfo.RecentlyPlayed = nil
			log.Printf("Error fetching recently played: %v", err)
			errorChannel <- fmt.Errorf("failed to fetch recently played: %w", err)
		} else {
			spotifyInfo.RecentlyPlayed = recentlyPlayed
		}
	}()

	wg.Wait()
	close(errorChannel)

	for err := range errorChannel {
		return nil, err
	}

	jsonData, err := json.MarshalIndent(spotifyInfo, "", "  ")
	if err != nil {
		return nil, err
	}

	cacheMutex.Lock()

	cache = make([]byte, len(jsonData))
	copy(cache, jsonData)
	cacheTime = time.Now()

	cacheMutex.Unlock()

	return jsonData, nil
}

func apiHandler(client *client.SpotifyClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := getSpotifyDataAsJSON(client, context.Background())

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Error retrieving data"))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

func assertEnvVariablesExist() error {
	requiredVars := []string{
		"SPOTIFY_CLIENT_ID",
		"SPOTIFY_CLIENT_SECRET",
		"SPOTIFY_REFRESH_TOKEN",
	}

	for _, envVar := range requiredVars {
		if os.Getenv(envVar) == "" {
			return fmt.Errorf("%s is not set", envVar)
		}
	}
	return nil
}

func checkRefreshTokenValidity(ctx context.Context) error {
	spotifyClient := client.NewSpotifyClient()
	_, err := spotifyClient.GetCurrentlyPlaying(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "unauthorized") || strings.Contains(err.Error(), "401") {
			return fmt.Errorf("refresh token is invalid or expired")
		}

		log.Printf("Warning: Error checking token validity: %v", err)
	}
	return nil
}

func runServer() error {
	if err := assertEnvVariablesExist(); err != nil {
		return fmt.Errorf("environment validation failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := checkRefreshTokenValidity(ctx); err != nil {
		log.Printf("Error: %v", err)
		log.Println("Please run 'go run auth/main.go' to get a new refresh token")
		return err
	}

	spotifyClient := client.NewSpotifyClient()
	log.Println("Spotify Client Created! ✅")

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(internal.SecurityHeaders)
	r.Use(internal.CORS([]string{"https://ash.xyz", "https://www.ash.xyz"}))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("This is a little project I'm working on 🎶☕!"))
	})

	r.Get("/api", apiHandler(spotifyClient))
	log.Println("API endpoint created! ✅")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting server on port %s", port)
	return http.ListenAndServe(":"+port, r)
}

func local() {
	if err := godotenv.Load(); err != nil {
		log.Fatalln("Error loading from .env file")
	}
	if err := runServer(); err != nil {
		log.Fatal(err)
	}
}

func setFlySecrets() error {
	secrets := map[string]string{
		"SPOTIFY_CLIENT_ID":     os.Getenv("SPOTIFY_CLIENT_ID"),
		"SPOTIFY_CLIENT_SECRET": os.Getenv("SPOTIFY_CLIENT_SECRET"),
		"SPOTIFY_REFRESH_TOKEN": os.Getenv("SPOTIFY_REFRESH_TOKEN"),
	}

	var args []string
	for key, value := range secrets {
		if value != "" {
			args = append(args, fmt.Sprintf("%s=%s", key, value))
		}
	}

	if len(args) == 0 {
		return fmt.Errorf("no secrets to set")
	}

	log.Println("Setting Fly.io secrets...")
	cmd := exec.Command("fly", append([]string{"secrets", "set"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func deploy() {
	if err := godotenv.Load(".env"); err != nil {
		log.Fatalln("Error loading from .env file - make sure .env exists with your Spotify credentials")
	}

	if err := assertEnvVariablesExist(); err != nil {
		log.Fatalf("Missing environment variables: %v", err)
	}

	if err := setFlySecrets(); err != nil {
		log.Printf("Warning: Failed to set secrets: %v", err)
		log.Println("You may need to set them manually if this is your first deployment")
	}

	log.Println("Deploying to Fly.io...")
	cmd := exec.Command("fly", "deploy")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("Fly deployment failed: %v", err)
	}
}

func runAuth(isProduction bool) error {
	cmd := exec.Command("go", "run", "auth/main.go")
	if isProduction {
		cmd.Args = append(cmd.Args, "--prod")
	} else {
		cmd.Args = append(cmd.Args, "--local")
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func needsAuth(isProduction bool) bool {
	if isProduction {
		return os.Getenv("SPOTIFY_REFRESH_TOKEN") == ""
	}

	if err := godotenv.Load(); err != nil {
		return true
	}

	refreshToken := os.Getenv("SPOTIFY_REFRESH_TOKEN")
	if refreshToken == "" {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spotifyClient := client.NewSpotifyClient()
	_, err := spotifyClient.GetCurrentlyPlaying(ctx)

	return err != nil && (strings.Contains(err.Error(), "unauthorized") || strings.Contains(err.Error(), "401"))
}

func main() {
	modeFlag := flag.String("mode", "local", "Mode to run in (deploy, local, run)")
	resetAuthFlag := flag.Bool("reset-auth", false, "Force new authentication flow")
	flag.Parse()

	switch *modeFlag {
	case "deploy":
		isProduction := true
		if *resetAuthFlag || needsAuth(isProduction) {
			log.Println("Setting up authentication for production...")
			if err := runAuth(isProduction); err != nil {
				log.Fatalf("Auth setup failed: %v", err)
			}
		}
		deploy()
	case "local":
		isProduction := false
		if *resetAuthFlag || needsAuth(isProduction) {
			log.Println("Setting up authentication for local development...")
			if err := runAuth(isProduction); err != nil {
				log.Fatalf("Auth setup failed: %v", err)
			}
		}
		local()
	case "run":
		if err := runServer(); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal("Invalid mode")
	}
}
