package auth

import (
	"context"
	"strings"
	"testing"
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
