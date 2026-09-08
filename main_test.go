package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ash-xyz/spotify/client"
	"golang.org/x/oauth2"
)

// retrieveError is what oauth2 hands back when the token endpoint refuses,
// wrapped the way it reaches us through the client and the cache.
func retrieveError(code string) error {
	return fmt.Errorf("failed to fetch currently playing: %w", &url.Error{
		Op:  "Post",
		URL: "https://accounts.spotify.com/api/token",
		Err: &oauth2.RetrieveError{ErrorCode: code},
	})
}

func TestClassifyCredentials(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want credentialState
	}{
		{"a working call", nil, credentialsWorking},
		{"a revoked refresh grant", retrieveError("invalid_grant"), credentialsRejected},
		{
			"a revoked grant among other section failures",
			errors.Join(errors.New("failed to fetch top artists: rate limited by Spotify API"), retrieveError("invalid_grant")),
			credentialsRejected,
		},
		{"the wrong client ID or secret", retrieveError("invalid_client"), credentialsMisconfigured},
		{"an app that isn't allowed this grant", retrieveError("unauthorized_client"), credentialsMisconfigured},
		{
			"the token endpoint having a bad day",
			fmt.Errorf("request failed: %w", &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}}),
			credentialsUnknown,
		},
		{"the API rejecting the access token", fmt.Errorf("request failed: %w", &client.StatusError{StatusCode: http.StatusUnauthorized}), credentialsRejected},
		{"being rate limited", fmt.Errorf("request failed: %w", &client.StatusError{StatusCode: http.StatusTooManyRequests}), credentialsUnknown},
		{"spotify being down", fmt.Errorf("request failed: %w", &client.StatusError{StatusCode: http.StatusBadGateway}), credentialsUnknown},
		{"a visitor who left", fmt.Errorf("gave up waiting for top tracks: %w", context.Canceled), credentialsUnknown},
		{"a fetch that timed out", fmt.Errorf("request failed: %w", context.DeadlineExceeded), credentialsUnknown},
		{"the network being unreachable", errors.New("dial tcp 1.2.3.4:443: connect: network is unreachable"), credentialsUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyCredentials(test.err); got != test.want {
				t.Errorf("classifyCredentials(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

// A missing token needs authorization without asking Spotify anything.
func TestNeedsAuthWithoutAToken(t *testing.T) {
	t.Setenv("SPOTIFY_REFRESH_TOKEN", "")

	required, err := needsAuth()
	if err != nil {
		t.Fatalf("needsAuth() returned error: %v", err)
	}
	if !required {
		t.Error("needsAuth() = false with no refresh token stored, want true")
	}
}

// credentials sets synthetic values, so nothing here touches a real .env or a
// real Spotify app.
func credentials(t *testing.T) {
	t.Helper()

	t.Setenv("SPOTIFY_CLIENT_ID", "synthetic-id")
	t.Setenv("SPOTIFY_CLIENT_SECRET", "synthetic-secret")
	t.Setenv("SPOTIFY_REFRESH_TOKEN", "synthetic-token")
}

// invocation is one call the deployment made to the fly CLI.
type invocation struct {
	name  string
	args  []string
	stdin string
}

// recordCommands replaces the fly CLI with a recorder that fails whichever
// invocation the test names.
func recordCommands(t *testing.T, failing string) *[]invocation {
	t.Helper()

	var calls []invocation

	original := runCommand
	runCommand = func(name string, stdin io.Reader, args ...string) error {
		call := invocation{name: name, args: args}
		if stdin != nil {
			piped, err := io.ReadAll(stdin)
			if err != nil {
				t.Errorf("couldn't read the piped stdin: %v", err)
			}
			call.stdin = string(piped)
		}
		calls = append(calls, call)

		if failing != "" && strings.Join(args, " ") == failing {
			return errors.New("exit status 1")
		}
		return nil
	}
	t.Cleanup(func() { runCommand = original })

	return &calls
}

// Deploying after a failed secrets import would publish new code against
// whatever credentials happen to already be up there.
func TestDeployStopsWhenSecretsImportFails(t *testing.T) {
	credentials(t)
	calls := recordCommands(t, "secrets import")

	err := deploy()
	if err == nil {
		t.Fatal("deploy() succeeded after the secrets import failed, want an error")
	}

	for _, call := range *calls {
		if strings.Join(call.args, " ") == "deploy" {
			t.Fatal("deploy ran anyway, with secrets that were never imported")
		}
	}

	if strings.Contains(err.Error(), "synthetic-token") {
		t.Errorf("the error carries a credential: %v", err)
	}
}

func TestDeployImportsSecretsThenDeploysOnce(t *testing.T) {
	credentials(t)
	calls := recordCommands(t, "")

	if err := deploy(); err != nil {
		t.Fatalf("deploy() returned error: %v", err)
	}

	if len(*calls) != 2 {
		t.Fatalf("ran %d commands, want the secrets import followed by one deploy: %v", len(*calls), *calls)
	}

	imported, deployed := (*calls)[0], (*calls)[1]
	if got := strings.Join(imported.args, " "); got != "secrets import" {
		t.Errorf("first command = fly %q, want %q", got, "secrets import")
	}
	if got := strings.Join(deployed.args, " "); got != "deploy" {
		t.Errorf("second command = fly %q, want %q", got, "deploy")
	}

	// The credentials go down the pipe, not through the process list, where
	// anything else on the machine could read them.
	if !strings.Contains(imported.stdin, "SPOTIFY_REFRESH_TOKEN=synthetic-token") {
		t.Errorf("the refresh token wasn't piped to the import: %q", imported.stdin)
	}
	for _, call := range *calls {
		for _, arg := range call.args {
			if strings.Contains(arg, "synthetic-") {
				t.Errorf("a credential was passed as an argument: %q", arg)
			}
		}
	}
}

func TestDeployReportsAFailedDeployment(t *testing.T) {
	credentials(t)
	recordCommands(t, "deploy")

	if err := deploy(); err == nil {
		t.Fatal("deploy() succeeded when fly deploy failed, want an error")
	}
}

func okFetch[T any](val T) func(context.Context) (T, error) {
	return func(context.Context) (T, error) { return val, nil }
}

func failingFetch[T any](message string) func(context.Context) (T, error) {
	return func(context.Context) (T, error) {
		var zero T
		return zero, errors.New(message)
	}
}

// stubCache is a cache over canned fetches, so handler tests never need a
// Spotify client.
func stubCache(
	currentlyPlaying func(context.Context) (*client.CurrentlyPlaying, error),
	recentlyPlayed func(context.Context) (*client.RecentlyPlayedTracks, error),
	topArtists func(context.Context) (*client.TopArtists, error),
	topTracks func(context.Context) (*client.TopTracks, error),
) *spotifyCache {
	return &spotifyCache{
		currentlyPlaying: &section[*client.CurrentlyPlaying]{
			name: "currently playing", ttl: currentlyPlayingTTL, maxStale: maxStaleness, fetch: currentlyPlaying,
		},
		recentlyPlayed: &section[*client.RecentlyPlayedTracks]{
			name: "recently played", ttl: recentlyPlayedTTL, maxStale: maxStaleness, fetch: recentlyPlayed,
		},
		topArtists: &section[*client.TopArtists]{
			name: "top artists", ttl: topChartsTTL, maxStale: maxStaleness, fetch: topArtists,
		},
		topTracks: &section[*client.TopTracks]{
			name: "top tracks", ttl: topChartsTTL, maxStale: maxStaleness, fetch: topTracks,
		},
	}
}

// One endpoint failing shouldn't blank out the other three.
func TestAPIHandlerServesWhatItHas(t *testing.T) {
	cache := stubCache(
		okFetch(&client.CurrentlyPlaying{IsPlaying: true, Track: &client.Track{Name: "A Song"}}),
		failingFetch[*client.RecentlyPlayedTracks]("spotify server error: 503"),
		okFetch(&client.TopArtists{Artists: []*client.Artist{{Name: "An Artist"}}}),
		okFetch(&client.TopTracks{Tracks: []*client.Track{{Name: "A Top Song"}}}),
	)

	recorder := httptest.NewRecorder()
	apiHandler(cache)(recorder, httptest.NewRequest(http.MethodGet, "/api", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: one failing section took the whole response down", recorder.Code, http.StatusOK)
	}

	var info SpotifyInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil {
		t.Fatalf("couldn't decode the response: %v", err)
	}

	if info.RecentlyPlayed != nil {
		t.Error("recently_played isn't null, though its fetch failed")
	}
	if info.CurrentlyPlaying == nil || info.TopArtists == nil || info.TopSongs == nil {
		t.Errorf("a section that fetched fine came back null: %+v", info)
	}
}

func TestAPIHandlerReportsATotalFailure(t *testing.T) {
	cache := stubCache(
		failingFetch[*client.CurrentlyPlaying]("unauthorized: invalid or expired token"),
		failingFetch[*client.RecentlyPlayedTracks]("unauthorized: invalid or expired token"),
		failingFetch[*client.TopArtists]("unauthorized: invalid or expired token"),
		failingFetch[*client.TopTracks]("unauthorized: invalid or expired token"),
	)

	recorder := httptest.NewRecorder()
	apiHandler(cache)(recorder, httptest.NewRequest(http.MethodGet, "/api", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

// Nothing playing is a successful answer, not an error: Spotify replies 204,
// which the client turns into a nil the cache is happy to hold.
func TestNowPlayingHandlerServesNullWhenNothingIsPlaying(t *testing.T) {
	cache := stubCache(
		okFetch[*client.CurrentlyPlaying](nil),
		failingFetch[*client.RecentlyPlayedTracks]("unused"),
		failingFetch[*client.TopArtists]("unused"),
		failingFetch[*client.TopTracks]("unused"),
	)

	recorder := httptest.NewRecorder()
	nowPlayingHandler(cache)(recorder, httptest.NewRequest(http.MethodGet, "/api/now-playing", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "null" {
		t.Errorf("body = %q, want %q", got, "null")
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
	}
}

func TestNowPlayingHandlerReportsAFailure(t *testing.T) {
	cache := stubCache(
		failingFetch[*client.CurrentlyPlaying]("spotify server error: 503"),
		failingFetch[*client.RecentlyPlayedTracks]("unused"),
		failingFetch[*client.TopArtists]("unused"),
		failingFetch[*client.TopTracks]("unused"),
	)

	recorder := httptest.NewRecorder()
	nowPlayingHandler(cache)(recorder, httptest.NewRequest(http.MethodGet, "/api/now-playing", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}
