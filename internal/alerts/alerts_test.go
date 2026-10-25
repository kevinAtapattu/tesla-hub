package alerts

import (
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func TestDetector(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	var d Detector
	d.Seed("P", t0.Add(-2*time.Hour))

	if d.Observe(at(0), "", f(0)) {
		t.Error("alerted while parked")
	}
	if !d.Observe(at(5), "R", nil) {
		t.Error("no alert when the car left Park after two hours")
	}
	if d.Observe(at(8), "D", f(10)) || d.Observe(at(60), "", f(50)) {
		t.Error("alerted twice for one drive")
	}
	// Speed-only records carry the gear forward, so a long drive stays one drive.
	for s := 61; s < 30*60; s += 30 {
		if d.Observe(at(s), "", f(60)) {
			t.Fatalf("alerted mid-drive at %ds", s)
		}
	}
	// Parks, then reverses out of the spot a minute later: same outing.
	d.Observe(at(30*60), "P", f(0))
	if d.Observe(at(31*60), "R", nil) {
		t.Error("alerted for a parking shuffle")
	}
	d.Observe(at(32*60), "P", f(0))
	// Leaves again after a long stop.
	if !d.Observe(at(32*60+int(Quiet.Seconds())+60), "D", nil) {
		t.Error("no alert after a long stop")
	}
}

func TestDetectorSeededMidDrive(t *testing.T) {
	// The hub restarts during a drive: the first record mustn't look like a new drive.
	t0 := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	var d Detector
	d.Seed("D", t0.Add(-3*time.Second))
	if d.Observe(t0, "", f(70)) {
		t.Error("alerted after a restart mid-drive")
	}
}
