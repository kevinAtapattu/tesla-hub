package rules

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/tesla"
)

type Scheduler struct {
	Store     *store.Store
	Commander *tesla.Commander
	Location  *time.Location
	VIN       func() string // current vehicle, "" until the poller has found one
	AfterRun  func()        // optional; e.g. ask the poller for a fresh sample

	nudgeOnce sync.Once
	nudge     chan struct{}
}

// Nudge makes the scheduler check right away instead of waiting for the next tick.
func (s *Scheduler) Nudge() {
	select {
	case s.nudgeChan() <- struct{}{}:
	default:
	}
}

func (s *Scheduler) nudgeChan() chan struct{} {
	s.nudgeOnce.Do(func() { s.nudge = make(chan struct{}, 1) })
	return s.nudge
}

// Run checks the rules every 30 seconds until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	nudge := s.nudgeChan()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-nudge:
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	vin := s.VIN()
	if vin == "" {
		return
	}
	s.runDepartures(ctx, vin)

	rules, err := s.Store.ListRules(ctx)
	if err != nil {
		slog.Error("list rules", "err", err)
		return
	}
	latest, err := s.Store.LatestSnapshot(ctx, vin)
	if err != nil {
		slog.Error("latest snapshot", "err", err)
		return
	}
	now := time.Now().In(s.Location)
	for _, r := range rules {
		d := Evaluate(r, now, latest)
		if !d.Due {
			continue
		}
		if d.SkipReason != "" {
			// Claiming with "skipped" also stops it being re-evaluated for the rest of the day.
			if _, claimed, err := s.Store.ClaimRuleRun(ctx, r.ID, now, "skipped", d.SkipReason); err == nil && claimed {
				slog.Info("rule skipped", "rule", r.Name, "reason", d.SkipReason)
			}
			continue
		}
		runID, claimed, err := s.Store.ClaimRuleRun(ctx, r.ID, now, "running", "")
		if err != nil || !claimed {
			continue
		}
		slog.Info("rule firing", "rule", r.Name)
		err = s.Commander.ClimateOn(ctx, vin, tesla.ClimateRequest{
			TempC: float32(r.TempC), SeatHeat: r.SeatHeat, SteeringHeat: r.SteeringHeat,
		})
		status, msg := "done", "climate on"
		if err != nil {
			status, msg = "failed", err.Error()
			slog.Error("rule failed", "rule", r.Name, "err", err)
		}
		if err := s.Store.FinishRuleRun(ctx, runID, status, msg); err != nil {
			slog.Error("finish rule run", "err", err)
		}
		if s.AfterRun != nil {
			s.AfterRun()
		}
	}
}

// runDepartures starts climate for one-off "leaving at X" plans whose start time has come.
func (s *Scheduler) runDepartures(ctx context.Context, vin string) {
	due, err := s.Store.DueDepartures(ctx, time.Now())
	if err != nil {
		slog.Error("due departures", "err", err)
		return
	}
	for _, d := range due {
		if ok, err := s.Store.ClaimDeparture(ctx, d.ID); err != nil || !ok {
			continue
		}
		if time.Since(d.DepartAt) > Window {
			// e.g. the Mac was asleep: heating the car after you've left is pointless.
			s.Store.FinishDeparture(ctx, d.ID, "missed", "the hub wasn't running at the start time")
			continue
		}
		slog.Info("departure firing", "depart_at", d.DepartAt, "temp", d.TempC)
		err := s.Commander.ClimateOn(ctx, vin, tesla.ClimateRequest{
			TempC: float32(d.TempC), SeatHeat: d.SeatHeat, SteeringHeat: d.SteeringHeat,
		})
		status, msg := "done", "climate on"
		if err != nil {
			status, msg = "failed", err.Error()
			slog.Error("departure failed", "err", err)
		}
		s.Store.FinishDeparture(ctx, d.ID, status, msg)
		if s.AfterRun != nil {
			s.AfterRun()
		}
	}
}
