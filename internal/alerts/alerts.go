// Package alerts pushes a phone notification when the car starts a drive, using
// ntfy (https://ntfy.sh): the hub posts to a random topic and the ntfy app on the
// phone, subscribed to that topic, shows it.
package alerts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	server     = "https://ntfy.sh"
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
	Topic  string    `json:"topic"`
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
	Store    Store
	ClickURL string // opened when the notification is tapped
	Location *time.Location
	Client   *http.Client

	once      sync.Once
	ch        chan *telemetry.Point
	detectors map[string]*Detector
	soc       map[string]float64
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
		w.detectors = map[string]*Detector{}
		w.soc = map[string]float64{}
	})
}

func (w *Watcher) Run(ctx context.Context) {
	w.init()
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-w.ch:
			w.handle(ctx, p)
		}
	}
}

func (w *Watcher) handle(ctx context.Context, p *telemetry.Point) {
	if p.Resend {
		return
	}
	if p.Soc != nil {
		w.soc[p.VIN] = *p.Soc
	}
	d := w.detectors[p.VIN]
	if d == nil {
		d = &Detector{}
		gear, last, err := w.Store.DriveState(ctx, p.VIN, p.Time)
		if err != nil {
			slog.Warn("drive alerts: load state", "err", err)
		}
		d.Seed(gear, last)
		w.detectors[p.VIN] = d
	}
	if !d.Observe(p.Time, p.Gear, p.SpeedKmh) || time.Since(p.Time) > stale {
		return
	}
	s, err := w.Settings(ctx)
	if err != nil || s.Mode == Off {
		return
	}
	msg := "Started driving at " + p.Time.In(w.Location).Format("3:04 PM")
	if soc, ok := w.soc[p.VIN]; ok {
		msg += fmt.Sprintf(" · battery %.0f%%", math.Round(soc))
	}
	if s.Mode == Next {
		msg += ". Alerts are off now; turn them on again in Settings."
	}
	if err := w.Send(ctx, s.Topic, "The car is moving", msg); err != nil {
		slog.Error("drive alert", "err", err)
		return
	}
	slog.Info("drive alert sent", "mode", s.Mode)
	s.LastAt = time.Now()
	if s.Mode == Next {
		s.Mode = Off
	}
	if err := w.Store.SetSetting(ctx, settingKey, s); err != nil {
		slog.Error("save drive alert settings", "err", err)
	}
}

// Settings returns the saved settings, creating the topic on first use.
func (w *Watcher) Settings(ctx context.Context) (Settings, error) {
	s := Settings{Mode: Off}
	if _, err := w.Store.GetSetting(ctx, settingKey, &s); err != nil {
		return s, err
	}
	if s.Topic == "" {
		b := make([]byte, 16)
		rand.Read(b)
		s.Topic = "tesla-hub-" + hex.EncodeToString(b)
		return s, w.Store.SetSetting(ctx, settingKey, s)
	}
	return s, nil
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

// Send posts a notification to the topic.
func (w *Watcher) Send(ctx context.Context, topic, title, msg string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/"+topic, strings.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "red_car")
	req.Header.Set("Priority", "high")
	if w.ClickURL != "" {
		req.Header.Set("Click", w.ClickURL)
	}
	c := w.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ntfy: %s", resp.Status)
	}
	return nil
}
