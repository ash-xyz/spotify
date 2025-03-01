package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
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
	if err := godotenv.Load(); err != nil {
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
	http.HandleFunc("/callback", completeAuth(cfg, state))

	if err := http.ListenAndServe(":8888", nil); err != nil {
		fmt.Println("Error starting callback server:", err)
		return
	}
}

func completeAuth(auth *oauth2.Config, state string) func(w http.ResponseWriter, r *http.Request) {
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

		fmt.Fprintf(w, "Authentication successful! Feel free to close this window and check your console!")
		fmt.Println("==== SPOTIFY TOKENS ====")
		fmt.Println("Access Token:", token.AccessToken)
		fmt.Println("Refresh Token:", token.RefreshToken)
		fmt.Println("Token Expires:", token.Expiry.Format(time.DateTime))

		go func() {
			time.Sleep(2)
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
