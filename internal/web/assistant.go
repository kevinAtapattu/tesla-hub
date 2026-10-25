package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/assistant"
	"github.com/kevin/tesla-hub/internal/planner"
	"github.com/kevin/tesla-hub/internal/store"
)

const (
	prefsKey = "prefs"
	tokenKey = "assistant_token"
)

func (s *Server) prefs(ctx context.Context) planner.Prefs {
	var p planner.Prefs
	if _, err := s.Store.GetSetting(ctx, prefsKey, &p); err != nil {
		slog.Warn("load prefs", "err", err)
	}
	return p.WithDefaults()
}

// AssistantToken returns the secret the phone sends with each message, creating it on first use.
func (s *Server) AssistantToken(ctx context.Context) (string, error) {
	var tok string
	if ok, err := s.Store.GetSetting(ctx, tokenKey, &tok); err != nil || (ok && tok != "") {
		return tok, err
	}
	return s.rotateToken(ctx)
}

func (s *Server) rotateToken(ctx context.Context) (string, error) {
	b := make([]byte, 24)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	return tok, s.Store.SetSetting(ctx, tokenKey, tok)
}

// --- HTTP ---

// assistantAPI is what the iPhone Shortcut calls. It takes the message as JSON
// {"message": "..."}, a form field, or a plain-text body, and answers in plain
// text so the Shortcut can hand the reply straight to Siri.
func (s *Server) assistantAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	want, err := s.AssistantToken(r.Context())
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err != nil || want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "The hub didn't accept the access token. Copy it again from Settings.")
		return
	}
	message, err := readMessage(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, err.Error())
		return
	}
	reply, err := s.ask(r.Context(), message)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
	}
	io.WriteString(w, reply)
}

// assistantTry is the test box in Settings (protected by the dashboard password, not the token).
func (s *Server) assistantTry(w http.ResponseWriter, r *http.Request) {
	message := strings.TrimSpace(r.FormValue("message"))
	if message != "" {
		s.ask(r.Context(), message)
	}
	s.render(w, "assistant-log", s.data(r.Context(), ""))
}

