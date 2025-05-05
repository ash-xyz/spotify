package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/ash-xyz/spotify/client"
	chi "github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
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
		cacheMutex.Unlock()
		return cache, nil
	}

	cacheMutex.Unlock()

	var wg sync.WaitGroup

	spotifyInfo := SpotifyInfo{}
	wg.Add(4)

	go func() {
		defer wg.Done()
		currentlyPlaying, err := client.GetCurrentlyPlaying(ctx)
		if err != nil {
			spotifyInfo.CurrentlyPlaying = nil
			fmt.Println(err) //TODO: Handle error properly
		} else {
			spotifyInfo.CurrentlyPlaying = currentlyPlaying
		}
	}()

	go func() {
		defer wg.Done()
		topArtists, err := client.GetTopArtists(ctx)
		if err != nil {
			spotifyInfo.TopArtists = nil
			fmt.Println(err) //TODO: Handle error properly
		} else {
			spotifyInfo.TopArtists = topArtists
		}
	}()

	go func() {
		defer wg.Done()
		topTracks, err := client.GetTopTracks(ctx)
		if err != nil {
			spotifyInfo.TopSongs = nil
			fmt.Println(err) //TODO: Handle error properly
		} else {
			spotifyInfo.TopSongs = topTracks
		}
	}()

	go func() {
		defer wg.Done()
		recentlyPlayed, err := client.GetRecentlyPlayed(ctx)
		if err != nil {
			spotifyInfo.RecentlyPlayed = nil
			fmt.Println(err) //TODO: Handle error properly
		} else {
			spotifyInfo.RecentlyPlayed = recentlyPlayed
		}
	}()

	wg.Wait()

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
		}

		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

func getCorsHandler() func(next http.Handler) http.Handler {
	return cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:8080"}, // TODO: Create a development mode for this
		AllowedMethods:   []string{"GET"},
		AllowCredentials: false,
	})
}

func main() {
	// TODO: reconsider this for deployment
	if err := godotenv.Load(); err != nil {
		fmt.Println("Error loading from .env file")
		return
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(getCorsHandler())
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello chi ☕!"))
	})

	log.Println("Creating Spotify Client 🔨")
	spotifyClient := client.NewSpotifyClient()
	log.Println("Spotify Client Created! ✅")

	r.Get("/api", apiHandler(spotifyClient))

	http.ListenAndServe(":8080", r)
}
