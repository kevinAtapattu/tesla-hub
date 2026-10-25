// Package web serves the dashboard, the OAuth callback, and car controls.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"github.com/kevin/tesla-hub/internal/alerts"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teslamotors/vehicle-command/pkg/protocol"

	"github.com/kevin/tesla-hub/internal/assistant"
	"github.com/kevin/tesla-hub/internal/config"
	"github.com/kevin/tesla-hub/internal/planner"
	"github.com/kevin/tesla-hub/internal/poller"
	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/telemetry"
	"github.com/kevin/tesla-hub/internal/tesla"
	"github.com/kevin/tesla-hub/internal/trips"
	"github.com/kevin/tesla-hub/internal/weather"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	Cfg       *config.Config
	Auth      *tesla.Auth
	Store     *store.Store
	Poller    *poller.Poller
	Commander *tesla.Commander
	Weather   *weather.Client
	// RunScheduler asks the scheduler to check for due departures right away.
	RunScheduler func()
	// Trips finds the drive under way for the live card.
	Trips *trips.Builder
	// Telemetry is the live feed from the car; nil when TELEMETRY_HOST isn't set.
	Telemetry *telemetry.Receiver
	// Alerts sends a phone notification when the car starts a drive.
	Alerts *alerts.Watcher
	// Assistant answers messages like "warm it up, I'll be there in 15". Nil without an API key.
	Assistant *assistant.Assistant

	tmpl *template.Template
}

