package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

const (
	spotifyBaseURL    = "https://api.spotify.com/v1"
	currentlyPlaying  = spotifyBaseURL + "/me/player/currently-playing"
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
	TimeRange    TimeRange
}

type TimeRange string

const (
	TimeRangeTag           = "time_range"
	ShortTerm    TimeRange = "short_term"
	MediumTerm   TimeRange = "medium_term"
	LongTerm     TimeRange = "long_term"
)

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

func WithLimit(limit int) func(*Options) {
	return func(o *Options) {
		if limit < 1 {
			limit = 1
		} else if limit > 50 {
			limit = 50
		}
		o.Limit = fmt.Sprintf("%d", limit)
	}
}

func WithTimeRange(timeRange TimeRange) func(*Options) {
	return func(o *Options) {
		o.TimeRange = timeRange
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

func (s *SpotifyClient) GetCurrentlyPlaying(ctx context.Context) (*CurrentlyPlaying, error) {
	cp := &SpotifyCurrentlyPlaying{}
	params := url.Values{
		"limit": {s.options.Limit},
	}

	err := s.doRequest(ctx, currentlyPlaying, params, cp)
	if err != nil {
		return nil, err
	}

	if cp.Item == nil {
		return nil, nil
	}

	return cp.Convert(), nil
}

func (s *SpotifyClient) GetRecentlyPlayed(ctx context.Context) (*RecentlyPlayedTracks, error) {
	rp := &SpotifyRecentlyPlayedTracks{}

	params := url.Values{
		"limit": {s.options.Limit},
	}

	err := s.doRequest(ctx, recentlyPlayedURL, params, rp)
	if err != nil {
		return nil, err
	}
	return rp.Convert(), nil
}

func (s *SpotifyClient) GetTopArtists(ctx context.Context) (*TopArtists, error) {
	ta := &SpotifyTopArtists{}

	params := url.Values{
		"limit":      {s.options.Limit},
		TimeRangeTag: {string(s.options.TimeRange)},
	}

	err := s.doRequest(ctx, topArtistsURL, params, ta)
	if err != nil {
		return nil, err
	}
	return ta.Convert(), nil
}

func (s *SpotifyClient) GetTopTracks(ctx context.Context) (*TopTracks, error) {
	tt := &SpotifyTopTracks{}

	params := url.Values{
		"limit":      {s.options.Limit},
		TimeRangeTag: {string(s.options.TimeRange)},
	}

	err := s.doRequest(ctx, topTracksURL, params, tt)
	if err != nil {
		return nil, err
	}
	return tt.Convert(), nil
}

func (s *SpotifyClient) doRequest(ctx context.Context, url string, params url.Values, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if params != nil {
		req.URL.RawQuery = params.Encode()
	}

	r, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer r.Body.Close()

	if r.StatusCode == http.StatusNoContent {
		return nil
	}

	if r.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized: invalid or expired token")
	}

	if r.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("rate limited by Spotify API")
	}

	if r.StatusCode >= 500 {
		return fmt.Errorf("spotify server error: %d", r.StatusCode)
	}

	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", r.StatusCode)
	}

	// Limit response body size to prevent memory exhaustion
	const maxResponseSize = 10 << 20 // 10MB
	limitedReader := &io.LimitedReader{R: r.Body, N: maxResponseSize}

	err = json.NewDecoder(limitedReader).Decode(result)
	if err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	return nil
}
