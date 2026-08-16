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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ash-xyz/spotify/client"
	"github.com/ash-xyz/spotify/internal"
	"github.com/ash-xyz/spotify/internal/auth"
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

// How long to wait for someone to finish authorizing in the browser.
const authTimeout = 5 * time.Minute

// How long in-flight requests get to finish once a shutdown starts.
const shutdownTimeout = 5 * time.Second

// setupHelp is what a first run on a new machine needs to know, which is when
// these values are most likely to be missing. Step 2 is the one that bites:
// Spotify matches the redirect URI as an exact string.
var setupHelp = fmt.Sprintf(`To set up:
  1. Create an app at https://developer.spotify.com/dashboard
  2. Add %s as a Redirect URI on it. This has to
     match exactly, or authorization fails with "redirect_uri: Not matching
     configuration". Override it with SPOTIFY_REDIRECT_URI if you need to.
  3. cp .env.example .env, then fill in the client ID and secret from step 1.
  4. go run . --mode local, which fetches the refresh token for you.`, auth.DefaultRedirectURI)

func assertEnvVariablesExist() error {
	requiredVars := []string{
		"SPOTIFY_CLIENT_ID",
		"SPOTIFY_CLIENT_SECRET",
		"SPOTIFY_REFRESH_TOKEN",
	}

	var missing []string
	for _, envVar := range requiredVars {
		if os.Getenv(envVar) == "" {
			missing = append(missing, envVar)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf("not set: %s\n\n%s", strings.Join(missing, ", "), setupHelp)
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
			log.Println("Please run 'go run . --mode local --reset-auth' to get a new one")
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
	r.Use(internal.CORS(allowedOrigins()))
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

	return serve(&http.Server{Handler: r}, listener)
}

// allowedOrigins is the site, plus anything listed in ALLOWED_ORIGINS. That's
// how you point a locally served copy of the site at this API, which is
// otherwise impossible without editing the allowlist.
func allowedOrigins() []string {
	origins := []string{"https://ash.xyz", "https://www.ash.xyz"}

	for _, origin := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			origins = append(origins, origin)
		}
	}

	return origins
}

// serve runs the server until it fails or the machine is asked to stop. Fly
// sends SIGINT every time it stops or suspends a machine, which with
// auto_stop_machines is a routine event rather than a rare one, so responses
// in flight are given a moment to finish instead of being cut off.
func serve(server *http.Server, listener net.Listener) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()

	select {
	case err := <-serverErr:
		return err
	case sig := <-stop:
		log.Printf("Got %s, finishing in-flight requests...", sig)

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		return server.Shutdown(ctx)
	}
}

func setFlySecrets() error {
	secrets := map[string]string{
		"SPOTIFY_CLIENT_ID":     os.Getenv("SPOTIFY_CLIENT_ID"),
		"SPOTIFY_CLIENT_SECRET": os.Getenv("SPOTIFY_CLIENT_SECRET"),
		"SPOTIFY_REFRESH_TOKEN": os.Getenv("SPOTIFY_REFRESH_TOKEN"),
	}

	var lines []string
	for key, value := range secrets {
		if value != "" {
			lines = append(lines, fmt.Sprintf("%s=%s", key, value))
		}
	}

	if len(lines) == 0 {
		return fmt.Errorf("no secrets to set")
	}

	log.Println("Setting Fly.io secrets...")
	// Piped in rather than passed as arguments, which any other process on the
	// machine could read out of the process list while this runs.
	cmd := exec.Command("fly", "secrets", "import")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func deploy() {
	if err := assertEnvVariablesExist(); err != nil {
		log.Fatal(err)
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

// loadEnvFile pulls in .env if there is one. A missing file isn't fatal by
// itself, since the values can come from the shell instead; what matters is
// whether the credentials end up set, which assertEnvVariablesExist reports
// with something actionable to say.
func loadEnvFile() {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: couldn't read .env: %v", err)
	}
}

// ensureAuth gets a refresh token if there isn't a working one already, and
// persists it. Running the flow in process rather than shelling out means the
// token is available to the rest of this run immediately.
func ensureAuth(isProduction, reset bool) error {
	if !reset && !needsAuth() {
		return nil
	}

	id, secret := os.Getenv("SPOTIFY_CLIENT_ID"), os.Getenv("SPOTIFY_CLIENT_SECRET")
	if id == "" || secret == "" {
		return fmt.Errorf("missing SPOTIFY_CLIENT_ID or SPOTIFY_CLIENT_SECRET\n\n%s", setupHelp)
	}

	if isProduction {
		log.Println("Setting up authentication for production...")
	} else {
		log.Println("Setting up authentication for local development...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	defer cancel()

	refreshToken, err := auth.Run(ctx, id, secret)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// The flow ran in this process, so the rest of this run can just use it.
	os.Setenv("SPOTIFY_REFRESH_TOKEN", refreshToken)

	if err := writeToEnvFile(refreshToken); err != nil {
		log.Printf("Failed to write to .env file: %v", err)
		fmt.Printf("Please manually add to .env: SPOTIFY_REFRESH_TOKEN=%s\n", refreshToken)
	} else {
		log.Println("Local .env file updated! ✅")
	}

	return nil
}

// needsAuth reports whether the stored refresh token is missing or rejected.
func needsAuth() bool {
	if os.Getenv("SPOTIFY_REFRESH_TOKEN") == "" {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	spotifyClient := client.NewSpotifyClient()
	_, err := spotifyClient.GetCurrentlyPlaying(ctx)

	return err != nil && (strings.Contains(err.Error(), "unauthorized") || strings.Contains(err.Error(), "401"))
}

// writeToEnvFile stores the refresh token in .env, leaving any other values in
// the file alone.
func writeToEnvFile(refreshToken string) error {
	envContent := fmt.Sprintf("SPOTIFY_REFRESH_TOKEN=%s\n", refreshToken)

	existingContent, err := os.ReadFile(".env")
	if err != nil && !os.IsNotExist(err) {
		// Carrying on here would replace a .env we couldn't read with one
		// holding only the refresh token, throwing away the client credentials.
		return fmt.Errorf("couldn't read .env to update it: %w", err)
	}

	if err == nil {
		lines := strings.Split(string(existingContent), "\n")
		var newLines []string
		found := false

		for _, line := range lines {
			if strings.HasPrefix(line, "SPOTIFY_REFRESH_TOKEN=") {
				newLines = append(newLines, fmt.Sprintf("SPOTIFY_REFRESH_TOKEN=%s", refreshToken))
				found = true
			} else {
				newLines = append(newLines, line)
			}
		}

		if !found {
			newLines = append(newLines, fmt.Sprintf("SPOTIFY_REFRESH_TOKEN=%s", refreshToken))
		}

		envContent = strings.Join(newLines, "\n")
	}

	return os.WriteFile(".env", []byte(envContent), 0600)
}

func main() {
	modeFlag := flag.String("mode", "local", "Mode to run in (deploy, local, run)")
	resetAuthFlag := flag.Bool("reset-auth", false, "Force new authentication flow")
	flag.Parse()

	switch *modeFlag {
	case "deploy":
		loadEnvFile()
		if err := ensureAuth(true, *resetAuthFlag); err != nil {
			log.Fatal(err)
		}
		deploy()
	case "local":
		loadEnvFile()
		if err := ensureAuth(false, *resetAuthFlag); err != nil {
			log.Fatal(err)
		}
		if err := runServer(); err != nil {
			log.Fatal(err)
		}
	case "run":
		if err := runServer(); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("Invalid mode %q: expected deploy, local, or run", *modeFlag)
	}
}
