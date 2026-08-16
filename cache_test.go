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
