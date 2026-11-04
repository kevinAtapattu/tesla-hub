package web

import (
	"strings"
	"testing"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/trips"
)

func TestSectors(t *testing.T) {
	// A straight line north at a steady 1 km per minute, then twice as fast.
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var pts []circuitPoint
	at := t0
	for i := 0; i <= 30; i++ {
		kmh := 60.0
		if i > 20 {
			kmh = 120
		}
		pts = append(pts, circuitPoint{Lat: 43 + float64(i)*0.009, Lon: -79, Kmh: kmh, T: at})
		at = at.Add(time.Duration(3600/kmh) * time.Second) // ~1 km per step
	}
	s := sectors(pts)
	if len(s) != 3 {
		t.Fatalf("got %d sectors", len(s))
	}
	if s[2].Class != "purple" || s[2].TopKmh != 120 {
		t.Errorf("last sector should be quickest: %+v", s[2])
	}
	for _, x := range s {
		if x.Km < 8.5 || x.Km > 11.5 || x.Time == 0 {
			t.Errorf("sector %d: %.1f km in %s", x.N, x.Km, x.Time)
		}
	}
	if svg := string(circuitSVG(pts, s)); !strings.Contains(svg, "SECTOR 3") || !strings.Contains(svg, "120 km/h") {
		t.Error("circuit is missing its labels or top-speed marker")
	}
	if sectors(pts[:2]) != nil {
		t.Error("too few points should give no sectors")
	}
}

func TestTripBoardRatings(t *testing.T) {
	var all []store.SavedTrip
	for i := 1; i <= 10; i++ {
		zt := time.Duration(12-i) * time.Second
		all = append(all, store.SavedTrip{ID: int64(i), Source: "telemetry", Trip: trips.Trip{
			MaxSpeedKmh: float64(90 + i*5), MaxAccelG: float64(i) / 20, MaxBrakeG: .3, MaxLatG: .3, PeakPowerKW: 100, ZeroTo100: &zt}})
	}
	b := tripBoard(&all[9], all, nil, "/trips/") // the fastest drive
	if b.Number != 10 || b.Ratings[0].Score != 5 {
		t.Errorf("number %d, launch score %d", b.Number, b.Ratings[0].Score)
	}
	if b.Ranks[0].Pos != 1 || b.Ranks[0].Of != 10 || b.Ranks[1].Label != "0–100" || b.Ranks[1].Pos != 1 {
		t.Errorf("ranks %+v", b.Ranks[:2])
	}
	if b = tripBoard(&all[0], all, nil, "/trips/"); b.Ranks[0].Pos != 10 || b.Ratings[0].Score != 1 {
		t.Errorf("slowest: pos %d, launch score %d", b.Ranks[0].Pos, b.Ratings[0].Score)
	}
}

func TestSectionSentence(t *testing.T) {
	zt := 5400 * time.Millisecond
	got := sectionSentence(&store.Section{Trip: &trips.Trip{DistanceKm: 6.34, Duration: 8 * time.Minute, MaxSpeedKmh: 112, ZeroTo100: &zt, MaxLatG: .61}})
	if want := "Saved: 6.3 km in 8 minutes, top speed 112 km/h, 0 to 100 in 5.4 seconds, 0.61 g cornering."; got != want {
		t.Errorf("got %q", got)
	}
	if got := sectionSentence(&store.Section{}); got == "" {
		t.Error("empty section needs a reply")
	}
}
