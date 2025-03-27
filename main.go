package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/ash-xyz/spotify/client"
	"github.com/joho/godotenv"
)

type SpotifyInfo struct {
	TopArtists       *client.TopArtists           `json:"top_artists"`
	TopSongs         *client.TopTracks            `json:"top_tracks"`
	CurrentlyPlaying *client.CurrentlyPlaying     `json:"currently_playing"`
	RecentlyPlayed   *client.RecentlyPlayedTracks `json:"recently_played"`
}

func getSpotifyDataAsJSON(client *client.SpotifyClient, ctx context.Context) (string, error) {
	// TODO: We should utilize caching

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
		return "", err
	}

	return string(jsonData), nil
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
	data, err := getSpotifyDataAsJSON(spotifyClient, context.Background())
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(data)

	// apiHandler(spotifyClient)

	// http.ListenAndServe(":8080", nil)
}
