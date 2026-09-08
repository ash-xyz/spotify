package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestRedirectURIDefaultsToLoopback(t *testing.T) {
	t.Setenv("SPOTIFY_REDIRECT_URI", "")

	if got := RedirectURI(); got != DefaultRedirectURI {
		t.Errorf("RedirectURI() = %q, want %q", got, DefaultRedirectURI)
	}
}

func TestRedirectURIHonoursOverride(t *testing.T) {
	const custom = "http://127.0.0.1:9999/spotify-callback"
	t.Setenv("SPOTIFY_REDIRECT_URI", custom)

	if got := RedirectURI(); got != custom {
		t.Errorf("RedirectURI() = %q, want %q", got, custom)
	}
}

// A redirect URI without a port can't be served locally, so Run should say so
// rather than listening on a random port and silently never being called back.
func TestRunRejectsRedirectURIWithoutPort(t *testing.T) {
	t.Setenv("SPOTIFY_REDIRECT_URI", "https://example.com/callback")

	_, err := Run(context.Background(), "id", "secret")
	if err == nil {
		t.Fatal("Run() succeeded, want an error about the missing port")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("Run() error = %v, want it to mention the missing port", err)
	}
}

func TestRunRejectsUnparseableRedirectURI(t *testing.T) {
	t.Setenv("SPOTIFY_REDIRECT_URI", "://not a url")

	if _, err := Run(context.Background(), "id", "secret"); err == nil {
		t.Fatal("Run() succeeded, want an error about the invalid redirect URI")
	}
}

func TestStateTokensAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		token, err := generateStateToken()
		if err != nil {
			t.Fatalf("generateStateToken() returned error: %v", err)
		}
		if seen[token] {
			t.Fatal("generateStateToken() repeated a value, which would weaken CSRF protection")
		}
		seen[token] = true
	}
}

// freePort picks a port the callback server can have to itself. There is a
// window between closing this listener and Run opening its own, but nothing
// else in the test is listening.
func freePort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("couldn't find a free port: %v", err)
	}
	defer listener.Close()

	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

// fakeSpotify stands in for the authorization and token endpoints, and points
// Run at itself. exchange handles the code-for-token call.
func fakeSpotify(t *testing.T, exchange http.HandlerFunc) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", exchange)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	original := authEndpoint
	authEndpoint = oauth2.Endpoint{
		AuthURL:   server.URL + "/authorize",
		TokenURL:  server.URL + "/api/token",
		AuthStyle: oauth2.AuthStyleInHeader,
	}
	t.Cleanup(func() { authEndpoint = original })
}

// visitCallback replaces opening a browser with the redirect a browser would
// eventually make, carrying the state Run generated.
func visitCallback(t *testing.T, code string) {
	t.Helper()

	original := openBrowser
	openBrowser = func(authURL string) error {
		parsed, err := url.Parse(authURL)
		if err != nil {
			return err
		}

		callback := RedirectURI() + "?code=" + url.QueryEscape(code) +
			"&state=" + url.QueryEscape(parsed.Query().Get("state"))

		// In the background: the callback handler blocks until the exchange
		// finishes, and Run hasn't started waiting for a result yet.
		go func() {
			response, err := http.Get(callback)
			if err == nil {
				response.Body.Close()
			}
		}()

		return nil
	}
	t.Cleanup(func() { openBrowser = original })
}

func TestRunReturnsTheRefreshToken(t *testing.T) {
	t.Setenv("SPOTIFY_REDIRECT_URI", "http://127.0.0.1:"+freePort(t)+"/callback")

	fakeSpotify(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`)
	})
	visitCallback(t, "auth-code")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	refreshToken, err := Run(ctx, "id", "secret")
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if refreshToken != "rt" {
		t.Errorf("Run() = %q, want %q", refreshToken, "rt")
	}
}

// Giving up on an authorization run has to give up on the token exchange too,
// rather than leaving it running against Spotify with nobody waiting for it.
func TestRunCancellationStopsTheTokenExchange(t *testing.T) {
	t.Setenv("SPOTIFY_REDIRECT_URI", "http://127.0.0.1:"+freePort(t)+"/callback")

	started := make(chan struct{})
	stopped := make(chan error, 1)
	abandon := make(chan struct{})
	fakeSpotify(t, func(w http.ResponseWriter, r *http.Request) {
		// Drained first: until the request body has been read, the server
		// doesn't watch the connection, and so never notices the client
		// abandoning the exchange.
		io.Copy(io.Discard, r.Body)
		close(started)

		select {
		case <-r.Context().Done():
			stopped <- r.Context().Err()
		case <-abandon:
		}
	})
	// Registered after the server, so a failing test tears down rather than
	// hanging in Close waiting on the handler above.
	t.Cleanup(func() { close(abandon) })
	visitCallback(t, "auth-code")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "id", "secret")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the token exchange never started")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() didn't return: an in-flight exchange kept it alive")
	}

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the token exchange carried on after the run was cancelled")
	}
}
