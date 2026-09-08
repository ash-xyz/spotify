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
	// maxStale is how long past the TTL stale data may still be served while
	// refreshes are failing. Zero means unbounded.
	maxStale time.Duration
	fetch    func(context.Context) (T, error)

	mu        sync.Mutex
	val       T
	fetchedAt time.Time
	cached    bool
	failedAt  time.Time
	lastErr   error
	// inflight is the fetch currently running, if any. Everyone who arrives
	// while it runs waits on it, so concurrent callers on a cold cache make
	// one request to Spotify between them rather than one each.
	inflight *fetchCall[T]
}

// fetchCall is a single fetch of a section, shared by every caller waiting on
// it. The value it carries is written once, before done is closed.
type fetchCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

func (s *section[T]) get(ctx context.Context) (T, error) {
	s.mu.Lock()
	val, cached, age := s.val, s.cached, time.Since(s.fetchedAt)
	s.mu.Unlock()

	if cached && age < s.ttl {
		return val, nil
	}

	// Refresh behind the caller while what we hold is still worth showing.
	// maxStale is an allowance on top of the TTL, not a total lifetime: a
	// 30 minute chart with a 10 minute allowance may be served up to 40
	// minutes old.
	if cached && (s.maxStale == 0 || age < s.ttl+s.maxStale) {
		if !s.inBackoff() {
			s.refreshInBackground()
		}
		return val, nil
	}

	// Nothing cached, or what we have is too old to pass off as current, so
	// this caller waits on Spotify.
	return s.refresh(ctx)
}

// refreshInBackground refreshes a stale section without blocking the caller.
func (s *section[T]) refreshInBackground() {
	s.mu.Lock()
	running := s.inflight != nil
	s.mu.Unlock()

	if running {
		return
	}

	go func() {
		// The request that triggered this refresh is already answered, so
		// nothing here should be waiting on that caller's context.
		if _, err := s.refresh(context.Background()); err != nil {
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

// refresh waits for a current value, sharing one fetch with any other caller
// waiting on the same section. A caller that gives up leaves the fetch running
// for the others: the work belongs to the section, not to whoever triggered it.
func (s *section[T]) refresh(ctx context.Context) (T, error) {
	call := s.startFetch()

	select {
	case <-call.done:
		return call.val, call.err
	case <-ctx.Done():
		// This caller's own doing, so it isn't a failure of the section and
		// mustn't put it into backoff for everyone else.
		var zero T
		return zero, fmt.Errorf("gave up waiting for %s: %w", s.name, ctx.Err())
	}
}

// startFetch returns the fetch to wait on: the one already running, a new one,
// or an already finished one carrying whatever made a fetch unnecessary.
func (s *section[T]) startFetch() *fetchCall[T] {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inflight != nil {
		return s.inflight
	}

	// Another caller may have refreshed while this one was on its way here.
	if s.cached && time.Since(s.fetchedAt) < s.ttl {
		return finishedFetch(s.val, nil)
	}

	if !s.failedAt.IsZero() && time.Since(s.failedAt) < failureBackoff {
		var zero T
		return finishedFetch(zero, fmt.Errorf("not retrying %s for another %s: %w",
			s.name, (failureBackoff-time.Since(s.failedAt)).Round(time.Second), s.lastErr))
	}

	call := &fetchCall[T]{done: make(chan struct{})}
	s.inflight = call

	go s.runFetch(call)

	return call
}

// runFetch does the work behind an in-flight fetch. It has its own timeout
// rather than a caller's context: the visitor who triggered it may disconnect
// at any moment, and other visitors are waiting on the same result.
func (s *section[T]) runFetch(call *fetchCall[T]) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	val, err := s.fetch(ctx)

	s.mu.Lock()
	if err != nil {
		s.failedAt, s.lastErr = time.Now(), err
		var zero T
		val = zero
	} else {
		s.val, s.fetchedAt, s.cached = val, time.Now(), true
		s.failedAt, s.lastErr = time.Time{}, nil
	}
	s.inflight = nil
	s.mu.Unlock()

	// Safe to write without the lock: nothing reads these before the channel
	// closes.
	call.val, call.err = val, err
	close(call.done)
}

func finishedFetch[T any](val T, err error) *fetchCall[T] {
	call := &fetchCall[T]{done: make(chan struct{}), val: val, err: err}
	close(call.done)
	return call
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
