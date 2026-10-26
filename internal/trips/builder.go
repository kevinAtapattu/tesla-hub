package trips

import (
	"context"
	"log/slog"
	"time"
)

const (
	telemetryGap = 5 * time.Minute  // no telemetry this long ends a drive (car slept or lost signal)
	pollingGap   = 10 * time.Minute // polled samples are 2 min apart while driving
	pathPoints   = 1500
)

// Store is what the builder reads and writes.
type Store interface {
	LastTripEnd(ctx context.Context, vin string) (time.Time, error)
	FirstTelemetry(ctx context.Context, vin string) (time.Time, error)
	TelemetrySamples(ctx context.Context, vin string, after time.Time, limit int) ([]Sample, error)
	PollingSamples(ctx context.Context, vin string, from, to time.Time) ([]Sample, error)
	SaveTrip(ctx context.Context, vin, source string, t Trip) error
}

// Builder saves each drive as a trip once it has finished.
type Builder struct {
	Store Store
	VIN   func() string
}

func (b *Builder) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if vin := b.VIN(); vin != "" {
			if err := b.Build(ctx, vin, time.Now()); err != nil {
				slog.Warn("build trips", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Build saves every finished drive after the newest saved trip. Drives before
// telemetry started come from the 2-minute polling data.
func (b *Builder) Build(ctx context.Context, vin string, now time.Time) error {
	last, err := b.Store.LastTripEnd(ctx, vin)
	if err != nil {
		return err
	}
	first, err := b.Store.FirstTelemetry(ctx, vin)
	if err != nil {
		return err
	}

	pollUntil := first
	if first.IsZero() {
		pollUntil = now
	}
	if last.Before(pollUntil) {
		samples, err := b.Store.PollingSamples(ctx, vin, last, pollUntil)
		if err != nil {
			return err
		}
		if err := b.save(ctx, vin, "polling", samples, pollingGap, now, !first.IsZero()); err != nil {
			return err
		}
	}
	if first.IsZero() {
		return nil
	}
	samples, err := b.Store.TelemetrySamples(ctx, vin, last, 500000)
	if err != nil {
		return err
	}
	return b.save(ctx, vin, "telemetry", Fill(samples), telemetryGap, now, false)
}

// save stores finished drives. The newest one may still be under way unless the
// car parked or went quiet; closed says the data source has ended for good.
func (b *Builder) save(ctx context.Context, vin, source string, samples []Sample, gap time.Duration, now time.Time, closed bool) error {
	segs := Segment(samples, gap)
	for i, seg := range segs {
		end := seg[len(seg)-1]
		// The newest drive may still be under way, or stopped briefly and about to go on.
		wait := gap
		if !end.driving() {
			wait = MinStop
		}
		ongoing := i == len(segs)-1 && !closed && now.Sub(end.Time) < wait
		if ongoing {
			break
		}
		t := Summarize(seg, pathPoints)
		if t.DistanceKm < 0.2 && t.MaxSpeedKmh < 10 {
			continue // shuffling in a driveway, not a drive
		}
		if err := b.Store.SaveTrip(ctx, vin, source, t); err != nil {
			return err
		}
		slog.Info("trip saved", "source", source, "start", t.Start, "km", t.DistanceKm)
	}
	return nil
}

// Current is the drive under way right now, if any.
func (b *Builder) Current(ctx context.Context, vin string, now time.Time) (*Trip, error) {
	last, err := b.Store.LastTripEnd(ctx, vin)
	if err != nil {
		return nil, err
	}
	if since := now.Add(-6 * time.Hour); last.Before(since) {
		last = since
	}
	samples, err := b.Store.TelemetrySamples(ctx, vin, last, 50000)
	if err != nil || len(samples) == 0 {
		return nil, err
	}
	segs := Segment(Fill(samples), telemetryGap)
	if len(segs) == 0 {
		return nil, nil
	}
	seg := segs[len(segs)-1]
	if end := seg[len(seg)-1]; !end.driving() || now.Sub(end.Time) > telemetryGap {
		return nil, nil
	}
	t := Summarize(seg, pathPoints)
	return &t, nil
}

// Fill carries each field forward from earlier records, since telemetry only
// sends values that changed.
func Fill(samples []Sample) []Sample {
	var cur Sample
	out := make([]Sample, 0, len(samples))
	for _, s := range samples {
		cur.Time = s.Time
		if s.Gear != "" {
			cur.Gear = s.Gear
		}
		keep := func(dst **float64, v *float64) {
			if v != nil {
				*dst = v
			}
		}
		keep(&cur.SpeedKmh, s.SpeedKmh)
		keep(&cur.Latitude, s.Latitude)
		keep(&cur.Longitude, s.Longitude)
		keep(&cur.OdometerKm, s.OdometerKm)
		keep(&cur.Soc, s.Soc)
		keep(&cur.PowerKW, s.PowerKW)
		keep(&cur.LatAccel, s.LatAccel)
		keep(&cur.LongAccel, s.LongAccel)
		out = append(out, cur)
	}
	return out
}
