package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// counted returns a fetch function that records how many times it ran, so tests
// can assert on the number of calls that would have reached Spotify.
func counted(value string, calls *atomic.Int64) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		calls.Add(1)
		return value, nil
	}
}

func TestSectionCachesWithinTTL(t *testing.T) {
	var calls atomic.Int64
	s := &section[string]{name: "test", ttl: time.Minute, fetch: counted("first", &calls)}

	for range 5 {
		got, err := s.get(context.Background())
		if err != nil {
			t.Fatalf("get() returned error: %v", err)
		}
		if got != "first" {
			t.Fatalf("get() = %q, want %q", got, "first")
		}
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1: cache is not holding within its TTL", n)
	}
}

func TestSectionSingleFlightsColdFetch(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})

	s := &section[string]{
		name: "test",
		ttl:  time.Minute,
		fetch: func(context.Context) (string, error) {
			calls.Add(1)
			<-release // hold the fetch open so every caller piles up behind it
			return "first", nil
		},
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.get(context.Background()); err != nil {
				t.Errorf("get() returned error: %v", err)
			}
		}()
	}

	// Give the goroutines time to arrive before letting the fetch finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1: concurrent cold callers each hit Spotify", n)
	}
}

func TestSectionServesStaleWhileRefreshing(t *testing.T) {
	var calls atomic.Int64
	value := "first"
	var mu sync.Mutex

	s := &section[string]{
		name: "test",
		ttl:  10 * time.Millisecond,
		fetch: func(context.Context) (string, error) {
			calls.Add(1)
			mu.Lock()
			defer mu.Unlock()
			return value, nil
		},
	}

	if _, err := s.get(context.Background()); err != nil {
		t.Fatalf("initial get() returned error: %v", err)
	}

	mu.Lock()
	value = "second"
	mu.Unlock()

	time.Sleep(20 * time.Millisecond) // let the entry go stale

	// The stale read must not wait on the fetch: it returns the old value and
	// refreshes behind the caller.
	got, err := s.get(context.Background())
	if err != nil {
		t.Fatalf("stale get() returned error: %v", err)
	}
	if got != "first" {
		t.Errorf("stale get() = %q, want %q: caller waited for the refresh", got, "first")
	}

	// The background refresh should land shortly after.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := s.get(context.Background()); got == "second" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("background refresh never updated the cached value")
}

func TestSectionReturnsErrorWhenCold(t *testing.T) {
	wantErr := errors.New("unauthorized: invalid or expired token")
	s := &section[string]{
		name: "test",
		ttl:  time.Minute,
		fetch: func(context.Context) (string, error) {
			return "", wantErr
		},
	}

	if _, err := s.get(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("get() error = %v, want %v", err, wantErr)
	}
}

// warm seeds a section as though its value had been fetched age ago, which
// beats waiting out a production TTL to reach the same state.
func warm(s *section[string], val string, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.val, s.cached, s.fetchedAt = val, true, time.Now().Add(-age)
}

// chart is a section carrying the production top-charts settings: a 30 minute
// TTL with 10 minutes of staleness allowed on top of it.
func chart(fetch func(context.Context) (string, error)) *section[string] {
	return &section[string]{name: "top artists", ttl: topChartsTTL, maxStale: maxStaleness, fetch: fetch}
}

// A chart just past its TTL is still worth showing while a refresh runs: the
// allowance is time on top of the TTL, not the whole life of the value.
func TestSectionServesStaleWithinTheAllowance(t *testing.T) {
	var calls atomic.Int64
	fetched := make(chan struct{}, 1)

	s := chart(func(context.Context) (string, error) {
		calls.Add(1)
		fetched <- struct{}{}
		return "new", nil
	})
	warm(s, "old", 31*time.Minute)

	got, err := s.get(context.Background())
	if err != nil {
		t.Fatalf("get() returned error: %v", err)
	}
	if got != "old" {
		t.Errorf("get() = %q, want %q: a chart 31 minutes old should be served while it refreshes", got, "old")
	}

	select {
	case <-fetched:
	case <-time.After(2 * time.Second):
		t.Fatal("no background refresh was started")
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
}

// Past the allowance the value is too old to pass off as current, so the
// caller waits — and gets an error rather than the stale value if that fails.
func TestSectionWaitsOnceBeyondTheAllowance(t *testing.T) {
	wantErr := errors.New("spotify server error: 503")
	s := chart(func(context.Context) (string, error) { return "", wantErr })
	warm(s, "old", 40*time.Minute)

	got, err := s.get(context.Background())
	if !errors.Is(err, wantErr) {
		t.Errorf("get() error = %v, want %v", err, wantErr)
	}
	if got != "" {
		t.Errorf("get() = %q, want no value: data 40 minutes old was served as a fallback", got)
	}
}

// While a section is inside its stale window, a failing refresh must not take
// away what we already have.
func TestSectionKeepsStaleDataWhenARefreshFails(t *testing.T) {
	failed := make(chan struct{}, 1)
	s := chart(func(context.Context) (string, error) {
		failed <- struct{}{}
		return "", errors.New("spotify server error: 503")
	})
	warm(s, "old", 31*time.Minute)

	if got, err := s.get(context.Background()); err != nil || got != "old" {
		t.Fatalf("get() = %q, %v, want %q and no error", got, err, "old")
	}

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("no background refresh was started")
	}

	if got, err := s.get(context.Background()); err != nil || got != "old" {
		t.Errorf("get() = %q, %v after a failed refresh, want %q and no error", got, err, "old")
	}
}

