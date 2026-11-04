package web

import (
	"github.com/kevin/tesla-hub/internal/alerts"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/kevin/tesla-hub/internal/poller"
	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/tesla"
	"github.com/kevin/tesla-hub/internal/trips"
)

func TestTemplatesRender(t *testing.T) {
	tmpl := template.Must(template.New("").Funcs(funcs(time.UTC)).ParseFS(templateFS, "templates/*.html"))
	f, b, n := 43.65, true, 80
	pages := []pageData{
		{}, // fresh install: nothing set up
		{
			Connected: true, SetupDone: true, Domain: "me.github.io", Budget: 4000,
			Status: poller.Status{VIN: "V", Name: "Model 3", State: "online", LastCheck: time.Now()},
			Latest: &tesla.Snapshot{Time: time.Now(), BatteryLevel: &n, RangeKm: &f, Latitude: &f, Longitude: &f, ClimateOn: &b, Locked: &b,
				ChargingState: "Charging", ChargeLimit: &n, Heading: &n},
			TelemetryOn: true, TelemetryHost: "car.example.ts.net", TelemetryPort: 8443, TelemetryNote: "Sent.",
			Usage: map[string]int{"data": 12},
			Rules: []*store.Rule{{ID: 1, Name: "Work", Enabled: true, Days: "1,2,3,4,5", AtTime: "07:30", TempC: 21, Latitude: &f, Longitude: &f, RadiusM: &f}},
			Runs:  []store.RuleRun{{RuleName: "Work", RanAt: time.Now(), Status: "done"}},
		},
	}
	zt := 4800 * time.Millisecond
	trip := trips.Trip{Start: time.Now().Add(-time.Hour), End: time.Now(), DistanceKm: 12.3, MaxSpeedKmh: 104, ZeroTo100: &zt,
		SocStart: &f, SocEnd: &f, EnergyKWh: 2, Path: []trips.PathPoint{{Lat: 43.6, Lon: -79.8, Speed: 50}}}
	pages[1].Trips = []store.SavedTrip{{ID: 1, Source: "telemetry", Driver: "Kevin", Trip: trip}, {ID: 2, Source: "polling", Trip: trip}}
	pages[1].Drivers, pages[1].DriverFilter = []string{"Kevin", "Sam"}, "Kevin"
	pages[1].TripTotals = totals(pages[1].Trips)
	pages[1].Trip = &store.SavedTrip{ID: 1, Source: "telemetry", Trip: trip}
	pages[1].TripPath = template.JS(`[{"lat":43.6,"lon":-79.8,"s":50}]`)
	pages[1].Live = &trip
	pages[1].Race = raceBoard(pages[1].Trips, "")
	pages[1].NowPlaying = &Track{Title: "Song", Artist: "Artist", Source: "Spotify", Status: "Playing"}
	pages[1].TripTracks = []Track{*pages[1].NowPlaying, {Station: "CBC Radio"}}
	pages[1].TopSpeedTrack = pages[1].NowPlaying
	lat, lon, kmh := 43.6, -79.8, 0.0
	var samples []trips.Sample
	for i := range 40 {
		la, lo, v := lat+float64(i)*0.001, lon+float64(i%7)*0.001, kmh+float64(i)*3
		samples = append(samples, trips.Sample{Time: trip.Start.Add(time.Duration(i) * time.Second), Latitude: &la, Longitude: &lo, SpeedKmh: &v})
	}
	pages[1].Board = tripBoard(pages[1].Trip, pages[1].Trips, circuitPoints(nil, samples), "/trips/")
	pages[1].TripSections = []store.Section{{ID: 1, Name: "Twisties", Start: trip.Start, End: trip.End, Trip: &trip}, {ID: 2, Start: trip.End}}
	pages[1].Recording = &Recording{ID: 2, Start: time.Now().Add(-90 * time.Second)}
	pages[1].AlertsOn, pages[1].AlertsNote = true, "Test sent."
	pages[1].Alerts = alerts.Settings{Mode: alerts.Next, LastAt: time.Now()}
	for i, d := range pages {
		for _, page := range []string{"race", "dashboard", "controls", "schedules", "settings", "trips", "trip"} {
			d.Page = page
			var sb strings.Builder
			if err := tmpl.ExecuteTemplate(&sb, "index.html", d); err != nil {
				t.Fatalf("data %d page %s: %v", i, page, err)
			}
		}
		if i == 1 { // the same trip page, shown as a section
			sec := d
			sec.Page, sec.Section, sec.ParentTrip = "trip", &pages[1].TripSections[1], pages[1].Trip
			var sb strings.Builder
			if err := tmpl.ExecuteTemplate(&sb, "index.html", sec); err != nil {
				t.Fatalf("section page: %v", err)
			}
		}
		for _, name := range []string{"departures", "rules", "assistant-log", "telemetry-card", "alerts-card", "live-drive", "trip-list"} {
			var sb strings.Builder
			if err := tmpl.ExecuteTemplate(&sb, name, d); err != nil {
				t.Fatalf("data %d partial %s: %v", i, name, err)
			}
		}
	}
}
