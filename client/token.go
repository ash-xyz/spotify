package client

import (
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	// How long one token endpoint round trip gets, response body included.
	// The oauth2 default client has no timeout at all, so without this a
	// stalled token endpoint stalls every request behind it for as long as it
	// cares to hold the connection.
	tokenTimeout = 10 * time.Second

	// How long a call to the Spotify API gets, token acquisition included.
	requestTimeout = 10 * time.Second
)

// tokenSource hands out Spotify access tokens, refreshing at most one at a
// time.
//
// oauth2's own reuse source is nearly this, except that it holds its mutex
// across the refresh HTTP call and takes no context from the caller: a slow
// token endpoint blocks every request for as long as it is slow, and nobody
// waiting on it can give up. Here the refresh runs on its own bounded context,
// because it belongs to the process rather than to whichever visitor happened
// to trigger it, and each caller waits on it only until its own context is done.
type tokenSource struct {
	// refresh is the underlying oauth2 source, bounded by tokenTimeout. Only
	// ever called from one goroutine at a time.
	refresh oauth2.TokenSource

	mu       sync.Mutex
	token    *oauth2.Token
	inflight *tokenCall
}

// tokenCall is a single refresh, shared by everyone who arrives while it runs.
type tokenCall struct {
	done  chan struct{}
	token *oauth2.Token
	err   error
}

// newTokenSource builds a source that refreshes against cfg's token endpoint,
// giving each refresh at most timeout to finish.
func newTokenSource(cfg *oauth2.Config, refreshToken string, timeout time.Duration) *tokenSource {
	// A background context, not a caller's: the source is long lived, and
	// oauth2 would otherwise pin every future refresh to the lifetime of
	// whatever context it was built with.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: timeout})

	return &tokenSource{
		refresh: cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}),
	}
}

// Token returns a valid access token, refreshing the held one if it has
// expired. If ctx is done first it returns ctx.Err(), leaving the refresh
// running for whoever else is waiting on it.
func (s *tokenSource) Token(ctx context.Context) (*oauth2.Token, error) {
	call := s.start()

	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// start returns the refresh to wait on: the one already running, a new one, or
// an already finished one carrying the token still in hand.
func (s *tokenSource) start() *tokenCall {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token.Valid() {
		return finishedCall(s.token, nil)
	}

	if s.inflight != nil {
		return s.inflight
	}

	call := &tokenCall{done: make(chan struct{})}
	s.inflight = call

	go func() {
		token, err := s.refresh.Token()

		s.mu.Lock()
		if err == nil {
			s.token = token
		}
		s.inflight = nil
		s.mu.Unlock()

		// Safe to write without the lock: nothing reads these before the
		// channel closes.
		call.token, call.err = token, err
		close(call.done)
	}()

	return call
}

func finishedCall(token *oauth2.Token, err error) *tokenCall {
	call := &tokenCall{done: make(chan struct{}), token: token, err: err}
	close(call.done)
	return call
}

// authTransport attaches a bearer token to every request. It is oauth2's own
// transport in shape, but it acquires the token with the request's context, so
// that a caller that has given up isn't left waiting on a refresh it has no
// way to abandon.
type authTransport struct {
	source *tokenSource
	base   http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.source.Token(req.Context())
	if err != nil {
		return nil, err
	}

	// A RoundTripper mustn't modify the request it is handed.
	authorized := req.Clone(req.Context())
	token.SetAuthHeader(authorized)

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(authorized)
}
