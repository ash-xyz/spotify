package client

import "testing"

func TestCurrentlyPlayingCarriesIsPlaying(t *testing.T) {
	// A paused track is still returned by Spotify, so this flag is the only
	// thing separating "listening to" from "stopped listening to".
	paused := &SpotifyCurrentlyPlaying{
		Progress:  1234,
		IsPlaying: false,
		Item:      &SpotifyTrack{Name: "Song", DurationMs: 200000},
	}

	got := paused.Convert()
	if got == nil {
		t.Fatal("Convert() = nil, want a value")
	}
	if got.IsPlaying {
		t.Error("Convert().IsPlaying = true for a paused track")
	}
	if got.Progress != 1234 {
		t.Errorf("Convert().Progress = %d, want 1234", got.Progress)
	}
	if got.Track.DurationMs != 200000 {
		t.Errorf("Convert().Track.DurationMs = %d, want 200000: progress is meaningless without it",
			got.Track.DurationMs)
	}
}

func TestConvertHandlesNilReceivers(t *testing.T) {
	// Spotify omits these entirely rather than sending empty objects, so every
	// conversion has to survive a nil.
	var (
		nilCurrent  *SpotifyCurrentlyPlaying
		nilTopTrack *SpotifyTopTracks
		nilArtists  *SpotifyTopArtists
		nilRecent   *SpotifyRecentlyPlayedTracks
	)

	if got := nilCurrent.Convert(); got != nil {
		t.Errorf("nil SpotifyCurrentlyPlaying.Convert() = %v, want nil", got)
	}
	if got := nilTopTrack.Convert(); got != nil {
		t.Errorf("nil SpotifyTopTracks.Convert() = %v, want nil", got)
	}
	if got := nilArtists.Convert(); got != nil {
		t.Errorf("nil SpotifyTopArtists.Convert() = %v, want nil", got)
	}
	if got := nilRecent.Convert(); got != nil {
		t.Errorf("nil SpotifyRecentlyPlayedTracks.Convert() = %v, want nil", got)
	}
}

func TestConvertTracksReadsSpotifyURL(t *testing.T) {
	top := &SpotifyTopTracks{
		Tracks: []*SpotifyTrack{
			{
				Name:         "Song",
				ExternalURLs: map[string]string{"spotify": "https://open.spotify.com/track/1"},
				Artists: []*SpotifyArtist{
					{Name: "Artist", ExternalURLs: map[string]string{"spotify": "https://open.spotify.com/artist/1"}},
				},
			},
		},
	}

	got := top.Convert()
	if len(got.Tracks) != 1 {
		t.Fatalf("Convert() returned %d tracks, want 1", len(got.Tracks))
	}

	track := got.Tracks[0]
	if track.SpotifyUrl == nil || *track.SpotifyUrl != "https://open.spotify.com/track/1" {
		t.Errorf("track.SpotifyUrl = %v, want the track URL", track.SpotifyUrl)
	}
	if len(track.Artists) != 1 || track.Artists[0].Name != "Artist" {
		t.Errorf("track.Artists = %v, want one named Artist", track.Artists)
	}
}

// A track with no external_urls shouldn't panic; the URL is just empty.
func TestConvertTrackWithoutExternalURLs(t *testing.T) {
	track := (&SpotifyTrack{Name: "Song"}).convert()

	if track == nil {
		t.Fatal("convert() = nil, want a track")
	}
	if track.SpotifyUrl == nil || *track.SpotifyUrl != "" {
		t.Errorf("track.SpotifyUrl = %v, want a pointer to the empty string", track.SpotifyUrl)
	}
}
