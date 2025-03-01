// Structs that I can unmarshal from Spotify API
package client

import "time"

type SpotifyTrack struct {
	Name         string            `json:"name"`
	Artists      []*SpotifyArtist  `json:"artists"`
	ExternalURLs map[string]string `json:"external_urls"`
}

type SpotifyArtist struct {
	Name         string            `json:"name"`
	ExternalURLs map[string]string `json:"external_urls"`
}

type SpotifyCurrentlyPlaying struct {
	Progress int           `json:"progress_ms"`
	Item     *SpotifyTrack `json:"item"`
}

type SpotifyRecentlyPlayed struct {
	Track    SpotifyTrack `json:"track"`
	PlayedAt time.Time    `json:"played_at"`
}

type SpotifyRecentlyPlayedTracks struct {
	RecentlyPlayed []*SpotifyRecentlyPlayed `json:"items"`
}

type SpotifyTopTracks struct {
	Tracks []*SpotifyTrack `json:"items"`
}

type SpotifyTopArtists struct {
	Artists []*SpotifyArtist `json:"items"`
}
