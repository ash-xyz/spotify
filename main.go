package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ash-xyz/spotify/client"
	"github.com/ash-xyz/spotify/internal"
	"github.com/ash-xyz/spotify/internal/auth"
	chi "github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
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
// served from cache instead of waiting on Spotify. It doubles as the credential
// check, since credentials Spotify won't accept surface here first.
func warmCache(cache *spotifyCache) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	if _, err := cache.all(ctx); err != nil {
		log.Printf("Warning: failed to warm cache: %v", err)

		switch classifyCredentials(err) {
		case credentialsRejected:
			log.Println("Spotify has rejected the refresh token, so it can't be renewed")
			log.Println("Please run 'go run . --mode local --reset-auth' to get a new one")
		case credentialsMisconfigured:
			log.Println("Spotify rejected the client ID and secret, so check them against the app in the dashboard")
		}
		return
	}

	log.Println("Cache warmed! ✅")
}

// credentialState is what a call to Spotify was able to tell us about the
// credentials in hand.
type credentialState int

const (
	// credentialsWorking: Spotify accepted them.
	credentialsWorking credentialState = iota
	// credentialsRejected: the refresh grant is gone — revoked, or the app's
	// secret was rotated out from under it. Only a new authorization fixes it.
	credentialsRejected
	// credentialsMisconfigured: the client ID or secret is wrong, which no
	// amount of reauthorizing will fix.
	credentialsMisconfigured
	// credentialsUnknown: the call didn't get far enough to say. Spotify being
	// unreachable, rate limiting us, or the caller giving up says nothing about
	// whether the credentials are any good.
	credentialsUnknown
)

// classifyCredentials reads an error from a Spotify call for what it says about
// the stored credentials. It works on the structure of the error rather than
// its text: the token endpoint reports a revoked grant as invalid_grant, which
// no amount of looking for "unauthorized" or "401" in a message will find.
func classifyCredentials(err error) credentialState {
	if err == nil {
		return credentialsWorking
	}

	// Nothing was actually learned about the credentials if we stopped waiting.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return credentialsUnknown
	}

	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		switch retrieveErr.ErrorCode {
		case "invalid_grant":
			return credentialsRejected
		case "invalid_client", "unauthorized_client":
			return credentialsMisconfigured
		default:
			// A 5xx or an unrecognised body from the token endpoint is Spotify
			// having a bad day, not a verdict on the refresh token.
			return credentialsUnknown
		}
	}

	// The API itself rejecting a freshly minted access token means the
	// authorization behind it is no longer good.
	var statusErr *client.StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusUnauthorized {
		return credentialsRejected
	}

	return credentialsUnknown
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

	return serve(newServer(r), listener)
}

// newServer applies timeouts, because Go's defaults are "wait forever": a
// handful of connections that dribble out a request can otherwise hold
// goroutines and file descriptors open indefinitely, and keep the machine
// awake while they do it. WriteTimeout has to clear fetchTimeout, so that a
// request arriving on a cold cache still gets a response.
func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      fetchTimeout + 20*time.Second,
		IdleTimeout:       60 * time.Second,
	}
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

// runCommand is a seam for testing the deployment sequence without a fly CLI
// to run, and without real credentials to hand it.
var runCommand = func(name string, stdin io.Reader, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func setFlySecrets() error {
	// Ordered, so that what is piped in doesn't depend on map iteration.
	names := []string{"SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET", "SPOTIFY_REFRESH_TOKEN"}

	var lines []string
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			lines = append(lines, fmt.Sprintf("%s=%s", name, value))
		}
	}

	if len(lines) == 0 {
		return fmt.Errorf("no secrets to set")
	}

	log.Println("Setting Fly.io secrets...")
	// Piped in rather than passed as arguments, which any other process on the
	// machine could read out of the process list while this runs.
	stdin := strings.NewReader(strings.Join(lines, "\n") + "\n")

	return runCommand("fly", stdin, "secrets", "import")
}

// deploy pushes the local credentials up as Fly secrets and then deploys.
//
// The order matters, and so does stopping: deploying after a failed secrets
// import publishes new code against whatever secrets happen to be up there
// already — the previous account's, or none at all.
func deploy() error {
	if err := assertEnvVariablesExist(); err != nil {
		return err
	}

	if err := setFlySecrets(); err != nil {
		return fmt.Errorf("failed to set Fly.io secrets, so nothing was deployed: %w", err)
	}

	log.Println("Deploying to Fly.io...")
	if err := runCommand("fly", nil, "deploy"); err != nil {
		return fmt.Errorf("fly deployment failed: %w", err)
	}

	return nil
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
	if !reset {
		required, err := needsAuth()
		if err != nil {
			return err
		}
		if !required {
			return nil
		}
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

// needsAuth reports whether the stored refresh token is missing or has been
// rejected, and returns an error for a problem authorizing won't solve.
func needsAuth() (bool, error) {
	if os.Getenv("SPOTIFY_REFRESH_TOKEN") == "" {
		return true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	spotifyClient := client.NewSpotifyClient()
	_, err := spotifyClient.GetCurrentlyPlaying(ctx)

	switch classifyCredentials(err) {
	case credentialsRejected:
		return true, nil
	case credentialsMisconfigured:
		return false, fmt.Errorf("spotify rejected the client ID and secret: %w\n\n%s", err, setupHelp)
	case credentialsUnknown:
		// Spotify being unreachable is no reason to throw away a refresh token
		// that is probably fine, or to sit waiting on a browser login.
		log.Printf("Couldn't check the stored refresh token, carrying on with it: %v", err)
		return false, nil
	}

	return false, nil
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

	if err == nil && len(existingContent) > 0 {
		// The trailing newline is taken off and put back, so that a replacement
		// lands on the last line rather than after the empty one it leaves.
		lines := strings.Split(strings.TrimSuffix(string(existingContent), "\n"), "\n")
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

		envContent = strings.Join(newLines, "\n") + "\n"
	}

	return replaceFile(".env", []byte(envContent))
}

// renameFile is a seam for testing what a failed replacement leaves behind.
var renameFile = os.Rename

// replaceFile writes data to path as a private file, atomically.
//
// os.WriteFile would be shorter, but its permissions only apply to a file it
// creates: a .env copied from the template stays 0644, readable by anything
// else on the machine. It also truncates the file it is replacing before
// writing, so a failure halfway leaves neither the old credentials nor the new
// ones. Writing alongside and renaming over gives every reader either the old
// file or the new one.
func replaceFile(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("couldn't create a temporary file next to %s: %w", path, err)
	}
	// Named now, because the file is gone from under this name once renamed.
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	// CreateTemp is already 0600, but say so rather than depending on it: this
	// file holds a refresh token from the moment it is written.
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't restrict permissions on %s: %w", tmpName, err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("couldn't write %s: %w", tmpName, err)
	}

	// Checked, not deferred: a write can fail on close, and renaming a
	// truncated file over the credentials would lose them.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("couldn't finish writing %s: %w", tmpName, err)
	}

	if err := renameFile(tmpName, path); err != nil {
		return fmt.Errorf("couldn't replace %s: %w", path, err)
	}

	return nil
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
		if err := deploy(); err != nil {
			log.Fatal(err)
		}
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
