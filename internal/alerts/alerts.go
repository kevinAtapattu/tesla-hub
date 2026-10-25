// Package alerts posts to a Discord channel (through a webhook) when the car
// starts a drive, mentioning the user so their phone is notified.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kevin/tesla-hub/internal/telemetry"
)

const (
	// Quiet is how long the car must have been out of gear before driving counts
	// as a new drive, so reversing into a spot or a stop for gas doesn't alert.
	Quiet = 10 * time.Minute
	// Stale records arrive in a batch when the car reconnects; they update the
	// state but are too old to alert about.
	stale = 2 * time.Minute

	settingKey = "drive_alerts"
)

// Modes for Settings.Mode.
const (
	Off   = "off"
	Next  = "next"  // the next drive only, then off
	Every = "every" // every drive
)

// Settings is what's saved in the settings table.
type Settings struct {
	Mode   string    `json:"mode"`
	LastAt time.Time `json:"last_at,omitempty"` // last drive alert sent
}

// Detector works out when a drive starts from a stream of telemetry records
// (oldest first). Records only carry fields that changed, so it remembers the gear.
type Detector struct {
	gear        string
	speed       float64
	lastDriving time.Time
}

// Seed sets what's known from before the hub started.
func (d *Detector) Seed(gear string, lastDriving time.Time) {
	d.gear, d.lastDriving = gear, lastDriving
}

// Observe takes one record and reports whether it starts a drive.
func (d *Detector) Observe(t time.Time, gear string, speedKmh *float64) bool {
	if gear != "" {
		d.gear = gear
	}
	if speedKmh != nil {
		d.speed = *speedKmh
	}
	driving := d.gear == "D" || d.gear == "R" || d.gear == "N" || (d.gear == "" && d.speed > 5)
	if !driving {
		return false
	}
	start := d.lastDriving.IsZero() || t.Sub(d.lastDriving) >= Quiet
	if t.After(d.lastDriving) {
		d.lastDriving = t
	}
	return start
}

// Store keeps the settings and knows the car's state before the hub started.
type Store interface {
	GetSetting(ctx context.Context, key string, v any) (bool, error)
	SetSetting(ctx context.Context, key string, v any) error
	DriveState(ctx context.Context, vin string, before time.Time) (gear string, lastDriving time.Time, err error)
}

// Watcher gets every telemetry record and sends the alert.
type Watcher struct {
	Store     Store
	Webhook   string // Discord webhook URL
	MentionID string // Discord user ID to @mention, so the phone notifies
	Client    *http.Client
	OnStart   func() // called when a drive starts, whether or not alerts are on

	once sync.Once
	ch   chan *telemetry.Point
	cars map[string]*car
}

// destWait is how long an alert waits for the navigation destination when the
// car starts driving before it's known; it usually follows within seconds.
const destWait = 30 * time.Second

type car struct {
	Detector
	soc     *float64
	dest    string    // navigation destination name, "" when none
	pending time.Time // a drive started at this time and its alert hasn't gone yet
}

// Observe queues a record without blocking the telemetry feed.
func (w *Watcher) Observe(p *telemetry.Point) {
	w.init()
	select {
	case w.ch <- p:
	default:
		slog.Warn("drive alerts: queue full, record skipped")
	}
}

func (w *Watcher) init() {
	w.once.Do(func() {
		w.ch = make(chan *telemetry.Point, 1000)
		w.cars = map[string]*car{}
	})
}

func (w *Watcher) Run(ctx context.Context) {
	w.init()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-w.ch:
			w.handle(ctx, p)
		case now := <-tick.C:
			for _, c := range w.cars {
				if !c.pending.IsZero() && now.Sub(c.pending) >= destWait {
					w.alert(ctx, c)
				}
			}
		}
	}
}

func (w *Watcher) handle(ctx context.Context, p *telemetry.Point) {
	if p.Resend {
		return
	}
	c := w.cars[p.VIN]
	if c == nil {
		c = &car{}
		gear, last, err := w.Store.DriveState(ctx, p.VIN, p.Time)
		if err != nil {
			slog.Warn("drive alerts: load state", "err", err)
		}
		c.Seed(gear, last)
		w.cars[p.VIN] = c
	}
	if p.Soc != nil {
		c.soc = p.Soc
	}
	if v, ok := p.Data["DestinationName"]; ok {
		c.dest, _ = v.(string) // nil when the route is cleared
	}
	if c.Observe(p.Time, p.Gear, p.SpeedKmh) && time.Since(p.Time) <= stale {
		if w.OnStart != nil {
			w.OnStart()
		}
		c.pending = p.Time
	}
	if !c.pending.IsZero() && c.dest != "" {
		w.alert(ctx, c)
	}
}

// Message is the alert text for a drive that just started.
func Message(dest string, soc *float64, mode string) string {
	msg := "🚗 The car started driving"
	if dest != "" {
		msg += " to " + dest
	}
	if soc != nil {
		msg += fmt.Sprintf(", battery %.0f%%", math.Round(*soc))
	}
	msg += "."
	if mode == Next {
		msg += " Drive alerts are off again until you turn them back on."
	}
	return msg
}

func (w *Watcher) alert(ctx context.Context, c *car) {
	c.pending = time.Time{}
	s, err := w.Settings(ctx)
	if err != nil || s.Mode == Off {
		return
	}
	if err := w.Send(ctx, Message(c.dest, c.soc, s.Mode)); err != nil {
		slog.Error("drive alert", "err", err)
		return
	}
	slog.Info("drive alert sent", "mode", s.Mode, "destination", c.dest != "")
	s.LastAt = time.Now()
	if s.Mode == Next {
		s.Mode = Off
	}
	if err := w.Store.SetSetting(ctx, settingKey, s); err != nil {
		slog.Error("save drive alert settings", "err", err)
	}
}

func (w *Watcher) Settings(ctx context.Context) (Settings, error) {
	s := Settings{Mode: Off}
	_, err := w.Store.GetSetting(ctx, settingKey, &s)
	return s, err
}

func (w *Watcher) SetMode(ctx context.Context, mode string) error {
	if mode != Off && mode != Next && mode != Every {
		return fmt.Errorf("unknown mode %q", mode)
	}
	s, err := w.Settings(ctx)
	if err != nil {
		return err
	}
	s.Mode = mode
	return w.Store.SetSetting(ctx, settingKey, s)
}

// Ready says whether the webhook is configured.
func (w *Watcher) Ready() bool { return w.Webhook != "" }

// Send posts msg to the Discord channel.
func (w *Watcher) Send(ctx context.Context, msg string) error {
	if !w.Ready() {
		return fmt.Errorf("set DISCORD_WEBHOOK_URL in .env, then run docker compose up -d hub")
	}
	body := map[string]any{"username": "Tesla Hub", "content": msg, "allowed_mentions": map[string]any{"parse": []string{}}}
	if w.MentionID != "" {
		body["content"] = msg + " <@" + w.MentionID + ">"
		body["allowed_mentions"] = map[string]any{"users": []string{w.MentionID}}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Webhook, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := w.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("discord: %w", errors.Unwrap(err)) // the error text would include the webhook URL
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("discord: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
