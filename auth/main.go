package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pkg/browser"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

const redirectURI = "http://localhost:8888/callback"

var scopes = []string{
	"user-read-currently-playing",
	"user-top-read",
	"user-read-recently-played",
}

func main() {
	var args = os.Args[1:]
	isProduction := slices.Contains(args, "--prod")

	if err := godotenv.Load(); err != nil && !isProduction {
		fmt.Println("Error loading from .env file")
		return
	}

	id := os.Getenv("SPOTIFY_CLIENT_ID")
	secret := os.Getenv("SPOTIFY_CLIENT_SECRET")

	if id == "" || secret == "" {
		log.Fatal("Missing SPOTIFY_CLIENT_ID or SPOTIFY_CLIENT_SECRET")
	}

	cfg := &oauth2.Config{
		ClientID:     id,
		ClientSecret: secret,
		RedirectURL:  redirectURI,
		Scopes:       scopes,
		Endpoint:     spotify.Endpoint,
	}

	state, err := generateStateToken()
	if err != nil {
		log.Fatal("Failed to generate state(csrf) token", err)
	}
	authUrl := cfg.AuthCodeURL(state, oauth2.SetAuthURLParam("show_dialog", "true"))

	if err := browser.OpenURL(authUrl); err != nil {
		fmt.Println("Error opening browser to authenticate user:", err)
		return
	}

	// Handle Callback
	http.HandleFunc("/callback", completeAuth(cfg, state, isProduction))

	if err := http.ListenAndServe(":8888", nil); err != nil {
		fmt.Println("Error starting callback server:", err)
		return
	}
}

func completeAuth(auth *oauth2.Config, state string, isProduction bool) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("state") != state {
			http.Error(w, "State token mismatch", http.StatusBadRequest)
			log.Println("State token mismatch")
			return
		}

		if error := r.FormValue("error"); error != "" {
			http.Error(w, error, http.StatusBadRequest)
			log.Println("Error:", error)
			return
		}

		code := r.FormValue("code")
		if code == "" {
			http.Error(w, "Couldn't get code required for token exchange", http.StatusBadRequest)
			return
		}

		// Exchange code for refresh token
		token, err := auth.Exchange(context.Background(), code)
		if err != nil {
			fmt.Println("Error exchanging code for token:", err)
			return
		}

		fmt.Fprintf(w, "Authentication successful! Feel free to close this window.")

		if isProduction {
			if err := setFlySecrets(token.RefreshToken); err != nil {
				log.Printf("Failed to set Fly secrets: %v", err)
				fmt.Printf("Please manually set: fly secrets set SPOTIFY_REFRESH_TOKEN=%s\n", token.RefreshToken)
			} else {
				fmt.Println("✅ Production secrets updated successfully!")
			}
		} else {
			if err := writeToEnvFile(token.RefreshToken); err != nil {
				log.Printf("Failed to write to .env file: %v", err)
				fmt.Printf("Please manually add to .env: SPOTIFY_REFRESH_TOKEN=%s\n", token.RefreshToken)
			} else {
				fmt.Println("✅ Local .env file updated successfully!")
			}
		}

		go func() {
			time.Sleep(2 * time.Second)
			os.Exit(0)
		}()
	}
}

func generateStateToken() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

func writeToEnvFile(refreshToken string) error {
	envContent := fmt.Sprintf("SPOTIFY_REFRESH_TOKEN=%s\n", refreshToken)

	if existingContent, err := os.ReadFile(".env"); err == nil {
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

	return os.WriteFile(".env", []byte(envContent), 0644)
}

func setFlySecrets(refreshToken string) error {
	cmd := exec.Command("fly", "secrets", "set", fmt.Sprintf("SPOTIFY_REFRESH_TOKEN=%s", refreshToken))
	return cmd.Run()
}
