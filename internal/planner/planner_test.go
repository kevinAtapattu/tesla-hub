package planner

import (
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func TestMake(t *testing.T) {
	now := time.Date(2026, 1, 15, 7, 0, 0, 0, time.UTC)
	in20 := now.Add(20 * time.Minute)

	tests := []struct {
		name        string
		in          Input
		action      Action
		temp        float64
		seat, wheel bool
		startAt     time.Time
		reasonHas   string
	}{
		{"freezing heats hard", Input{Now: now, Departure: in20, OutsideC: f(-8), CabinC: f(-5)},
			Heat, 22, true, true, in20.Add(-20 * time.Minute), "seat and steering wheel"},
		{"cool morning heats with seat", Input{Now: now, Departure: in20, OutsideC: f(3), CabinC: f(8)},
			Heat, 21, true, false, in20.Add(-10 * time.Minute), "Heating to 21°"},
		{"hot day cools", Input{Now: now, Departure: in20, OutsideC: f(31), CabinC: f(40)},
			Cool, 20, false, false, in20.Add(-15 * time.Minute), "Cooling"},
		{"mild needs nothing", Input{Now: now, Departure: in20, OutsideC: f(18), CabinC: f(20)},
			None, 21, false, false, in20, "no climate needed"},
		{"leaving now starts now", Input{Now: now, Departure: now, OutsideC: f(-8)},
			Heat, 22, true, true, now, "starting now"},
		{"snow adds defrost time", Input{Now: now, Departure: now.Add(time.Hour), OutsideC: f(-1), CabinC: f(10), Precipitating: true},
			Heat, 22, true, true, now.Add(time.Hour - 14*time.Minute), ""},
		{"no data still heats", Input{Now: now, Departure: in20},
			Heat, 21, false, false, in20.Add(-6 * time.Minute), "No temperature data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Make(tt.in)
			if p.Action != tt.action || p.TempC != tt.temp || p.SeatHeat != tt.seat || p.SteeringHeat != tt.wheel {
				t.Fatalf("got %+v", p)
			}
			if !p.StartAt.Equal(tt.startAt) {
				t.Errorf("StartAt = %s, want %s", p.StartAt.Format("15:04"), tt.startAt.Format("15:04"))
			}
			if !strings.Contains(p.Reason, tt.reasonHas) {
				t.Errorf("Reason %q missing %q", p.Reason, tt.reasonHas)
			}
		})
	}
}
