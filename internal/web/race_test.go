package web

import (
	"testing"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
)

func TestPlaylist(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 11, 3, 20, 0, s, 0, time.UTC) }
	str := func(s string) *string { return &s }
	ts := playlist([]store.MediaEvent{
		{Time: at(0), Title: str("One"), Source: str("Spotify"), Status: str("MediaStatusPlaying")},
		{Time: at(1), Artist: str("A")},                      // arrives a moment after the title
		{Time: at(200), Title: str("Two"), Artist: str("B")}, // next song
		{Time: at(300), Status: str("MediaStatusPaused")},
		{Time: at(400), Title: str("")}, // cleared
	})
	if len(ts) != 2 {
		t.Fatalf("got %d tracks, want 2: %+v", len(ts), ts)
	}
	if ts[0].Line() != "One · A" || ts[0].Source != "Spotify" || ts[0].Status != "Playing" {
		t.Errorf("first track %+v", ts[0])
	}
	if ts[1].Line() != "Two · B" || ts[1].Status != "Paused" || ts[1].Source != "Spotify" {
		t.Errorf("second track %+v", ts[1])
	}
}