// A zero allowance means unbounded: whatever we hold is better than nothing.
func TestSectionWithoutAnAllowanceServesAnythingItHas(t *testing.T) {
	s := &section[string]{
		name: "test", ttl: time.Minute,
		fetch: func(context.Context) (string, error) { return "new", nil },
	}
	warm(s, "old", 12*time.Hour)

	if got, err := s.get(context.Background()); err != nil || got != "old" {
		t.Errorf("get() = %q, %v, want %q and no error", got, err, "old")
	}
}

// blocking returns a fetch that waits to be released, reporting when it starts.
func blocking(started chan<- struct{}, release <-chan struct{}, calls *atomic.Int64) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		calls.Add(1)
		started <- struct{}{}

		select {
		case <-release:
			return "value", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// A visitor who disconnects during a cold fetch is not an upstream failure.
// Recording one would lock every other visitor out of the section for the
// length of the backoff.
func TestSectionCancelledCallerDoesNotBackOffTheSection(t *testing.T) {
	var calls atomic.Int64
	started, release := make(chan struct{}, 1), make(chan struct{})

	s := &section[string]{name: "test", ttl: time.Minute, fetch: blocking(started, release, &calls)}

	ctx, cancel := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() {
		_, err := s.get(ctx)
		left <- err
	}()

	<-started
	cancel()

	select {
	case err := <-left:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling a caller didn't release it: the wait isn't context aware")
	}

	if s.inBackoff() {
		t.Error("a caller leaving put the section into backoff, locking out everyone else")
	}

	// The upstream is healthy, so an unrelated visitor arriving now must be
	// served rather than told to come back in 30 seconds.
	close(release)

	got, err := s.get(context.Background())
	if err != nil {
		t.Fatalf("the next request failed after an unrelated caller left: %v", err)
	}
	if got != "value" {
		t.Errorf("get() = %q, want %q", got, "value")
	}
}

// One caller giving up mustn't take the shared fetch down with it: the others
// waiting on it still need the answer.
func TestSectionCancelledCallerLeavesSharedWorkRunning(t *testing.T) {
	var calls atomic.Int64
	started, release := make(chan struct{}, 1), make(chan struct{})

	s := &section[string]{name: "test", ttl: time.Minute, fetch: blocking(started, release, &calls)}

	staying := make(chan string, 1)
	go func() {
		val, err := s.get(context.Background())
		if err != nil {
			t.Errorf("the remaining caller failed: %v", err)
		}
		staying <- val
	}()

	<-started

	leavingCtx, cancel := context.WithCancel(context.Background())
	left := make(chan struct{})
	go func() {
		defer close(left)
		if _, err := s.get(leavingCtx); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled caller error = %v, want context.Canceled", err)
		}
	}()

	// Let the second caller attach to the fetch already running, then cancel it.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-left

	close(release)

	select {
	case val := <-staying:
		if val != "value" {
			t.Errorf("get() = %q, want %q", val, "value")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the shared fetch was abandoned when one of its callers left")
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
}

// A genuine upstream failure still has to back the section off, so an outage
// isn't met with a fresh request from every visitor.
func TestSectionBacksOffAfterAnUpstreamFailure(t *testing.T) {
	var calls atomic.Int64
	wantErr := errors.New("spotify server error: 503")

	s := &section[string]{
		name: "test", ttl: time.Minute,
		fetch: func(context.Context) (string, error) {
			calls.Add(1)
			return "", wantErr
		},
	}

	if _, err := s.get(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("get() error = %v, want %v", err, wantErr)
	}

	if !s.inBackoff() {
		t.Error("a failed fetch left the section ready to try again immediately")
	}

	if _, err := s.get(context.Background()); err == nil {
		t.Error("get() succeeded during backoff, want the failure to be reported")
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1: backoff didn't hold off the second request", n)
	}
}

// The cache warmer and a visitor arriving together on a cold start is the
// common case for this, and it should still be one request to Spotify.
func TestSectionSharesWorkBetweenWarmerAndVisitor(t *testing.T) {
	var calls atomic.Int64
	started, release := make(chan struct{}, 2), make(chan struct{})

	s := &section[string]{name: "test", ttl: time.Minute, fetch: blocking(started, release, &calls)}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.get(context.Background()); err != nil {
				t.Errorf("get() returned error: %v", err)
			}
		}()
	}

	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
}
