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

func TestShortStopStaysOneTrip(t *testing.T) {
	start := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	at := func(min float64) time.Time { return start.Add(time.Duration(min * float64(time.Minute))) }
	s := []Sample{
		{Time: at(0), Gear: "D", SpeedKmh: f(50)},
		{Time: at(10), Gear: "D", SpeedKmh: f(50)},
		{Time: at(11), Gear: "P", SpeedKmh: f(0)}, // letting someone out
		{Time: at(11.5), Gear: "P", SpeedKmh: f(0)},
		{Time: at(11.8), Gear: "D", SpeedKmh: f(40)},
		{Time: at(20), Gear: "D", SpeedKmh: f(40)},
		{Time: at(21), Gear: "P", SpeedKmh: f(0)}, // a friend's house
		{Time: at(24), Gear: "", SpeedKmh: f(0)},
		{Time: at(26), Gear: "P", SpeedKmh: f(0)},
		{Time: at(60), Gear: "D", SpeedKmh: f(60)},
		{Time: at(70), Gear: "P", SpeedKmh: f(0)},
	}
	segs := Segment(s, time.Hour) // samples are sparse here; only the stops should split
	if len(segs) != 2 {
		t.Fatalf("got %d trips, want 2", len(segs))
	}
	if end := segs[0][len(segs[0])-1].Time; !end.Equal(at(21)) {
		t.Errorf("first trip ends at %v, want when it parked at the friend's house", end.Sub(start))
	}
}

func TestRollingRuns(t *testing.T) {
	start := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	at := func(sec float64) time.Time { return start.Add(time.Duration(sec * float64(time.Second))) }
	// Cruising at 50, then 50 → 130 at 10 km/h per second from t=10 s.
	var s []Sample
	for i := 0; i <= 10; i++ {
		s = append(s, Sample{Time: at(float64(i)), Gear: "D", SpeedKmh: f(50)})
	}
	for i := 1; i <= 8; i++ {
		s = append(s, Sample{Time: at(float64(10 + i)), Gear: "D", SpeedKmh: f(50 + float64(i)*10)})
	}
	tr := Summarize(s, 10)
	for name, got := range map[string]*time.Duration{"60–100": tr.Rolling60to100, "80–120": tr.Rolling80to120} {
		if got == nil || math.Abs(got.Seconds()-4) > 0.01 {
			t.Errorf("%s = %v, want 4s", name, got)
		}
	}
	if tr.ZeroTo60 != nil || tr.ZeroTo100 != nil {
		t.Errorf("no standing start, got 0–60 %v 0–100 %v", tr.ZeroTo60, tr.ZeroTo100)
	}

	// Lifting off at 90 and dropping back below 60 restarts the run.
	s = []Sample{
		{Time: at(0), Gear: "D", SpeedKmh: f(55)}, {Time: at(2), Gear: "D", SpeedKmh: f(90)},
		{Time: at(4), Gear: "D", SpeedKmh: f(55)}, {Time: at(5), Gear: "D", SpeedKmh: f(65)}, {Time: at(7), Gear: "D", SpeedKmh: f(105)},
	}
	// 60 is crossed at 4.5 s, 100 at 6.75 s.
	if r := Summarize(s, 10).Rolling60to100; r == nil || math.Abs(r.Seconds()-2.25) > 0.01 {
		t.Errorf("60–100 after lifting = %v, want 2.25s", r)
	}
}

func TestSectionCarriesEarlierValues(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	raw := []Sample{
		// gear and power only arrive before the section starts
		{Time: t0, Gear: "D", SpeedKmh: f(40), PowerKW: f(30), Latitude: f(43.6), Longitude: f(-79.6)},
	}
	for i := 1; i <= 60; i++ {
		raw = append(raw, Sample{Time: t0.Add(time.Duration(i) * time.Second), SpeedKmh: f(40 + float64(i)),
			Latitude: f(43.6 + float64(i)*0.0002), Longitude: f(-79.6)})
	}
	from, to := t0.Add(20*time.Second), t0.Add(50*time.Second)
	s, ok := Section(raw, from, to)
	if !ok {
		t.Fatal("no section")
	}
	if !s.Start.Equal(from) || !s.End.Equal(to) || s.MaxSpeedKmh != 90 {
		t.Errorf("start %v end %v top %.0f", s.Start, s.End, s.MaxSpeedKmh)
	}
	if s.PeakPowerKW != 30 || s.Moving == 0 {
		t.Errorf("earlier gear/power weren't carried in: power %.0f, moving %s", s.PeakPowerKW, s.Moving)
	}
	if _, ok := Section(raw, t0.Add(time.Hour), t0.Add(2*time.Hour)); ok {
		t.Error("a section with no data should not be ok")
	}
}
