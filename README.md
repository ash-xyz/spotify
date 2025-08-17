# Simple Spotify API

A simple Go web service that fetches and caches your personal Spotify data (top artists, tracks, currently playing, recently played), mostly used to relay spotify data for my website.

## Prerequisites

- Go 1.24+
- [Spotify Developer App](https://developer.spotify.com/dashboard) with these scopes:
  - `user-read-currently-playing`
  - `user-top-read`
  - `user-read-recently-played`

## Quick Start

```bash
# Clone and setup
...
# Create .env with your Spotify app credentials
echo "SPOTIFY_CLIENT_ID=your_client_id" > .env
echo "SPOTIFY_CLIENT_SECRET=your_client_secret" >> .env

# Run (automatically handles Spotify OAuth)
go run main.go --mode local
```

## API

**GET /api** - Returns your Spotify data (cached for 3 minutes)

```json
{
  "top_artists": {"artists": [{"name": "Artist Name", "spotify_url": "..."}]},
  "top_tracks": {"tracks": [{"name": "Track Name", "artists": [...]}]},
  "currently_playing": {"track": {...}, "progress_ms": 12345},
  "recently_played": {"tracks": [...]}
}
```

## Deployment

```bash
# Deploy to Fly.io (automatically handles auth + secrets)
go run main.go --mode deploy
```

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `SPOTIFY_CLIENT_ID` | Your Spotify app client ID | ✅ |
| `SPOTIFY_CLIENT_SECRET` | Your Spotify app client secret | ✅ |
| `SPOTIFY_REFRESH_TOKEN` | Auto-generated during auth flow | Auto |

## Commands

- `go run main.go --mode local` - Run locally
- `go run main.go --mode deploy` - Deploy to production
- `go run main.go --mode local --reset-auth` - Force re-authentication