func (s *Server) Handler() http.Handler {
	s.tmpl = template.Must(template.New("").Funcs(funcs(s.Cfg.Location)).ParseFS(templateFS, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page("dashboard"))
	mux.HandleFunc("GET /controls", s.page("controls"))
	mux.HandleFunc("GET /schedules", s.page("schedules"))
	mux.HandleFunc("GET /settings", s.page("settings"))
	mux.HandleFunc("GET /trips", s.tripsPage)
	mux.HandleFunc("GET /trips/{id}", s.tripPage)
	mux.HandleFunc("POST /trips/{id}/driver", s.setTripDriver)
	mux.HandleFunc("POST /driver", s.driverCheckIn) // the phone; checks its own token
	mux.HandleFunc("GET /live", s.livePartial)
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/callback", s.callback) // must stay public: Tesla redirects here
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("POST /refresh", s.refresh)
	mux.HandleFunc("POST /climate/on", s.climateOn)
	mux.HandleFunc("POST /climate/off", s.climateOff)
	mux.HandleFunc("POST /cmd/{name}", s.simpleCommand)
	mux.HandleFunc("GET /rules", s.rulesPartial)
	mux.HandleFunc("POST /rules", s.createRule)
	mux.HandleFunc("POST /rules/{id}/toggle", s.toggleRule)
	mux.HandleFunc("DELETE /rules/{id}", s.deleteRule)
	mux.HandleFunc("POST /depart", s.depart)
	mux.HandleFunc("DELETE /departures/{id}", s.cancelDeparture)
	mux.HandleFunc("POST /assistant", s.assistantAPI) // the phone; checks its own token
	mux.HandleFunc("POST /assistant/try", s.assistantTry)
	mux.HandleFunc("POST /assistant/token", s.newToken)
	mux.HandleFunc("POST /settings/prefs", s.savePrefs)
	mux.HandleFunc("POST /telemetry/configure", s.configureTelemetry)
	mux.HandleFunc("POST /telemetry/check", s.checkTelemetry)
	mux.HandleFunc("POST /alerts/mode", s.setAlertMode)
	mux.HandleFunc("POST /alerts/test", s.testAlert)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return s.basicAuth(mux)
}

// basicAuth protects everything except the OAuth callback when DASHBOARD_PASSWORD is set.
// Set it before exposing the dashboard beyond your own machine.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	if s.Cfg.DashboardPassword == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, ok := r.BasicAuth()
		if r.URL.Path == "/auth/callback" || r.URL.Path == "/healthz" || r.URL.Path == "/assistant" || r.URL.Path == "/driver" ||
			(ok && subtle.ConstantTimeCompare([]byte(pw), []byte(s.Cfg.DashboardPassword)) == 1) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="tesla-hub"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// --- pages ---

type setupStep struct {
	Done   bool
	Title  string
	Detail string
}

type pageData struct {
	Page      string // which sidebar page is shown
	Status    poller.Status
	Latest    *tesla.Snapshot
	Usage     map[string]int
	UsageCost float64
	Budget    int
	Rules     []*store.Rule
	Runs      []store.RuleRun
	Connected bool
	Setup     []setupStep
	SetupDone bool
	Domain    string
	Flash     string
	Error     string

	Now            time.Time
	Range          string // chart window: "24h" or "7d"
	Weather        *weather.Report
	Preview        *planner.Plan // what "leaving in 15 min" would do right now
	Departures     []store.Departure
	Notice         string // result of the last "leaving" request
	TodayKm        *float64
	Battery        chart
	Temps          chart
	Speed          chart
	Prefs          planner.Prefs
	AssistantOn    bool
	AssistantModel string
	AssistantURL   string
	DriverURL      string
	AssistantToken string
	AssistantLog   []store.AssistantEntry

	Trips            []store.SavedTrip
	TripPreviews     map[int64]template.HTML // route previews for the trip list, by trip ID
	TripPreviewPaths map[int64]string        // the same routes as JSON, for the mini maps
	Trip             *store.SavedTrip
	TripPath         template.JS
	TripSpeed        chart
	TripPower        chart
	TripG            template.HTML
	Live             *trips.Trip // drive under way

	Drivers      []string
	DriverFilter string // trips page: only this driver's trips
	TripTotals   TripTotals

	TelemetryOn     bool
	TelemetryHost   string
	TelemetryPort   int
	TelemetryStatus telemetry.Status
	TelemetryNote   string // result of the last configure/check, shown in the card

	AlertsOn   bool
	Alerts     alerts.Settings
	AlertsNote string

	// DrivingEvery is the poll interval while driving, shown so the speed chart isn't mistaken for live data.
	DrivingEvery string
}

func (s *Server) data(ctx context.Context, rangeParam string) pageData {
	d := pageData{Status: s.Poller.Status(), Budget: s.Cfg.MonthlyBudget, Domain: s.Cfg.Domain, Now: time.Now(), Range: "24h",
		DrivingEvery: strings.TrimSuffix(s.Cfg.DrivingInterval.String(), "0s")}
	if rangeParam == "7d" {
		d.Range = "7d"
	}
	d.Connected = s.Auth.Connected(ctx)
	if d.Status.VIN != "" {
		d.Latest, _ = s.Store.LatestSnapshot(ctx, d.Status.VIN)
	}
	d.Usage, _ = s.Store.MonthUsage(ctx)
	d.UsageCost = estimateCost(d.Usage)
	d.Rules, _ = s.Store.ListRules(ctx)
	d.Runs, _ = s.Store.RecentRuns(ctx, 8)
	d.Departures, _ = s.Store.RecentDepartures(ctx)
	d.Prefs = s.prefs(ctx)
	d.AssistantOn = s.Assistant != nil
	if d.AssistantOn {
		d.AssistantModel = s.Assistant.Model()
	}
	d.AssistantURL = s.Cfg.PublicBaseURL + "/assistant"
	d.AssistantToken, _ = s.AssistantToken(ctx)
	d.AssistantLog, _ = s.Store.RecentAssistant(ctx, 8)
	if s.Telemetry != nil {
		d.TelemetryOn, d.TelemetryHost, d.TelemetryPort, d.TelemetryStatus = true, s.Cfg.TelemetryHost, s.Cfg.TelemetryPort, s.Telemetry.Status()
	}

	if d.Latest != nil && d.Latest.Latitude != nil && d.Latest.Longitude != nil && s.Weather != nil {
		var err error
		if d.Weather, err = s.Weather.Get(ctx, *d.Latest.Latitude, *d.Latest.Longitude); err != nil {
			slog.Warn("weather", "err", err)
		}
	}
	p := s.plan(ctx, d.Latest, d.Weather, d.Now, d.Now.Add(15*time.Minute), nil)
	d.Preview = &p

	if d.Status.VIN != "" && d.Latest != nil {
		to := d.Now
		from := to.Add(-24 * time.Hour)
		if d.Range == "7d" {
			from = to.Add(-7 * 24 * time.Hour)
		}
		pts, err := s.Store.Series(ctx, d.Status.VIN, from)
		if err != nil {
			slog.Warn("series", "err", err)
		}
		loc := s.Cfg.Location
		zero, hundred := 0.0, 100.0
		d.Battery = buildChart(pts, from, to, loc, "%", &zero, &hundred, series{Name: "Battery", Class: "s-battery", Value: batteryOf, Area: true})
		d.Temps = buildChart(pts, from, to, loc, "°", nil, nil,
			series{Name: "Cabin", Class: "s-inside", Value: insideOf}, series{Name: "Outside (car sensor)", Class: "s-outside", Value: outsideOf})
		d.Speed = buildChart(pts, from, to, loc, "", &zero, nil, series{Name: "Speed", Class: "s-speed", Value: speedOf, Area: true})

		local := d.Now.In(loc)
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		d.TodayKm, _ = s.Store.DistanceSince(ctx, d.Status.VIN, midnight)
	}

	missing := s.Cfg.MissingTeslaApp()
	// Load the key the same way the commander does, so ✓ means commands can use it.
	_, keyErr := protocol.LoadPrivateKey(s.Cfg.PrivateKeyPath)
	d.Setup = []setupStep{
		{len(missing) == 0, "Tesla app credentials in .env", "missing: " + strings.Join(missing, ", ")},
		{keyErr == nil, "Private key readable", fmt.Sprint(keyErr)},
		{d.Connected, "Tesla account connected", "click Connect Tesla below"},
		{d.Status.VIN != "", "Vehicle found", "waiting for first poll"},
		{d.Latest != nil, "First data received", "pair the key, wake the car from the Tesla app, then press Refresh"},
	}
	d.SetupDone = true
	for _, st := range d.Setup {
		d.SetupDone = d.SetupDone && st.Done
	}
	return d
}

// estimateCost uses Tesla's published per-request prices; check developer.tesla.com for current rates.
func estimateCost(u map[string]int) float64 {
	return float64(u["data"])*0.002 + float64(u["command"])*0.001 + float64(u["wake"])*0.02 + float64(u["signal"])/150000
}

// plan works out what "I'm leaving at departAt" should do. Outside temperature comes
// from the weather service when available, because the car's own sensor is stale
// while it sleeps. req carries what the owner explicitly asked for, if anything.
func (s *Server) plan(ctx context.Context, latest *tesla.Snapshot, wx *weather.Report, now, departAt time.Time, req *assistant.PrepareRequest) planner.Plan {
	in := planner.Input{Now: now.In(s.Cfg.Location), Departure: departAt.In(s.Cfg.Location), Prefs: s.prefs(ctx)}
	if latest != nil {
		in.CabinC, in.OutsideC = latest.InsideTempC, latest.OutsideTempC
	}
	if wx != nil {
		t := wx.TempC
		in.OutsideC, in.Precipitating = &t, wx.Precipitating()
	}
	if req != nil {
		switch req.Want {
		case "heat":
			in.Want = planner.Heat
		case "cool":
			in.Want = planner.Cool
		}
		in.TempC, in.SeatHeat, in.SteeringHeat = req.TempC, req.SeatHeat, req.SteeringHeat
	}
	return planner.Make(in)
}

// schedule plans a departure and, unless no climate is needed, saves it for the
// scheduler, which starts the climate at the plan's StartAt.
func (s *Server) schedule(ctx context.Context, departAt time.Time, req *assistant.PrepareRequest) (planner.Plan, error) {
	now := time.Now()
	var latest *tesla.Snapshot
	var wx *weather.Report
	if vin := s.Poller.VIN(); vin != "" {
		latest, _ = s.Store.LatestSnapshot(ctx, vin)
	}
	if latest != nil && latest.Latitude != nil && latest.Longitude != nil && s.Weather != nil {
		wx, _ = s.Weather.Get(ctx, *latest.Latitude, *latest.Longitude)
	}
	p := s.plan(ctx, latest, wx, now, departAt, req)
	if p.Action == planner.None {
		return p, nil
	}
	dep := &store.Departure{DepartAt: departAt, StartAt: p.StartAt, TempC: p.TempC,
		SeatHeat: p.SeatHeat, SteeringHeat: p.SteeringHeat, Reason: p.Reason}
	if err := s.Store.CreateDeparture(ctx, dep); err != nil {
		return p, err
	}
	if !p.StartAt.After(now.Add(30*time.Second)) && s.RunScheduler != nil {
		s.RunScheduler()
	}
	return p, nil
}

// parseClock turns "HH:MM" into the next time it occurs (today, or tomorrow if it has passed).
func (s *Server) parseClock(at string, now time.Time) (time.Time, error) {
	t, err := time.ParseInLocation("15:04", at, s.Cfg.Location)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(s.Cfg.Location)
	when := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, s.Cfg.Location)
	if when.Before(now) {
		when = when.AddDate(0, 0, 1)
	}
	return when, nil
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := s.data(r.Context(), r.URL.Query().Get("range"))
		d.Page = name
		d.AssistantURL = requestBase(r) + "/assistant"
		d.DriverURL = requestBase(r) + "/driver"
		d.Drivers, _ = s.Store.Drivers(r.Context())
		s.alertData(r.Context(), &d)
		d.Flash = r.URL.Query().Get("flash")
		d.Error = r.URL.Query().Get("error")
		s.render(w, "index.html", d)
	}
}

