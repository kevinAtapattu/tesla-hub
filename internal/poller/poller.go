// Package poller periodically records vehicle data while keeping Fleet API spend low.
//
// Every request is billed, and requests to an awake car stop it from sleeping, so:
//   - asleep/offline: only check the (cheap, non-waking) vehicle state, rarely
//   - driving or charging: sample vehicle_data at a moderate rate
//   - parked and idle: take one sample, then leave it alone long enough to fall asleep
package poller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/kevin/tesla-hub/internal/config"
	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/tesla"
)

type Poller struct {
	Cfg   *config.Config
	Fleet *tesla.Client
	Store *store.Store

	nudge chan struct{}

	mu        sync.Mutex
	vin       string
	name      string
	state     string
	lastCheck time.Time
	nextCheck time.Time
	lastErr   string
}

type Status struct {
	VIN, Name, State, LastErr string
	LastCheck, NextCheck      time.Time
}

func New(cfg *config.Config, fleet *tesla.Client, st *store.Store) *Poller {
	p := &Poller{Cfg: cfg, Fleet: fleet, Store: st, nudge: make(chan struct{}, 1), vin: cfg.VIN}
	if p.vin == "" {
		// Pick up the car from the last snapshot, so the dashboard works after a
		// restart even while polling is paused (e.g. the monthly budget is spent).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		p.vin, p.name, _ = st.LastVehicle(ctx)
		cancel()
	}
	return p
}

// Nudge asks for a check right away (never wakes the car).
func (p *Poller) Nudge() {
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}

func (p *Poller) VIN() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.vin
}

func (p *Poller) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{VIN: p.vin, Name: p.name, State: p.state, LastErr: p.lastErr, LastCheck: p.lastCheck, NextCheck: p.nextCheck}
}

func (p *Poller) Run(ctx context.Context) {
	for {
		wait := p.check(ctx)
		p.mu.Lock()
		p.nextCheck = time.Now().Add(wait)
		p.mu.Unlock()

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-p.nudge:
			t.Stop()
		case <-t.C:
		}
	}
}

// check does one round and returns how long to wait before the next.
func (p *Poller) check(ctx context.Context) time.Duration {
	wait, err := p.checkOnce(ctx)
	p.mu.Lock()
	p.lastCheck = time.Now()
	p.lastErr = ""
	if err != nil {
		p.lastErr = err.Error()
	}
	p.mu.Unlock()
	if err != nil && !errors.Is(err, tesla.ErrNotConnected) {
		slog.Warn("poll", "err", err)
	}
	return wait
}

func (p *Poller) checkOnce(ctx context.Context) (time.Duration, error) {
	if !p.Fleet.Auth.Connected(ctx) {
		return 30 * time.Second, tesla.ErrNotConnected
	}

	if usage, err := p.Store.MonthUsage(ctx); err == nil && total(usage) >= p.Cfg.MonthlyBudget {
		return time.Hour, errors.New("monthly request budget reached; polling paused (raise MONTHLY_REQUEST_BUDGET to resume)")
	}

	vin := p.VIN()
	if vin == "" {
		vs, err := p.Fleet.ListVehicles(ctx)
		if err != nil {
			return 5 * time.Minute, err
		}
		if len(vs) == 0 {
			return time.Hour, errors.New("no vehicles on this Tesla account")
		}
		vin = vs[0].VIN
		p.mu.Lock()
		p.vin, p.name = vin, vs[0].DisplayName
		p.mu.Unlock()
		slog.Info("using vehicle", "vin", vin, "name", vs[0].DisplayName)
	}

	v, err := p.Fleet.Vehicle(ctx, vin)
	if err != nil {
		return 5 * time.Minute, err
	}
	p.mu.Lock()
	p.state = v.State
	if p.name == "" {
		p.name = v.DisplayName
	}
	p.mu.Unlock()
	_ = p.Store.RecordPresence(ctx, vin, v.State)

	if v.State != "online" {
		return p.Cfg.SleepCheckInterval, nil
	}

	snap, err := p.Fleet.VehicleData(ctx, vin)
	if errors.Is(err, tesla.ErrAsleep) {
		return p.Cfg.SleepCheckInterval, nil
	}
	if err != nil {
		return 5 * time.Minute, err
	}
	snap.VIN, snap.State = vin, v.State
	if err := p.Store.InsertSnapshot(ctx, snap); err != nil {
		return time.Minute, err
	}

	switch {
	case snap.Driving():
		return p.Cfg.DrivingInterval, nil
	case snap.Charging(), snap.ClimateRunning():
		return p.Cfg.ChargingInterval, nil
	default:
		return p.Cfg.ParkedInterval, nil
	}
}

// total counts billable requests; streamed telemetry signals are budgeted separately.
func total(m map[string]int) int {
	n := 0
	for k, v := range m {
		if k != "signal" {
			n += v
		}
	}
	return n
}
