package web

import (
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
	for i, d := range pages {
		for _, page := range []string{"dashboard", "controls", "schedules", "settings", "trips", "trip"} {
			d.Page = page
			var sb strings.Builder
			if err := tmpl.ExecuteTemplate(&sb, "index.html", d); err != nil {
				t.Fatalf("data %d page %s: %v", i, page, err)
			}
		}
		for _, name := range []string{"departures", "rules", "assistant-log", "telemetry-card", "live-drive"} {
			var sb strings.Builder
			if err := tmpl.ExecuteTemplate(&sb, name, d); err != nil {
				t.Fatalf("data %d partial %s: %v", i, name, err)
			}
		}
	}
}