func (s *Server) rulesPartial(w http.ResponseWriter, r *http.Request) {
	s.render(w, "rules", s.data(r.Context(), ""))
}

func (s *Server) render(w http.ResponseWriter, name string, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, d); err != nil {
		slog.Error("render", "template", name, "err", err)
	}
}

// --- OAuth ---

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if missing := s.Cfg.MissingTeslaApp(); len(missing) > 0 {
		http.Redirect(w, r, "/?error="+urlq("Fill in "+strings.Join(missing, ", ")+" in .env and restart first"), http.StatusSeeOther)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "oauth_state", Value: state, Path: "/auth", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.Auth.AuthCodeURL(state), http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Redirect(w, r, "/?error="+urlq("Tesla login failed: "+e+" "+q.Get("error_description")), http.StatusSeeOther)
		return
	}
	c, err := r.Cookie("oauth_state")
	if err != nil || c.Value == "" || c.Value != q.Get("state") {
		http.Redirect(w, r, "/?error="+urlq("Login expired or state mismatch; try Connect again"), http.StatusSeeOther)
		return
	}
	if err := s.Auth.Exchange(r.Context(), q.Get("code")); err != nil {
		slog.Error("oauth exchange", "err", err)
		http.Redirect(w, r, "/?error="+urlq("Token exchange failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	s.Poller.Nudge()
	http.Redirect(w, r, "/?flash="+urlq("Tesla account connected"), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Disconnect(r.Context())
	http.Redirect(w, r, "/settings?flash="+urlq("Disconnected"), http.StatusSeeOther)
}

// --- controls (htmx endpoints return a small message fragment) ---

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	s.Poller.Nudge()
	w.Header().Set("HX-Trigger", "commandDone") // the page reloads status a few seconds later
	msg(w, "ok", "Checking the car… (no new data if it's asleep)")
}

var commandLabels = map[string]string{
	"lock": "Locked", "unlock": "Unlocked", "flash": "Lights flashed", "honk": "Honked",
	"charge_start": "Charging started", "charge_stop": "Charging stopped",
	"port_open": "Charge port opened", "port_close": "Charge port closed",
	"sentry_on": "Sentry Mode on", "sentry_off": "Sentry Mode off",
}

func (s *Server) simpleCommand(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	label, ok := commandLabels[name]
	if !ok {
		msg(w, "err", "Unknown command")
		return
	}
	vin := s.Poller.VIN()
	if vin == "" {
		msg(w, "err", "No vehicle yet")
		return
	}
	s.commandResult(w, s.Commander.Simple(r.Context(), vin, name), label)
}

func (s *Server) climateOn(w http.ResponseWriter, r *http.Request) {
	vin := s.Poller.VIN()
	if vin == "" {
		msg(w, "err", "No vehicle yet")
		return
	}
	temp, _ := strconv.ParseFloat(r.FormValue("temp_c"), 32)
	err := s.Commander.ClimateOn(r.Context(), vin, tesla.ClimateRequest{
		TempC:        float32(temp),
		SeatHeat:     r.FormValue("seat_heat") == "on",
		SteeringHeat: r.FormValue("steering_heat") == "on",
	})
	s.commandResult(w, err, "Climate on")
}

func (s *Server) climateOff(w http.ResponseWriter, r *http.Request) {
	vin := s.Poller.VIN()
	if vin == "" {
		msg(w, "err", "No vehicle yet")
		return
	}
	s.commandResult(w, s.Commander.ClimateOff(r.Context(), vin), "Climate off")
}

func (s *Server) commandResult(w http.ResponseWriter, err error, ok string) {
	if err != nil {
		slog.Error("command", "err", err)
		msg(w, "err", err.Error())
		return
	}
	s.Poller.Nudge()
	w.Header().Set("HX-Trigger", "commandDone")
	msg(w, "ok", ok)
}

// --- rules ---

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	rule := &store.Rule{
		Name:         strings.TrimSpace(r.FormValue("name")),
		Enabled:      true,
		Days:         strings.Join(r.Form["days"], ","),
		AtTime:       r.FormValue("at_time"),
		TempC:        floatOr(r.FormValue("temp_c"), 21),
		SeatHeat:     r.FormValue("seat_heat") == "on",
		SteeringHeat: r.FormValue("steering_heat") == "on",
	}
	if rule.Name == "" {
		rule.Name = "Preheat " + rule.AtTime
	}
	if r.FormValue("use_geofence") == "on" {
		rule.Latitude = optFloat(r.FormValue("latitude"))
		rule.Longitude = optFloat(r.FormValue("longitude"))
		rule.RadiusM = optFloat(r.FormValue("radius_m"))
	}
	if r.FormValue("use_max_temp") == "on" {
		rule.MaxOutsideTempC = optFloat(r.FormValue("max_outside_temp_c"))
	}
	if _, err := time.Parse("15:04", rule.AtTime); err != nil || rule.Days == "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		msg(w, "err", "Pick at least one day and a time")
		return
	}
	if err := s.Store.CreateRule(r.Context(), rule); err != nil {
		msg(w, "err", err.Error())
		return
	}
	s.rulesPartial(w, r)
}

