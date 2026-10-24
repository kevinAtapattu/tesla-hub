package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/kevin/tesla-hub/internal/tesla"
)

// Runs against a real Postgres; skipped unless TEST_DATABASE_URL is set, e.g.
//
//	TEST_DATABASE_URL='postgres://tesla:tesla@127.0.0.1:5495/tesla?sslmode=disable' go test ./internal/store
//
// It only touches rows for a fake VIN and a rule it creates, and removes them afterwards.
func TestStoreAgainstPostgres(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close) // registered first so it runs after the row cleanup below
	const vin = "TESTVIN0000000000"
	t.Cleanup(func() {
		s.db.Exec(ctx, `DELETE FROM vehicle_states WHERE vin = $1`, vin)
		s.db.Exec(ctx, `DELETE FROM vehicle_presence WHERE vin = $1`, vin)
		s.db.Exec(ctx, `DELETE FROM rules WHERE name = 'store-test'`)
		s.db.Exec(ctx, `DELETE FROM departures WHERE reason = 'store-test'`)
		s.db.Exec(ctx, `UPDATE api_requests SET count = count - 1 WHERE month = date_trunc('month', now())::date AND kind = 'test'`)
		s.db.Exec(ctx, `DELETE FROM api_requests WHERE kind = 'test' AND count <= 0`)
	})

	raw := json.RawMessage(`{"vin":"` + vin + `","charge_state":{"battery_level":55,"battery_range":100},"climate_state":{"outside_temp":-4},"drive_state":{"latitude":43.6,"longitude":-79.4,"shift_state":null},"vehicle_state":{"locked":true}}`)
	snap, err := tesla.ParseVehicleData(raw)
	if err != nil {
		t.Fatal(err)
	}
	snap.VIN, snap.State = vin, "online"
	if err := s.InsertSnapshot(ctx, snap); err != nil {
		t.Fatal("insert:", err)
	}
	got, err := s.LatestSnapshot(ctx, vin)
	if err != nil || got == nil {
		t.Fatal("latest:", got, err)
	}
	if *got.BatteryLevel != 55 || *got.OutsideTempC != -4 || *got.Latitude != 43.6 || got.InsideTempC != nil || !*got.Locked {
		t.Fatalf("round trip wrong: %+v", got)
	}

	for _, st := range []string{"asleep", "asleep", "online"} {
		if err := s.RecordPresence(ctx, vin, st); err != nil {
			t.Fatal("presence:", err)
		}
	}
	var n int
	s.db.QueryRow(ctx, `SELECT count(*) FROM vehicle_presence WHERE vin = $1`, vin).Scan(&n)
	if n != 2 {
		t.Fatalf("presence rows = %d, want 2 (duplicates should be skipped)", n)
	}

	if err := s.CountRequest(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if u, err := s.MonthUsage(ctx); err != nil || u["test"] < 1 {
		t.Fatalf("usage = %v, %v", u, err)
	}

	r := &Rule{Name: "store-test", Enabled: true, Days: "1", AtTime: "07:00", TempC: 21}
	if err := s.CreateRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	today := time.Now()
	id, ok, err := s.ClaimRuleRun(ctx, r.ID, today, "running", "")
	if err != nil || !ok {
		t.Fatal("first claim:", ok, err)
	}
	if _, ok, err := s.ClaimRuleRun(ctx, r.ID, today, "running", ""); err != nil || ok {
		t.Fatal("second claim should be refused:", ok, err)
	}
	if err := s.FinishRuleRun(ctx, id, "done", "climate on"); err != nil {
		t.Fatal(err)
	}
	runs, err := s.RecentRuns(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, run := range runs {
		found = found || (run.RuleName == "store-test" && run.Status == "done")
	}
	if !found {
		t.Fatalf("run not in RecentRuns: %+v", runs)
	}

	pts, err := s.Series(ctx, vin, time.Now().Add(-time.Hour))
	if err != nil || len(pts) != 1 || *pts[0].BatteryLevel != 55 {
		t.Fatalf("series = %+v, %v", pts, err)
	}
	if d, err := s.DistanceSince(ctx, vin, time.Now().Add(-time.Hour)); err != nil || d != nil && *d != 0 {
		t.Fatalf("distance = %v, %v", d, err)
	}

	dep := &Departure{DepartAt: time.Now().Add(time.Minute), StartAt: time.Now().Add(-time.Second), TempC: 22, Reason: "store-test"}
	if err := s.CreateDeparture(ctx, dep); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueDepartures(ctx, time.Now())
	if err != nil || len(due) == 0 {
		t.Fatalf("due = %v, %v", due, err)
	}
	if ok, err := s.ClaimDeparture(ctx, dep.ID); err != nil || !ok {
		t.Fatal("claim departure:", ok, err)
	}
	if ok, _ := s.ClaimDeparture(ctx, dep.ID); ok {
		t.Fatal("departure claimed twice")
	}
	if err := s.FinishDeparture(ctx, dep.ID, "done", "climate on"); err != nil {
		t.Fatal(err)
	}
	if recent, err := s.RecentDepartures(ctx); err != nil || len(recent) == 0 {
		t.Fatalf("recent = %v, %v", recent, err)
	}
}
