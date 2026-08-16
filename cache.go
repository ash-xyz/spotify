package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ash-xyz/spotify/client"
)

// How long each kind of data stays fresh. Currently playing changes every few
// minutes, top artists and tracks barely change from day to day, so caching
// them together at one TTL means either stale playback or needless API calls.
const (
	currentlyPlayingTTL = 5 * time.Second
	recentlyPlayedTTL   = time.Minute
	topChartsTTL        = 30 * time.Minute

	// How far past its TTL a section may be served while refreshing. Past this
	// the data is too old to pass off as current — if Spotify has been failing
	// for this long, an error beats showing an hour old track as live.
	maxStaleness = 10 * time.Minute

	fetchTimeout = 10 * time.Second

	// How long to leave a failing section alone before trying it again, so a
	// Spotify outage or a 429 isn't met with a retry on every single request.
	failureBackoff = 30 * time.Second
)

// sectionCount is how many sections make up a full response.
const sectionCount = 4

// section caches the result of one Spotify endpoint. Once warm it never makes a
// caller wait: stale data is served immediately and refreshed in the background.
type section[T any] struct {
	name string
	ttl  time.Duration
	// maxStale bounds how long stale data may be served when refreshes are
	// failing. Zero means unbounded.
	maxStale time.Duration
	fetch    func(context.Context) (T, error)

	mu         sync.Mutex
	val        T
	fetchedAt  time.Time
	cached     bool
	refreshing bool
	failedAt   time.Time
	lastErr    error

	// Held for the duration of a fetch so that concurrent callers on a cold
	// cache make one request to Spotify between them rather than one each.
	fetchMu sync.Mutex
}

func (s *section[T]) get(ctx context.Context) (T, error) {
	s.mu.Lock()
	val, cached, age := s.val, s.cached, time.Since(s.fetchedAt)
	s.mu.Unlock()

	if cached && age < s.ttl {
		return val, nil
	}

	// Refresh behind the caller while what we hold is still worth showing.
	if cached && (s.maxStale == 0 || age < s.maxStale) {
		if !s.inBackoff() {
			s.refreshInBackground()
		}
		return val, nil
	}

	// Nothing cached, or what we have is too old to pass off as current, so
	// this caller waits on Spotify.
	return s.refresh(ctx)
}

// refreshInBackground refreshes a stale section without blocking the caller. At
// most one refresh runs at a time, so a burst of requests can't pile up
// goroutines all fetching the same thing.
func (s *section[T]) refreshInBackground() {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.refreshing = false
			s.mu.Unlock()
		}()

		// The request that triggered this refresh is already answered, so use a
		// fresh context rather than one that's about to be cancelled.
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		if _, err := s.refresh(ctx); err != nil {
			log.Printf("Background refresh of %s failed: %v", s.name, err)
		}
	}()
}

// inBackoff reports whether this section failed recently enough that trying
// again would just add load to something already struggling.
func (s *section[T]) inBackoff() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return !s.failedAt.IsZero() && time.Since(s.failedAt) < failureBackoff
}

func (s *section[T]) refresh(ctx context.Context) (T, error) {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	// Another caller may have refreshed while we waited for the lock.
	s.mu.Lock()
	val, cached, age := s.val, s.cached, time.Since(s.fetchedAt)
	failedAt, lastErr := s.failedAt, s.lastErr
	s.mu.Unlock()

	if cached && age < s.ttl {
		return val, nil
	}

	if !failedAt.IsZero() && time.Since(failedAt) < failureBackoff {
		var zero T
		return zero, fmt.Errorf("not retrying %s for another %s: %w",
			s.name, (failureBackoff - time.Since(failedAt)).Round(time.Second), lastErr)
	}

	val, err := s.fetch(ctx)
	if err != nil {
		s.mu.Lock()
		s.failedAt, s.lastErr = time.Now(), err
		s.mu.Unlock()

		var zero T
		return zero, err
	}

	s.mu.Lock()
	s.val, s.fetchedAt, s.cached = val, time.Now(), true
	s.failedAt, s.lastErr = time.Time{}, nil
	s.mu.Unlock()

	return val, nil
}

// spotifyCache fronts every endpoint the API exposes.
type spotifyCache struct {
	currentlyPlaying *section[*client.CurrentlyPlaying]
	recentlyPlayed   *section[*client.RecentlyPlayedTracks]
	topArtists       *section[*client.TopArtists]
	topTracks        *section[*client.TopTracks]
}

func newSpotifyCache(c *client.SpotifyClient) *spotifyCache {
	return &spotifyCache{
		currentlyPlaying: &section[*client.CurrentlyPlaying]{
			name: "currently playing", ttl: currentlyPlayingTTL, maxStale: maxStaleness, fetch: c.GetCurrentlyPlaying,
		},
		recentlyPlayed: &section[*client.RecentlyPlayedTracks]{
			name: "recently played", ttl: recentlyPlayedTTL, maxStale: maxStaleness, fetch: c.GetRecentlyPlayed,
		},
		topArtists: &section[*client.TopArtists]{
			name: "top artists", ttl: topChartsTTL, maxStale: maxStaleness, fetch: c.GetTopArtists,
		},
		topTracks: &section[*client.TopTracks]{
			name: "top tracks", ttl: topChartsTTL, maxStale: maxStaleness, fetch: c.GetTopTracks,
		},
	}
}

// all gathers every section in parallel. Warm sections return immediately, so
// in practice this waits only on whatever has gone stale.
func (c *spotifyCache) all(ctx context.Context) (*SpotifyInfo, error) {
	var (
		wg           sync.WaitGroup
		info         SpotifyInfo
		errorChannel = make(chan error, sectionCount)
	)

	wg.Add(sectionCount)

	go func() {
		defer wg.Done()
		currentlyPlaying, err := c.currentlyPlaying.get(ctx)
		if err != nil {
			errorChannel <- fmt.Errorf("failed to fetch currently playing: %w", err)
			return
		}
		info.CurrentlyPlaying = currentlyPlaying
	}()

	go func() {
		defer wg.Done()
		recentlyPlayed, err := c.recentlyPlayed.get(ctx)
		if err != nil {
			errorChannel <- fmt.Errorf("failed to fetch recently played: %w", err)
			return
		}
		info.RecentlyPlayed = recentlyPlayed
	}()

	go func() {
		defer wg.Done()
		topArtists, err := c.topArtists.get(ctx)
		if err != nil {
			errorChannel <- fmt.Errorf("failed to fetch top artists: %w", err)
			return
		}
		info.TopArtists = topArtists
	}()

	go func() {
		defer wg.Done()
		topTracks, err := c.topTracks.get(ctx)
		if err != nil {
			errorChannel <- fmt.Errorf("failed to fetch top tracks: %w", err)
			return
		}
		info.TopSongs = topTracks
	}()

	wg.Wait()
	close(errorChannel)

	var errs []error
	for err := range errorChannel {
		errs = append(errs, err)
		log.Printf("Error building response: %v", err)
	}

	// Serve whatever came back. One endpoint failing shouldn't blank out the
	// other three, and the response models every section as nullable already.
	// Only a total failure is worth reporting as one, since that means
	// something systemic like a dead token rather than one flaky endpoint.
	if len(errs) == sectionCount {
		return nil, errors.Join(errs...)
	}

	return &info, nil
}