func (s *Server) toggleRule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.Store.SetRuleEnabled(r.Context(), id, r.FormValue("enabled") == "true")
	s.rulesPartial(w, r)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.Store.DeleteRule(r.Context(), id)
	s.rulesPartial(w, r)
}

// --- departures ("I'm leaving in N minutes") ---

func (s *Server) depart(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	departAt := now
	if at := r.FormValue("at"); at != "" {
		var err error
		if departAt, err = s.parseClock(at, now); err != nil {
			s.renderDepartures(w, r, "Pick a valid time.")
			return
		}
	} else {
		mins, _ := strconv.Atoi(r.FormValue("minutes"))
		departAt = now.Add(time.Duration(max(mins, 0)) * time.Minute)
	}
	p, err := s.schedule(r.Context(), departAt, nil)
	if err != nil {
		s.renderDepartures(w, r, "Couldn't save: "+err.Error())
		return
	}
	s.renderDepartures(w, r, p.Reason)
}

func (s *Server) cancelDeparture(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	s.Store.CancelDeparture(r.Context(), id)
	s.renderDepartures(w, r, "Cancelled.")
}

func (s *Server) renderDepartures(w http.ResponseWriter, r *http.Request, notice string) {
	d := s.data(r.Context(), "")
	d.Notice = notice
	s.render(w, "departures", d)
}

