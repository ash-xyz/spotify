package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// tokenEndpoint stands in for Spotify's token endpoint. handler decides what
// each refresh does; hits counts how many actually reached it.
func tokenEndpoint(t *testing.T, handler http.HandlerFunc) (*oauth2.Config, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return &oauth2.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		Endpoint:     oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInHeader},
	}, &hits
}

// issueToken writes the response Spotify sends back for a good refresh.
func issueToken(w http.ResponseWriter, accessToken string, expiresIn int) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":%d}`, accessToken, expiresIn)
}

// A token endpoint that accepts the connection and then says nothing must not
// hold requests open indefinitely: without a bound this is what wedges every
// request behind one refresh.
func TestTokenSourceGivesUpOnAStalledEndpoint(t *testing.T) {
	release := make(chan struct{})
	cfg, _ := tokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// Registered after the server, so it runs before the server is closed:
	// closing waits on handlers, and this one waits on release.
	t.Cleanup(func() { close(release) })

	source := newTokenSource(cfg, "refresh-token", 50*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := source.Token(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Token() succeeded against a stalled endpoint, want a timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Token() never returned: the token fetch is unbounded")
	}
}

// A visitor who disconnects has to be able to stop waiting, even though the
// refresh they triggered is shared and carries on for everyone else.
func TestTokenSourceReleasesCancelledCallers(t *testing.T) {
	release := make(chan struct{})
	cfg, hits := tokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		issueToken(w, "access-token", 3600)
	})

	source := newTokenSource(cfg, "refresh-token", time.Minute)

	// The first caller starts the refresh; the second arrives behind it.
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan error, 1)
	go func() {
		_, err := source.Token(firstCtx)
		first <- err
	}()

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	second := make(chan error, 1)
	go func() {
		// Give the first caller a moment to be the one that starts the fetch.
		time.Sleep(20 * time.Millisecond)
		_, err := source.Token(secondCtx)
		second <- err
	}()

	// Both are now waiting on the same refresh. Cancelling the one behind must
	// release it without disturbing the one in front.
	time.Sleep(50 * time.Millisecond)
	cancelSecond()

	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling a caller waiting behind a refresh didn't release it")
	}

	select {
	case err := <-first:
		t.Fatalf("the remaining caller was released too, with %v: one caller leaving cancelled the shared refresh", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("Token() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refresh never completed for the caller still waiting on it")
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times, want 1", n)
	}
}

func TestTokenSourceReusesAValidToken(t *testing.T) {
	cfg, hits := tokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		issueToken(w, "access-token", 3600)
	})

	source := newTokenSource(cfg, "refresh-token", time.Minute)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := source.Token(context.Background())
			if err != nil {
				t.Errorf("Token() returned error: %v", err)
				return
			}
			if token.AccessToken != "access-token" {
				t.Errorf("AccessToken = %q, want %q", token.AccessToken, "access-token")
			}
		}()
	}
	wg.Wait()

	if n := hits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times, want 1: concurrent callers each refreshed", n)
	}
}

// A token that's already at the end of its life is refreshed again next time,
// rather than the source wedging on the first one it fetched.
func TestTokenSourceRefreshesAgainAfterExpiry(t *testing.T) {
	var issued atomic.Int64
	cfg, hits := tokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		// Inside oauth2's expiry delta, so it is never considered valid.
		issueToken(w, fmt.Sprintf("access-token-%d", issued.Add(1)), 1)
	})

	source := newTokenSource(cfg, "refresh-token", time.Minute)

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token() returned error: %v", err)
	}

	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token() returned error: %v", err)
	}

	if first.AccessToken == second.AccessToken {
		t.Errorf("both calls returned %q: an expired token was reused", first.AccessToken)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("token endpoint hit %d times, want 2", n)
	}
}

// A revoked refresh token has to reach the caller as the structured error it
// is, so that a caller can tell it apart from Spotify being down.
func TestRejectedGrantSurvivesTheClientBoundary(t *testing.T) {
	cfg, _ := tokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Refresh token revoked"}`)
	})

	spotify := &SpotifyClient{
		client: &http.Client{
			Transport: &authTransport{source: newTokenSource(cfg, "revoked", time.Minute)},
		},
		options: &Options{Limit: "5", TimeRange: ShortTerm},
	}

	// The request never leaves the machine: acquiring the token fails first.
	_, err := spotify.GetCurrentlyPlaying(context.Background())
	if err == nil {
		t.Fatal("GetCurrentlyPlaying() succeeded with a revoked refresh token")
	}

	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		t.Fatalf("error %v is not a *oauth2.RetrieveError: the reason was flattened on the way out", err)
	}
	if retrieveErr.ErrorCode != "invalid_grant" {
		t.Errorf("ErrorCode = %q, want %q", retrieveErr.ErrorCode, "invalid_grant")
	}
}

// A caller's cancellation has to come back as cancellation, not as something
// that reads like Spotify rejecting us.
func TestCancelledRequestReportsCancellation(t *testing.T) {
	release := make(chan struct{})
	cfg, _ := tokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	spotify := &SpotifyClient{
		client: &http.Client{
			Transport: &authTransport{source: newTokenSource(cfg, "refresh-token", time.Minute)},
		},
		options: &Options{Limit: "5", TimeRange: ShortTerm},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := spotify.GetCurrentlyPlaying(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}
