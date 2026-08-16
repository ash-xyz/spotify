# Simple Spotify API

A simple Go web service that fetches and caches your personal Spotify data (top artists, tracks, currently playing, recently played), mostly used to relay spotify data for my website.

## Prerequisites

- Go 1.24+
- A [Spotify app](https://developer.spotify.com/dashboard)
- [flyctl](https://fly.io/docs/flyctl/install/) and `fly auth login`, for deploying only

## Quick Start

Create a Spotify app, then **add `http://127.0.0.1:8888/callback` to it as a
Redirect URI**. Spotify compares this as an exact string, so anything else —
`localhost` instead of `127.0.0.1`, a stray slash — fails the login with
`redirect_uri: Not matching configuration`.

```bash
git clone https://github.com/ash-xyz/spotify.git
cd spotify

cp .env.example .env
# Fill in SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET from your app

# Opens a browser to authorize, saves the refresh token to .env, and serves
# on http://localhost:8080
go run . --mode local
```

The app requests these scopes, which you approve in that browser step:
`user-read-currently-playing`, `user-top-read`, `user-read-recently-played`.

## API

**GET /api** - Returns all your Spotify data

```json
{
  "top_artists": {"artists": [{"name": "Artist Name", "spotify_url": "..."}]},
  "top_tracks": {"tracks": [{"name": "Track Name", "artists": [...]}]},
  "currently_playing": {"track": {...}, "progress_ms": 12345},
  "recently_played": {"tracks": [...]}
}
```

**GET /api/now-playing** - Just the current track, for polling live playback
without refetching everything else. `null` when nothing is playing.

```json
{"track": {"name": "Track Name", "artists": [...]}, "progress_ms": 12345}
```

Each kind of data is cached for as long as it stays useful — 5s for currently
playing, 1 minute for recently played, 30 minutes for the top charts. Once warm,
stale data is served immediately and refreshed in the background, so requests
don't wait on Spotify.

## Deployment

Pushing to `main` deploys via GitHub Actions, once the build, vet, tests and
`docker build` all pass. That needs a `FLY_API_TOKEN` repository secret:

```bash
fly tokens create deploy -x 8760h | gh secret set FLY_API_TOKEN --repo ash-xyz/spotify
```

To deploy from your own machine instead — which also pushes your local
credentials up as Fly secrets, and is how a first deploy gets bootstrapped:

```bash
go run . --mode deploy
```

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `SPOTIFY_CLIENT_ID` | Your Spotify app client ID | ✅ |
| `SPOTIFY_CLIENT_SECRET` | Your Spotify app client secret | ✅ |
| `SPOTIFY_REFRESH_TOKEN` | Fetched by the auth flow and written to `.env` | Auto |
| `SPOTIFY_REDIRECT_URI` | Overrides the callback, if your app registers a different one | Optional |
| `PORT` | Port to serve on (default `8080`) | Optional |

## Commands

- `go run . --mode local` - Run locally, authorizing first if needed
- `go run . --mode run` - Run the server only, reading config from the environment
- `go run . --mode deploy` - Deploy to production
- `go run . --mode local --reset-auth` - Force re-authentication
