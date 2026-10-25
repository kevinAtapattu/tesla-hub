package web

import (
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/kevin/tesla-hub/internal/poller"
	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/tesla"
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
			Usage: map[string]int{"data": 12},
			Rules: []*store.Rule{{ID: 1, Name: "Work", Enabled: true, Days: "1,2,3,4,5", AtTime: "07:30", TempC: 21, Latitude: &f, Longitude: &f, RadiusM: &f}},
			Runs:  []store.RuleRun{{RuleName: "Work", RanAt: time.Now(), Status: "done"}},
		},
	}
	for i, d := range pages {
		for _, page := range []string{"dashboard", "controls", "schedules", "settings"} {
			d.Page = page
			var sb strings.Builder
			if err := tmpl.ExecuteTemplate(&sb, "index.html", d); err != nil {
				t.Fatalf("data %d page %s: %v", i, page, err)
			}
		}
		for _, name := range []string{"departures", "rules"} {
			var sb strings.Builder
			if err := tmpl.ExecuteTemplate(&sb, name, d); err != nil {
				t.Fatalf("data %d partial %s: %v", i, name, err)
			}
		}
	}
}