// --- helpers ---

// msg writes a toast. kind is "ok" or "err".
func msg(w http.ResponseWriter, kind, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	icon := `<path d="M20 6 9 17l-5-5"/>`
	if kind == "err" {
		icon = `<circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/>`
	}
	fmt.Fprintf(w, `<div class="msg %s" role="status"><svg viewBox="0 0 24 24" aria-hidden="true">%s</svg><span>%s</span></div>`,
		kind, icon, template.HTMLEscapeString(text))
}

func urlq(s string) string { return strings.ReplaceAll(template.URLQueryEscaper(s), "+", "%20") }

func floatOr(s string, def float64) float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}

func optFloat(s string) *float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil
	}
	return &f
}

var dayNames = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func funcs(loc *time.Location) template.FuncMap {
	return template.FuncMap{
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(loc).Format("3:04 PM")
		},
		"mod": func(a, b int) int { return a % b },
		"nf":  rollingNumber,
		"inc": func(i int) int { return i + 1 },
		"deref": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"hms": func(d time.Duration) string {
			m := int(d.Round(time.Minute).Minutes())
			if m < 60 {
				return fmt.Sprintf("%d min", m)
			}
			return fmt.Sprintf("%d h %02d min", m/60, m%60)
		},
		"secs": func(d *time.Duration) string {
			if d == nil {
				return "—"
			}
			return fmt.Sprintf("%.1f s", d.Seconds())
		},
		"partOfDay": func(t time.Time) string {
			switch h := t.In(loc).Hour(); {
			case h < 5:
				return "Night drive"
			case h < 12:
				return "Morning drive"
			case h < 17:
				return "Afternoon drive"
			case h < 21:
				return "Evening drive"
			}
			return "Night drive"
		},
		"dur": func(mins *int) string {
			switch {
			case mins == nil:
				return "—"
			case *mins < 60:
				return fmt.Sprintf("%d min", *mins)
			case *mins%60 == 0:
				return fmt.Sprintf("%d h", *mins/60)
			}
			return fmt.Sprintf("%d h %d min", *mins/60, *mins%60)
		},
		"compass": func(deg *int) string {
			if deg == nil {
				return "—"
			}
			dirs := []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}
			return fmt.Sprintf("%s · %d°", dirs[((*deg%360+360)%360+22)/45%8], *deg)
		},
		"usageTotal": usageTotal,
		"usagePct": func(u map[string]int, budget int) int {
			if budget <= 0 {
				return 0
			}
			return min(usageTotal(u)*100/budget, 100)
		},
		"hour": func(t time.Time) string { return t.In(loc).Format("3 PM") },
		"sky":  weather.Describe,
		// fresh grades how old a reading is so stale data is visibly stale.
		"fresh": func(t time.Time) string {
			switch age := time.Since(t); {
			case age < 30*time.Minute:
				return "fresh"
			case age < 3*time.Hour:
				return "stale"
			}
			return "old"
		},
		"local": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(loc).Format("Mon Jan 2, 3:04 PM")
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := time.Since(t).Round(time.Minute)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%d min ago", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("%.0f h ago", d.Hours())
			}
			return fmt.Sprintf("%.0f days ago", d.Hours()/24)
		},
		"num": func(p *float64, digits int) string {
			if p == nil {
				return "—"
			}
			return strconv.FormatFloat(*p, 'f', digits, 64)
		},
		"int": func(p *int) string {
			if p == nil {
				return "—"
			}
			return strconv.Itoa(*p)
		},
		"yes": func(p *bool) bool { return p != nil && *p },
		// lat/lon offset a coordinate, for building the map's bounding box.
		"lat": offsetCoord,
		"lon": offsetCoord,
		"pct": func(p *int) int {
			if p == nil {
				return 0
			}
			return min(max(*p, 0), 100)
		},
		"days": func(days string) string {
			var out []string
			for _, f := range strings.Split(days, ",") {
				if n, err := strconv.Atoi(f); err == nil && n >= 0 && n < 7 {
					out = append(out, dayNames[n])
				}
			}
			if len(out) == 7 {
				return "Every day"
			}
			if strings.Join(out, ",") == "Mon,Tue,Wed,Thu,Fri" {
				return "Weekdays"
			}
			return strings.Join(out, " ")
		},
		"km": func(m *float64) string {
			if m == nil {
				return ""
			}
			return strconv.FormatFloat(*m/1000, 'f', 1, 64) + " km"
		},
		"f": func(p *float64) string {
			if p == nil {
				return ""
			}
			return strconv.FormatFloat(*p, 'f', -1, 64)
		},
	}
}