func (s *Server) savePrefs(w http.ResponseWriter, r *http.Request) {
	p := s.prefs(r.Context())
	for field, dst := range map[string]*float64{
		"warm_c": &p.WarmC, "cool_c": &p.CoolC, "heat_below_c": &p.HeatBelowC,
		"cool_above_c": &p.CoolAboveC, "seat_heat_below_c": &p.SeatHeatBelowC, "wheel_heat_below_c": &p.WheelHeatBelowC,
	} {
		if v, err := strconv.ParseFloat(r.FormValue(field), 64); err == nil {
			*dst = v
		}
	}
	p.WarmC = min(max(p.WarmC, 15), 28)
	p.CoolC = min(max(p.CoolC, 15), 28)
	if err := s.Store.SetSetting(r.Context(), prefsKey, p); err != nil {
		http.Redirect(w, r, "/settings?error="+urlq("Couldn't save: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?flash="+urlq("Preferences saved"), http.StatusSeeOther)
}

func (s *Server) newToken(w http.ResponseWriter, r *http.Request) {
	if _, err := s.rotateToken(r.Context()); err != nil {
		http.Redirect(w, r, "/settings?error="+urlq(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?flash="+urlq("New token created. Update it in your Shortcut.")+"#assistant", http.StatusSeeOther)
}

func readMessage(r *http.Request) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return "", err
	}
	msg := strings.TrimSpace(string(body))
	switch ct := r.Header.Get("Content-Type"); {
	case strings.HasPrefix(ct, "application/json"):
		var v struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return "", errors.New(`send JSON like {"message": "warm it up"}`)
		}
		msg = strings.TrimSpace(v.Message)
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		// A form with a message field, or plain text sent with a form content type.
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if v := strings.TrimSpace(r.PostFormValue("message")); v != "" {
			msg = v
		}
	}
	if msg == "" {
		return "", errors.New("the message was empty")
	}
	return msg, nil
}

// ask runs the assistant and logs the exchange. On failure the returned text is
// still something sensible for Siri to say.
func (s *Server) ask(ctx context.Context, message string) (string, error) {
	if s.Assistant == nil {
		return "The assistant isn't set up yet. Add ANTHROPIC_API_KEY to the hub's .env file.", errors.New("no api key")
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	reply, err := s.Assistant.Handle(ctx, message)
	entry := store.AssistantEntry{Message: message, Reply: reply}
	if err != nil {
		slog.Error("assistant", "err", err)
		entry.Error = err.Error()
		reply = "Sorry, something went wrong on the hub. Check Settings for details."
	}
	if err := s.Store.LogAssistant(context.WithoutCancel(ctx), entry); err != nil {
		slog.Warn("log assistant", "err", err)
	}
	return reply, err
}

// --- what the assistant can do (assistant.Actions) ---

type assistantActions struct{ s *Server }

// AssistantActions exposes the hub's car actions to the assistant.
func (s *Server) AssistantActions() assistant.Actions { return assistantActions{s} }

func (a assistantActions) Status(ctx context.Context) (string, error) {
	s := a.s
	d := s.data(ctx, "")
	if d.Latest == nil {
		return "No data from the car yet.", nil
	}
	v := d.Latest
	var b strings.Builder
	fmt.Fprintf(&b, "Car: %s, %s. Last reading %s.\n", orDash(d.Status.Name), orDash(d.Status.State), ago(v.Time))
	fmt.Fprintf(&b, "Battery %s%% (limit %s%%), range %s km, charging state %s.\n", intp(v.BatteryLevel), intp(v.ChargeLimit), floatp(v.RangeKm, 0), orDash(v.ChargingState))
	fmt.Fprintf(&b, "Cabin %s°C, climate %s. ", floatp(v.InsideTempC, 0), onOff(v.ClimateOn))
	if d.Weather != nil {
		fmt.Fprintf(&b, "Outside %.0f°C, %s.\n", d.Weather.TempC, strings.ToLower(d.Weather.Summary()))
	} else {
		fmt.Fprintf(&b, "Outside %s°C (car sensor).\n", floatp(v.OutsideTempC, 0))
	}
	fmt.Fprintf(&b, "Doors %s, Sentry %s.\n", map[bool]string{true: "locked", false: "unlocked"}[v.Locked != nil && *v.Locked], onOff(v.Sentry))
	for _, dep := range d.Departures {
		if dep.Status == "pending" {
			fmt.Fprintf(&b, "Scheduled: ready by %s, %s\n", dep.DepartAt.In(s.Cfg.Location).Format("15:04"), dep.Reason)
		}
	}
	return b.String(), nil
}

func (a assistantActions) Prepare(ctx context.Context, r assistant.PrepareRequest) (string, error) {
	s := a.s
	if s.Poller.VIN() == "" {
		return "", errors.New("no car found on the account yet")
	}
	now := time.Now()
	departAt := now
	switch {
	case r.LeaveAt != "":
		t, err := s.parseClock(r.LeaveAt, now)
		if err != nil {
			return "", fmt.Errorf("leave_at must be HH:MM, got %q", r.LeaveAt)
		}
		departAt = t
	case r.MinutesFromNow != nil:
		departAt = now.Add(time.Duration(min(max(*r.MinutesFromNow, 0), 24*60)) * time.Minute)
	}
	// A new request replaces any earlier one ("in 15... actually make it 25").
	if _, err := s.Store.CancelPendingDepartures(ctx); err != nil {
		return "", err
	}
	p, err := s.schedule(ctx, departAt, &r)
	if err != nil {
		return "", err
	}
	if p.Action == planner.None {
		return p.Reason + " Nothing was scheduled.", nil
	}
	return fmt.Sprintf("%s Ready by %s.", p.Reason, departAt.In(s.Cfg.Location).Format("15:04")), nil
}

func (a assistantActions) CancelPlans(ctx context.Context) (string, error) {
	n, err := a.s.Store.CancelPendingDepartures(ctx)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "There was nothing scheduled to cancel.", nil
	}
	return fmt.Sprintf("Cancelled %d scheduled plan(s). Climate that's already running stays on.", n), nil
}

func (a assistantActions) ClimateOff(ctx context.Context) (string, error) {
	s := a.s
	vin := s.Poller.VIN()
	if vin == "" {
		return "", errors.New("no car found on the account yet")
	}
	// Waking the car can take up to a minute, longer than Siri waits, so finish in the background.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.Commander.ClimateOff(ctx, vin); err != nil {
			slog.Error("assistant climate off", "err", err)
			return
		}
		s.Poller.Nudge()
	}()
	return "Turning the climate off now. It can take up to a minute if the car is asleep.", nil
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func intp(p *int) string {
	if p == nil {
		return "unknown"
	}
	return strconv.Itoa(*p)
}

func floatp(p *float64, digits int) string {
	if p == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*p, 'f', digits, 64)
}

func onOff(p *bool) string {
	if p != nil && *p {
		return "on"
	}
	return "off"
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	if d < time.Minute {
		return "just now"
	}
	return d.String() + " ago"
}
