// Command resummarize adds newly added race stats to trips already saved, from
// their stored telemetry. It keeps every trip's ID, driver and existing stats.
package main

import (
	"context"
	"log"
	"os"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/trips"
)

func main() {
	ctx := context.Background()
	st, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	vin := os.Getenv("VIN")
	ts, err := st.ListTrips(ctx, vin, "", 100000)
	if err != nil {
		log.Fatal(err)
	}
	n := 0
	for _, t := range ts {
		if t.Source != "telemetry" {
			continue // polling has no 1 Hz data
		}
		samples, err := st.TripSamples(ctx, vin, t.Start, t.End)
		if err != nil || len(samples) == 0 {
			log.Printf("trip %d: %v", t.ID, err)
			continue
		}
		fresh := trips.Summarize(trips.Fill(samples), 0)
		t.ZeroTo60, t.Rolling60to100, t.Rolling80to120 = fresh.ZeroTo60, fresh.Rolling60to100, fresh.Rolling80to120
		t.MaxCombinedG = fresh.MaxCombinedG
		if fresh.MaxSpeedKmh == t.MaxSpeedKmh {
			t.TopSpeedAt = fresh.TopSpeedAt
		}
		if err := st.UpdateTripSummary(ctx, t.ID, t.Trip); err != nil {
			log.Fatal(err)
		}
		n++
	}
	log.Printf("updated %d trips", n)
}