func offsetCoord(p *float64, d float64) string {
	if p == nil {
		return "0"
	}
	return strconv.FormatFloat(*p+d, 'f', 6, 64)
}

// rollingNumber renders a number the page animates into place (see the
// number-flow script in index.html). key ties it to the same number after an
// auto-refresh so it rolls from the old value; the text inside is the no-JS fallback.
func rollingNumber(key string, v any, digits int) template.HTML {
	var f float64
	switch x := v.(type) {
	case *float64:
		if x == nil {
			return "—"
		}
		f = *x
	case *int:
		if x == nil {
			return "—"
		}
		f = float64(*x)
	case float64:
		f = x
	case int:
		f = float64(x)
	default:
		return "—"
	}
	n := strconv.FormatFloat(f, 'f', digits, 64)
	if math.Abs(f) >= 10000 { // 12345 -> 12,345
		i := strings.IndexAny(n, ".")
		if i < 0 {
			i = len(n)
		}
		for j := i - 3; j > 0 && n[j-1] != '-'; j -= 3 {
			n = n[:j] + "," + n[j:]
		}
	}
	return template.HTML(fmt.Sprintf(`<span class="num" data-nf data-key="%s">%s</span>`, template.HTMLEscapeString(key), n))
}

// usageTotal counts billable requests. Streamed telemetry signals are priced
// separately (150,000 per $1) and don't count toward the polling budget.
func usageTotal(u map[string]int) int {
	n := 0
	for k, v := range u {
		if k != "signal" {
			n += v
		}
	}
	return n
}

// requestBase is how the browser reached the hub, e.g. https://tesla-hub.tailnet.ts.net
// through Tailscale, so links shown on the page work from the phone too.
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}
