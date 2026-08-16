package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
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

func apiHandler(cache *spotifyCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := cache.all(r.Context())
		if err != nil {
			log.Printf("Error retrieving data: %v", err)
			http.Error(w, "Error retrieving data", http.StatusInternalServerError)
			return
		}

		// Capped at the shortest section TTL, since the response carries
		// currently playing alongside much slower moving data.
		writeJSON(w, info, fmt.Sprintf("public, max-age=%d", int(currentlyPlayingTTL.Seconds())))
	}
}

// nowPlayingHandler serves just the currently playing track, so a page polling
// for live playback doesn't refetch the top charts every few seconds.
func nowPlayingHandler(cache *spotifyCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		currentlyPlaying, err := cache.currentlyPlaying.get(r.Context())
		if err != nil {
			log.Printf("Error retrieving currently playing: %v", err)
			http.Error(w, "Error retrieving data", http.StatusInternalServerError)
			return
		}

		// This endpoint exists to be polled tightly, so let every poll through
		// rather than having the browser damp it to the cache lifetime. The
		// server answers from its own cache in well under a millisecond.
		writeJSON(w, currentlyPlaying, "no-cache")
	}
}

// writeJSON writes v as JSON under the given Cache-Control policy.
//
// Deliberately no stale-while-revalidate: a poller would then render the
// previous response while revalidating, putting the page a full poll interval
// behind.
func writeJSON(w http.ResponseWriter, v any, cacheControl string) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
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

// warmCache primes every section so the first visitor after a cold start is
// served from cache instead of waiting on Spotify. It doubles as the refresh
// token check, since a bad token surfaces here as an unauthorized error.
func warmCache(cache *spotifyCache) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	if _, err := cache.all(ctx); err != nil {
		log.Printf("Warning: failed to warm cache: %v", err)
		if strings.Contains(err.Error(), "unauthorized") || strings.Contains(err.Error(), "401") {
			log.Println("Refresh token is invalid or expired")
			log.Println("Please run 'go run . --mode local --reset-auth' to get a new refresh token")
		}
		return
	}

	log.Println("Cache warmed! ✅")
}

func runServer() error {
	if err := assertEnvVariablesExist(); err != nil {
		return fmt.Errorf("environment validation failed: %w", err)
	}

	spotifyClient := client.NewSpotifyClient()
	cache := newSpotifyCache(spotifyClient)
	log.Println("Spotify Client Created! ✅")

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(internal.SecurityHeaders)
	r.Use(internal.CORS([]string{"https://ash.xyz", "https://www.ash.xyz"}))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("This is a little project I'm working on 🎶☕!"))
	})

	r.Get("/api", apiHandler(cache))
	r.Get("/api/now-playing", nowPlayingHandler(cache))
	log.Println("API endpoints created! ✅")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Open the socket before touching Spotify. Fly holds the visitor's
	// connection until this port accepts, so any network call made first is
	// added directly to the cold start they see.
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return fmt.Errorf("failed to listen on port %s: %w", port, err)
	}
	log.Printf("Starting server on port %s", port)

	go warmCache(cache)

	return http.Serve(listener, r)
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
	// Both modes run from a dev machine, where .env holds the token; in prod
	// mode a missing .env is fine, since the values may come from the shell.
	if err := godotenv.Load(); err != nil && !isProduction {
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
