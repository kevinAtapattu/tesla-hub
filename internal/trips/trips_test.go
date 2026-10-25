package trips

import (
	"math"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

// drive builds a 1 Hz drive: park, launch 0→120 km/h in 6 s, cruise, brake to a stop, park.
func drive(start time.Time) []Sample {
	var out []Sample
	add := func(sec int, gear string, speed, power, longAcc float64) {
		lat := 43.69 + float64(sec)*0.0003
		out = append(out, Sample{
			Time: start.Add(time.Duration(sec) * time.Second), Gear: gear, SpeedKmh: f(speed),
			Latitude: f(lat), Longitude: f(-79.85), OdometerKm: f(1000 + float64(sec)*0.03),
			Soc: f(80 - float64(sec)*0.01), PowerKW: f(power), LatAccel: f(1.5), LongAccel: f(longAcc),
		})
	}
	add(0, "P", 0, 0, 0)
	add(1, "D", 0, 1, 0)
	for s := 2; s <= 8; s++ { // 0 → 120 km/h by 20 km/h steps
		add(s, "D", float64(s-2)*20, 250, 5.5)
	}
	for s := 9; s <= 60; s++ {
		add(s, "D", 120, 30, 0)
	}
	for s := 61; s <= 66; s++ { // braking with regen
		add(s, "D", 120-float64(s-60)*20, -60, -7)
	}
	add(67, "P", 0, 0, 0)
	return out
}

func TestSegmentAndSummarize(t *testing.T) {
	start := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	samples := append(drive(start), drive(start.Add(time.Hour))...)
	segs := Segment(samples, 5*time.Minute)
	if len(segs) != 2 {
		t.Fatalf("got %d trips, want 2", len(segs))
	}
	tr := Summarize(segs[0], 50)

	near := func(name string, got, want, tol float64) {
		t.Helper()
		if math.Abs(got-want) > tol {
			t.Errorf("%s = %.3f, want %.3f", name, got, want)
		}
	}
	near("max speed", tr.MaxSpeedKmh, 120, 0.01)
	near("distance (odometer)", tr.DistanceKm, 66*0.03, 0.001)
	near("peak power", tr.PeakPowerKW, 250, 0.01)
	near("peak regen", tr.PeakRegenKW, 60, 0.01)
	near("max accel g", tr.MaxAccelG, 5.5/gravity, 0.001)
	near("max brake g", tr.MaxBrakeG, 7/gravity, 0.001)
	near("max lateral g", tr.MaxLatG, 1.5/gravity, 0.001)
	near("regen kWh", tr.RegenKWh, 60*6/3600.0, 0.001)
	if tr.ZeroTo100 == nil {
		t.Fatal("expected a 0–100 time")
	}
	// Standing at 2 s (0 km/h); 100 km/h is reached at 7 s → 5 s.
	near("0–100 s", tr.ZeroTo100.Seconds(), 5, 0.01)
	if tr.Moving <= 0 || tr.AvgSpeedKmh <= 0 {
		t.Errorf("moving %v avg %.1f", tr.Moving, tr.AvgSpeedKmh)
	}
	if len(tr.Path) > 50 || math.Abs(tr.Path[0].Lat-43.6903) > 1e-9 { // starts at the first sample in Drive
		t.Errorf("path has %d points starting %v", len(tr.Path), tr.Path[0])
	}
}

func TestDataGapEndsTrip(t *testing.T) {
	start := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	s := []Sample{
		{Time: start, Gear: "D", SpeedKmh: f(50)},
		{Time: start.Add(time.Minute), Gear: "D", SpeedKmh: f(50)},
		{Time: start.Add(30 * time.Minute), Gear: "D", SpeedKmh: f(50)}, // car slept or lost signal
		{Time: start.Add(31 * time.Minute), Gear: "D", SpeedKmh: f(50)},
	}
	if n := len(Segment(s, 5*time.Minute)); n != 2 {
		t.Fatalf("got %d trips, want 2", n)
	}
}

func TestNoLaunchMeansNoZeroTo100(t *testing.T) {
	start := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	var s []Sample
	for i := 0; i <= 60; i++ { // a slow 0→100 over a minute isn't a launch
		s = append(s, Sample{Time: start.Add(time.Duration(i) * time.Second), Gear: "D", SpeedKmh: f(float64(i) * 100 / 60)})
	}
	if tr := Summarize(s, 10); tr.ZeroTo100 != nil {
		t.Errorf("0–100 = %v, want none", *tr.ZeroTo100)
	}
}
