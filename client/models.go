// Simplifies Spotify API responses for consumption
package client

type Track struct {
	Name       string    `json:"name"`
	Artists    []*Artist `json:"artists"`
	SpotifyUrl *string   `json:"spotify_url"`
}

type Artist struct {
	Name       string  `json:"name"`
	SpotifyUrl *string `json:"spotify_url"`
}

type CurrentlyPlaying struct {
	Progress int    `json:"progress_ms"`
	Track    *Track `json:"track"`
}

type RecentlyPlayedTracks struct {
	RecentlyPlayed []*Track `json:"tracks"`
}

type TopTracks struct {
	Tracks []*Track `json:"tracks"`
}

type TopArtists struct {
	Artists []*Artist `json:"artists"`
}

func (t *SpotifyTrack) SpotifyUrl() string {
	if t == nil {
		return ""
	}
	return t.ExternalURLs["spotify"]
}

func (a *SpotifyArtist) SpotifyUrl() string {
	if a == nil {
		return ""
	}
	return a.ExternalURLs["spotify"]
}

func (r *SpotifyRecentlyPlayedTracks) Convert() *RecentlyPlayedTracks {
	if r == nil || r.RecentlyPlayed == nil {
		return nil
	}
	tracks := make([]*Track, 0, len(r.RecentlyPlayed))
	for _, item := range r.RecentlyPlayed {
		tracks = append(tracks, item.Track.convert())
	}
	return &RecentlyPlayedTracks{
		RecentlyPlayed: tracks,
	}
}

func (c *SpotifyCurrentlyPlaying) Convert() *CurrentlyPlaying {
	if c == nil {
		return nil
	}
	return &CurrentlyPlaying{
		Progress: c.Progress,
		Track:    c.Item.convert(),
	}
}

func (t *SpotifyTopTracks) Convert() *TopTracks {
	if t == nil {
		return nil
	}
	tracks := make([]*Track, 0, len(t.Tracks))
	for _, track := range t.Tracks {
		tracks = append(tracks, track.convert())
	}
	return &TopTracks{
		Tracks: tracks,
	}
}

func (a *SpotifyTopArtists) Convert() *TopArtists {
	if a == nil {
		return nil
	}
	artists := make([]*Artist, 0, len(a.Artists))
	for _, artist := range a.Artists {
		artists = append(artists, artist.convert())
	}
	return &TopArtists{
		Artists: artists,
	}
}

func (r *SpotifyRecentlyPlayed) Convert() *Track {
	if r == nil {
		return nil
	}
	return r.Track.convert()
}

func convertArtists(artists []*SpotifyArtist) []*Artist {
	if artists == nil {
		return nil
	}
	converted := make([]*Artist, 0, len(artists))
	for _, artist := range artists {
		converted = append(converted, artist.convert())
	}
	return converted
}

func convertTracks(tracks []*SpotifyTrack) []*Track {
	if tracks == nil {
		return nil
	}
	converted := make([]*Track, 0, len(tracks))
	for _, track := range tracks {
		converted = append(converted, track.convert())
	}
	return converted
}

func (s *SpotifyTrack) convert() *Track {
	if s == nil {
		return nil
	}
	url := s.SpotifyUrl()
	return &Track{
		Name:       s.Name,
		Artists:    convertArtists(s.Artists),
		SpotifyUrl: &url,
	}
}

func (a *SpotifyArtist) convert() *Artist {
	if a == nil {
		return nil
	}
	url := a.SpotifyUrl()
	return &Artist{
		Name:       a.Name,
		SpotifyUrl: &url,
	}
}
