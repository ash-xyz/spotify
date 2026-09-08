// Package auth runs the Spotify authorization code flow and hands back a
// refresh token. It deliberately doesn't decide where that token is stored:
// the caller knows whether this is a local run or a deploy.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/pkg/browser"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

// DefaultRedirectURI is where Spotify sends the user back to. Spotify requires
// loopback redirects to use the explicit IP 127.0.0.1 — the "localhost"
// hostname is rejected as insecure — and this must match a Redirect URI
// registered on the app in the Spotify dashboard character for character.
const DefaultRedirectURI = "http://127.0.0.1:8888/callback"

var scopes = []string{
	"user-read-currently-playing",
	"user-top-read",
	"user-read-recently-played",
}

const (
	// How long the callback's code-for-token exchange gets. Spotify holding
	// the connection open shouldn't be able to keep the callback server, and
	// so the whole authorization run, alive indefinitely.
	exchangeTimeout = 30 * time.Second

	// How long the callback server gets to finish serving the page that says
	// authorization worked, once there's nothing left to wait for.
	shutdownGrace = 5 * time.Second
)

// Seams for tests, which can neither open a browser nor reach Spotify.
var (
	openBrowser  = browser.OpenURL
	authEndpoint = spotify.Endpoint
)

// RedirectURI returns the callback to use, overridable for anyone whose
// dashboard has something other than the default registered.
func RedirectURI() string {
	if uri := os.Getenv("SPOTIFY_REDIRECT_URI"); uri != "" {
		return uri
	}
	return DefaultRedirectURI
}

// result carries the outcome of the callback back to Run.
type result struct {
	refreshToken string
	err          error
}

// Run opens a browser for the user to authorize the app, serves the callback it
// redirects to, and returns the resulting refresh token. It blocks until the
// user finishes or ctx is done.
func Run(ctx context.Context, clientID, clientSecret string) (string, error) {
	redirect, err := url.Parse(RedirectURI())
	if err != nil {
		return "", fmt.Errorf("invalid redirect URI %q: %w", RedirectURI(), err)
	}

	port := redirect.Port()
	if port == "" {
		return "", fmt.Errorf("redirect URI %q needs a port, since the callback is served locally", redirect)
	}

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirect.String(),
		Scopes:       scopes,
		Endpoint:     authEndpoint,
	}

	// Everything this run starts hangs off here, so that returning — whether
	// the user finished, gave up, or ctx expired — also ends any token
	// exchange still in flight rather than leaving it to run on unattended.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	state, err := generateStateToken()
	if err != nil {
		return "", fmt.Errorf("failed to generate state (csrf) token: %w", err)
	}

	// Bound to the redirect's own host rather than every interface. The
	// default is loopback, so on a shared network nobody else can reach the
	// callback during the window it's open.
	address := net.JoinHostPort(redirect.Hostname(), port)

	// Listen before opening the browser so that a quick authorization can't
	// redirect back before anything is there to answer it.
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return "", fmt.Errorf("failed to listen on %s for the callback: %w", address, err)
	}
	defer listener.Close()

	results := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(redirect.Path, completeAuth(ctx, cfg, state, results))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go server.Serve(listener)

	// Bounded, because Shutdown waits on in-flight requests: an unbounded one
	// would wait out a callback that never finishes.
	defer func() {
		// Cancelled first, since a callback still waiting on the token
		// exchange is exactly what Shutdown would otherwise sit behind.
		cancel()

		shutdownCtx, stop := context.WithTimeout(context.Background(), shutdownGrace)
		defer stop()
		server.Shutdown(shutdownCtx)
	}()

	authURL := cfg.AuthCodeURL(state, oauth2.SetAuthURLParam("show_dialog", "true"))
	if err := openBrowser(authURL); err != nil {
		// Not fatal: the user can follow the link themselves, which is the only
		// option on a machine without a browser to open.
		fmt.Println("Couldn't open a browser automatically. Visit this URL to authorize:")
		fmt.Println(authURL)
	}

	select {
	case r := <-results:
		return r.refreshToken, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("gave up waiting for authorization: %w", ctx.Err())
	}
}

// completeAuth serves the callback. runCtx is the authorization run's own
// context: the exchange is bound to it so that giving up on the run gives up
// on the exchange too.
func completeAuth(runCtx context.Context, cfg *oauth2.Config, state string, results chan<- result) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// finish reports the outcome to Run exactly once. The buffered channel
		// means a duplicate callback can't block a handler forever.
		finish := func(err error) {
			select {
			case results <- result{err: err}:
			default:
			}
		}

		if r.FormValue("state") != state {
			http.Error(w, "State token mismatch", http.StatusBadRequest)
			finish(errors.New("state token mismatch, so the callback wasn't from the request we started"))
			return
		}

		if denied := r.FormValue("error"); denied != "" {
			http.Error(w, denied, http.StatusBadRequest)
			finish(fmt.Errorf("spotify refused authorization: %s", denied))
			return
		}

		code := r.FormValue("code")
		if code == "" {
			http.Error(w, "Couldn't get code required for token exchange", http.StatusBadRequest)
			finish(errors.New("callback carried no authorization code"))
			return
		}

		ctx, cancel := context.WithTimeout(runCtx, exchangeTimeout)
		defer cancel()

		token, err := cfg.Exchange(ctx, code)
		if err != nil {
			http.Error(w, "Failed to exchange code for token", http.StatusInternalServerError)
			finish(fmt.Errorf("failed to exchange code for token: %w", err))
			return
		}

		if token.RefreshToken == "" {
			http.Error(w, "Spotify returned no refresh token", http.StatusInternalServerError)
			finish(errors.New("spotify returned no refresh token"))
			return
		}

		fmt.Fprint(w, "Authentication successful! Feel free to close this window.")

		select {
		case results <- result{refreshToken: token.RefreshToken}:
		default:
		}
	}
}

func generateStateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
