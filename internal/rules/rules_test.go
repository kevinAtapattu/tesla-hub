package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/tesla"
)

func ptr[T any](v T) *T { return &v }

func TestEvaluate(t *testing.T) {
	toronto, _ := time.LoadLocation("America/Toronto")
	home := [2]float64{43.6532, -79.3832}
	// Wednesday 2026-10-21 07:32 local
	wed0732 := time.Date(2026, 10, 21, 7, 32, 0, 0, toronto)

	weekday := &store.Rule{Enabled: true, Days: "1,2,3,4,5", AtTime: "07:30", TempC: 21}
	geofenced := &store.Rule{Enabled: true, Days: "3", AtTime: "07:30",
		Latitude: &home[0], Longitude: &home[1], RadiusM: ptr(200.0)}
	cold := &store.Rule{Enabled: true, Days: "3", AtTime: "07:30", MaxOutsideTempC: ptr(5.0)}

	atHome := &tesla.Snapshot{Latitude: ptr(43.6535), Longitude: ptr(-79.3830)}
	atWork := &tesla.Snapshot{Latitude: ptr(43.7000), Longitude: ptr(-79.4000)}

	tests := []struct {
		name     string
		rule     *store.Rule
		now      time.Time
		latest   *tesla.Snapshot
		wantDue  bool
		wantSkip string // substring; "" means no skip
	}{
		{"fires in window", weekday, wed0732, nil, true, ""},
		{"before time", weekday, wed0732.Add(-3 * time.Minute), nil, false, ""},
		{"after window", weekday, wed0732.Add(Window), nil, false, ""},
		{"wrong day", weekday, wed0732.AddDate(0, 0, 3), nil, false, ""}, // Saturday
		{"disabled", &store.Rule{Days: "3", AtTime: "07:30"}, wed0732, nil, false, ""},
		{"geofence inside", geofenced, wed0732, atHome, true, ""},
		{"geofence outside", geofenced, wed0732, atWork, true, "km from"},
		{"geofence no location", geofenced, wed0732, nil, true, "unknown"},
		{"warm skips", cold, wed0732, &tesla.Snapshot{OutsideTempC: ptr(12.0)}, true, "above"},
		{"cold runs", cold, wed0732, &tesla.Snapshot{OutsideTempC: ptr(-3.0)}, true, ""},
		{"unknown temp runs", cold, wed0732, &tesla.Snapshot{}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Evaluate(tt.rule, tt.now, tt.latest)
			if d.Due != tt.wantDue {
				t.Fatalf("Due = %v, want %v", d.Due, tt.wantDue)
			}
			if tt.wantSkip == "" && d.SkipReason != "" {
				t.Fatalf("unexpected skip: %s", d.SkipReason)
			}
			if tt.wantSkip != "" && !strings.Contains(d.SkipReason, tt.wantSkip) {
				t.Fatalf("SkipReason = %q, want it to contain %q", d.SkipReason, tt.wantSkip)
			}
		})
	}
}

func TestDistanceM(t *testing.T) {
	// Toronto City Hall to CN Tower is roughly 1.4 km.
	d := DistanceM(43.6534, -79.3841, 43.6426, -79.3871)
	if d < 1100 || d > 1400 {
		t.Fatalf("distance = %.0f m", d)
	}
}
