package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

const (
	spotifyBaseURL    = "https://api.spotify.com/v1"
	currentURL        = spotifyBaseURL + "/me/player/currently-playing"
	recentlyPlayedURL = spotifyBaseURL + "/me/player/recently-played"
	topTracksURL      = spotifyBaseURL + "/me/top/tracks"
	topArtistsURL     = spotifyBaseURL + "/me/top/artists"
)

type SpotifyClient struct {
	client  *http.Client
	options *Options
}

type Options struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	Limit        string
	TimeRange    string
}

func WithClientID(clientID string) func(*Options) {
	return func(o *Options) {
		o.ClientID = clientID
	}
}

func WithClientSecret(clientSecret string) func(*Options) {
	return func(o *Options) {
		o.ClientSecret = clientSecret
	}
}

func WithRefreshToken(refreshToken string) func(*Options) {
	return func(o *Options) {
		o.RefreshToken = refreshToken
	}
}

// Determines limit of tracks, artists and recently played songs to be fetched
func WithLimit(limit string) func(*Options) {
	return func(o *Options) {
		o.Limit = limit //TODO: range is 0-50, let's do some validation on this
	}
}

func WithTimeRange(timeRange string) func(*Options) {
	return func(o *Options) {
		o.TimeRange = timeRange // TODO: this is either short_term, medium_term or long_term (Let's make this an enum/type)
	}
}

func NewSpotifyClient(opts ...func(*Options)) *SpotifyClient {
	options := &Options{
		Limit:        "5",
		TimeRange:    "short_term",
		ClientID:     os.Getenv("SPOTIFY_CLIENT_ID"),
		ClientSecret: os.Getenv("SPOTIFY_CLIENT_SECRET"),
		RefreshToken: os.Getenv("SPOTIFY_REFRESH_TOKEN"),
	}

	for _, opt := range opts {
		opt(options)
	}

	cfg := &oauth2.Config{
		ClientID:     options.ClientID,
		ClientSecret: options.ClientSecret,
		Endpoint:     spotify.Endpoint,
	}

	token := &oauth2.Token{
		RefreshToken: options.RefreshToken,
	}

	ctx := context.Background()
	client := cfg.Client(ctx, token)
	client.Timeout = 10 * time.Second

	return &SpotifyClient{
		client:  client,
		options: options,
	}
}

type Track struct {
	Name         string            `json:"name"`
	Artists      []Artist          `json:"artists"`
	ExternalURLs map[string]string `json:"external_urls"`
}

type Artist struct {
	Name         string            `json:"name"`
	ExternalURLs map[string]string `json:"external_urls"`
}

type CurrentlyPlaying struct {
	Progress int   `json:"progress_ms"`
	Item     Track `json:"item"`
}

type RecentlyPlayed struct {
	Tracks Track `json:"track"`
	// PlayedAt time.Time `json:"played_at"`
}

type RecentlyPlayedTracks struct {
	RecentlyPlayed []RecentlyPlayed `json:"items"`
}

type TopTracks struct {
	Tracks []Track `json:"items"`
}

type TopArtists struct {
	Artists []Artist `json:"items"`
}

func (s *SpotifyClient) GetCurrentlyPlaying() (*CurrentlyPlaying, error) {
	r, err := s.client.Get(currentURL)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()

	if r.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	cp := &CurrentlyPlaying{}
	err = json.NewDecoder(r.Body).Decode(cp)

	if err != nil {
		return nil, err
	}

	return cp, nil
}

func (s *SpotifyClient) GetRecentlyPlayed() (*RecentlyPlayedTracks, error) {
	rp := &RecentlyPlayedTracks{}

	params := url.Values{
		"limit": {s.options.Limit},
	}

	err := s.doRequest(recentlyPlayedURL, params, rp)
	if err != nil {
		return nil, err
	}
	return rp, nil
}

func (s *SpotifyClient) GetTopTrack() (*TopTracks, error) {
	tt := &TopTracks{}

	params := url.Values{
		"limit":      {s.options.Limit},
		"time_range": {s.options.TimeRange},
	}

	err := s.doRequest(topTracksURL, params, tt)
	if err != nil {
		return nil, err
	}
	return tt, nil
}

func (s *SpotifyClient) GetTopTracks() (*TopTracks, error) {
	tt := &TopTracks{}

	params := url.Values{
		"limit":      {s.options.Limit},
		"time_range": {s.options.TimeRange},
	}

	err := s.doRequest(topTracksURL, params, tt)
	if err != nil {
		return nil, err
	}
	return tt, nil
}

func (s *SpotifyClient) doRequest(url string, params url.Values, result interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	if params != nil {
		req.URL.RawQuery = params.Encode()
	}

	r, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	err = json.NewDecoder(r.Body).Decode(result)
	if err != nil {
		return err
	}

	return nil
}
