package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ash-xyz/spotify/client"
	"github.com/joho/godotenv"
)

type SpotifyInfo struct {
	TopArtists       client.TopArtists           `json:"top_artists"`
	TopSongs         client.TopTracks            `json:"top_tracks"`
	CurrentlyPlaying client.CurrentlyPlaying     `json:"currently_playing"`
	RecentlyPlayed   client.RecentlyPlayedTracks `json:"recently_played"`
}

func getSpotifyDataAsJSON(client *client.SpotifyClient, ctx context.Context) (string, error) {
	// TODO: We should utilize caching
	// TODO: We should use concurrency to fetch all data simultaneously
	_, err := client.GetCurrentlyPlaying(ctx)
	if err != nil {
		return "", err
	}

	_, err = client.GetTopArtists(ctx)
	if err != nil {
		return "", err
	}

	_, err = client.GetTopTracks(ctx)
	if err != nil {
		return "", err
	}

	_, err = client.GetRecentlyPlayed(ctx)
	if err != nil {
		return "", err
	}

	return "", nil
}

func apiHandler(client *client.SpotifyClient) {
	http.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		// data, err := getSpotifyDataAsJSON(client)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("TODO"))
	})
}

func main() {
	// TODO: reconsider this for deployment
	if err := godotenv.Load(); err != nil {
		fmt.Println("Error loading from .env file")
		return
	}

	spotifyClient := client.NewSpotifyClient()
	data, err := spotifyClient.GetCurrentlyPlaying(context.Background())
	if err != nil {
		fmt.Println(err)
		return
	}

	js, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(string(js))

	// apiHandler(spotifyClient)

	// http.ListenAndServe(":8080", nil)
}
