package web

import (
	"html/template"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/trips"
)

// TestPreviewMedia writes the pages that show music, filled with made-up
// songs, so they can be looked at without the car streaming:
//
//	PREVIEW_DIR=/tmp/preview go test ./internal/web -run PreviewMedia
func TestPreviewMedia(t *testing.T) {
	dir := os.Getenv("PREVIEW_DIR")
	if dir == "" {
		t.Skip("set PREVIEW_DIR to write the previews")
	}
	tmpl := template.Must(template.New("").Funcs(funcs(time.Local)).ParseFS(templateFS, "templates/*.html"))
	now := time.Now()
	playing := &Track{Title: "Blinding Lights", Artist: "The Weeknd", Album: "After Hours", Source: "Spotify", Status: "Playing", Start: now.Add(-2 * time.Minute)}
	tracks := []Track{
		{Title: "Midnight City", Artist: "M83", Source: "Spotify", Start: now.Add(-14 * time.Minute)},
		{Title: "Nightcall", Artist: "Kavinsky", Source: "Spotify", Start: now.Add(-10 * time.Minute)},
		*playing,
	}
	zt, z6 := 5200*time.Millisecond, 3100*time.Millisecond
	live := &trips.Trip{Start: now.Add(-15 * time.Minute), End: now, Duration: 15 * time.Minute, DistanceKm: 14.2, MaxSpeedKmh: 142, PeakPowerKW: 310, ZeroTo100: &zt}
	pages := map[string]pageData{
		"bests-playing.html": {Page: "race", NowPlaying: playing},
		"bests-driving.html": {Page: "race", NowPlaying: playing, Live: live},
		"car-playing.html":   {Page: "dashboard", NowPlaying: playing},
	}
	saved := *live
	saved.ZeroTo60 = &z6
	d := pageData{Page: "trip", TripTracks: tracks, TopSpeedTrack: &tracks[1]}
	d.Trip = &store.SavedTrip{ID: 1, Source: "telemetry", Trip: saved}
	pages["drive-soundtrack.html"] = d
	for name, d := range pages {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := tmpl.ExecuteTemplate(f, "index.html", d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f.Close()
	}
}
