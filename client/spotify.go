// Structs that I can unmarshal from Spotify API
package client

import "time"

type SpotifyTrack struct {
	Name         string            `json:"name"`
	Artists      []*SpotifyArtist  `json:"artists"`
	ExternalURLs map[string]string `json:"external_urls"`
	DurationMs   int               `json:"duration_ms"`
}

type SpotifyArtist struct {
	Name         string            `json:"name"`
	ExternalURLs map[string]string `json:"external_urls"`
}

type SpotifyCurrentlyPlaying struct {
	Progress int           `json:"progress_ms"`
	Item     *SpotifyTrack `json:"item"`
	// IsPlaying is false while paused. The track stays in the response either
	// way, so without this a paused track looks like it's still playing.
	IsPlaying bool `json:"is_playing"`
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
