package web

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/trips"
)

// Record is an all-time best and the drive that set it.
type Record struct {
	Label  string
	Value  string // "—" until there's one
	N      float64
	Unit   string
	TripID int64
	When   time.Time
	Driver string
}

// RaceBoard is the home page: personal bests across every drive.
type RaceBoard struct {
	Best0100 Record
	Runs     []Record // 0–60 and the rolling runs
	LaunchG  Record
	TopSpeed Record
	Tiles    []Record
	Drivers  []DriverRow
	Drives   int
	Km       float64
	Form     []Form
	Honours  []Honour
}

// DriverRow is one driver's bests, for the leaderboard.
type DriverRow struct {
	Name     string
	Drives   int
	Km       float64
	Best0100 *time.Duration
	TopSpeed float64
	MaxG     float64
}

func raceBoard(all []store.SavedTrip, driver string) RaceBoard {
	var ts, tele []store.SavedTrip
	for _, t := range all {
		if driver != "" && t.Driver != driver {
			continue
		}
		ts = append(ts, t)
		// Launch times and g-forces need the 1 Hz stream; polling is minutes apart.
		if t.Source == "telemetry" {
			tele = append(tele, t)
		}
	}
	b := RaceBoard{
		Best0100: durRecord("0–100 km/h", tele, func(t trips.Trip) *time.Duration { return t.ZeroTo100 }),
		TopSpeed: maxRecord("Top speed", "km/h", 0, ts, func(t trips.Trip) float64 { return t.MaxSpeedKmh }),
		Runs: []Record{
			durRecord("0–60", tele, func(t trips.Trip) *time.Duration { return t.ZeroTo60 }),
			durRecord("60–100", tele, func(t trips.Trip) *time.Duration { return t.Rolling60to100 }),
			durRecord("80–120", tele, func(t trips.Trip) *time.Duration { return t.Rolling80to120 }),
		},
		LaunchG: maxRecord("Launch", "g", 2, tele, func(t trips.Trip) float64 { return t.MaxAccelG }),
		Tiles: []Record{
			maxRecord("Peak power", "kW", 0, tele, func(t trips.Trip) float64 { return t.PeakPowerKW }),
			maxRecord("Braking", "g", 2, tele, func(t trips.Trip) float64 { return t.MaxBrakeG }),
			maxRecord("Cornering", "g", 2, tele, func(t trips.Trip) float64 { return t.MaxLatG }),
			maxRecord("Combined", "g", 2, tele, func(t trips.Trip) float64 { return t.MaxCombinedG }),
		},
		Drives:  len(ts),
		Form:    formGuide(ts),
		Honours: honours(ts),
	}
	rows := map[string]*DriverRow{}
	for _, t := range ts {
		b.Km += t.DistanceKm
		if t.Driver == "" {
			continue
		}
		r := rows[t.Driver]
		if r == nil {
			r = &DriverRow{Name: t.Driver}
			rows[t.Driver] = r
		}
		r.Drives++
		r.Km += t.DistanceKm
		r.TopSpeed = math.Max(r.TopSpeed, t.MaxSpeedKmh)
		if t.Source == "telemetry" {
			r.MaxG = math.Max(r.MaxG, t.MaxCombinedG)
			if t.ZeroTo100 != nil && (r.Best0100 == nil || *t.ZeroTo100 < *r.Best0100) {
				r.Best0100 = t.ZeroTo100
			}
		}
	}
	for _, r := range rows {
		b.Drivers = append(b.Drivers, *r)
	}
	// Fastest 0–100 first; drivers without one go last, by top speed.
	sort.Slice(b.Drivers, func(i, j int) bool {
		a, c := b.Drivers[i], b.Drivers[j]
		if (a.Best0100 == nil) != (c.Best0100 == nil) {
			return a.Best0100 != nil
		}
		if a.Best0100 != nil && *a.Best0100 != *c.Best0100 {
			return *a.Best0100 < *c.Best0100
		}
		return a.TopSpeed > c.TopSpeed
	})
	return b
}

func durRecord(label string, ts []store.SavedTrip, get func(trips.Trip) *time.Duration) Record {
	r := Record{Label: label, Value: "—", Unit: "s"}
	var best *time.Duration
	for _, t := range ts {
		if d := get(t.Trip); d != nil && (best == nil || *d < *best) {
			best, r.TripID, r.When, r.Driver = d, t.ID, t.Start, t.Driver
		}
	}
	if best != nil {
		r.Value, r.N = fmt.Sprintf("%.1f", best.Seconds()), best.Seconds()
	}
	return r
}

func maxRecord(label, unit string, digits int, ts []store.SavedTrip, get func(trips.Trip) float64) Record {
	r := Record{Label: label, Value: "—", Unit: unit}
	best := 0.0
	for _, t := range ts {
		if v := get(t.Trip); v > best {
			best, r.TripID, r.When, r.Driver = v, t.ID, t.Start, t.Driver
		}
	}
	if best > 0 {
		r.Value, r.N = fmt.Sprintf("%.*f", digits, best), best
	}
	return r
}

func (s *Server) racePage(w http.ResponseWriter, r *http.Request) {
	d := s.data(r.Context(), "")
	d.Page = "race"
	d.Flash = r.URL.Query().Get("flash")
	d.Error = r.URL.Query().Get("error")
	d.DriverFilter = r.URL.Query().Get("driver")
	d.Drivers, _ = s.Store.Drivers(r.Context())
	if vin := s.Poller.VIN(); vin != "" {
		all, _ := s.Store.TripStats(r.Context(), vin)
		d.Race = raceBoard(all, d.DriverFilter)
		d.Trips, _ = s.Store.ListTrips(r.Context(), vin, d.DriverFilter, 4)
		s.previews(&d)
		songsFor := d.Trips
		for _, t := range all { // the top-speed record's song too
			if t.ID == d.Race.TopSpeed.TripID {
				songsFor = append(songsFor[:len(songsFor):len(songsFor)], t)
			}
		}
		d.TripSongs = s.topSongs(r.Context(), songsFor)
	}
	d.Live = s.liveTrip(r)
	d.NowPlaying = s.nowPlaying(r.Context())
	s.render(w, "index.html", d)
}

// Track is a song or station that played in the car.
type Track struct {
	Art                  string // cover image URL, looked up by title and artist; "" when unknown
	Title, Artist, Album string
	Station, Source      string
	Start                time.Time
	Ended                time.Time // when the car cleared or stopped it; zero if it ran into the next track
	Status               string    // Playing, Paused or Stopped; "" when unknown
}

// Line is "Title · Artist" for one-line display.
func (t Track) Line() string {
	parts := []string{}
	for _, p := range []string{t.Title, t.Artist} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}

// playlist turns media changes into the tracks that played, oldest first. The
// car sends only what changed, so the other fields carry forward; a new title
// starts a new track, and an artist or album arriving a moment later fills it in.
func playlist(events []store.MediaEvent) []Track {
	var out []Track
	var cur Track
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	for _, e := range events {
		newTitle := e.Title != nil && *e.Title != cur.Title
		if newTitle {
			cur = Track{Title: *e.Title, Start: e.Time, Station: cur.Station, Source: cur.Source, Status: cur.Status}
		}
		set(&cur.Artist, e.Artist)
		set(&cur.Album, e.Album)
		set(&cur.Station, e.Station)
		set(&cur.Source, e.Source)
		if e.Status != nil {
			cur.Status = strings.TrimPrefix(*e.Status, "MediaStatus")
		}
		// a cleared title or stopped player ends the last track
		if n := len(out); n > 0 && out[n-1].Ended.IsZero() &&
			((newTitle && cur.Title == "") || cur.Status == "Stopped") {
			out[n-1].Ended = e.Time
		}
		switch {
		case newTitle && cur.Title != "":
			out = append(out, cur)
		case len(out) > 0 && out[len(out)-1].Start.Equal(cur.Start):
			out[len(out)-1] = cur // fill in the current track
		}
	}
	return out
}

// nowPlaying is what the car is playing, or nil when it's asleep, not
// streaming, or quiet.
func (s *Server) nowPlaying(ctx context.Context) *Track {
	vin := s.Poller.VIN()
	if vin == "" || s.Telemetry == nil || time.Since(s.Telemetry.Status().LastRecord) > 5*time.Minute {
		return nil
	}
	ev, err := s.Store.MediaEvents(ctx, vin, time.Now().Add(-12*time.Hour), time.Now())
	if err != nil {
		return nil
	}
	ts := playlist(ev)
	if len(ts) == 0 {
		return nil
	}
	t := ts[len(ts)-1]
	if t.Status == "Stopped" {
		return nil
	}
	s.coverArt(ctx, &t)
	return &t
}

// soundtrack is what played during a drive, and the track on at its top speed.
func (s *Server) soundtrack(ctx context.Context, t *store.SavedTrip, samples []trips.Sample) ([]Track, *Track) {
	ev, err := s.Store.MediaEvents(ctx, s.Poller.VIN(), t.Start.Add(-2*time.Hour), t.End)
	if err != nil {
		return nil, nil
	}
	all := playlist(ev)
	var out []Track
	for i, tr := range all {
		// keep tracks still playing when the drive started
		if i+1 < len(all) && !all[i+1].Start.After(t.Start) {
			continue
		}
		if tr.Status == "Stopped" && tr.Start.Before(t.Start) {
			continue
		}
		out = append(out, tr)
	}
	top := t.TopSpeedAt
	if top.IsZero() { // trips saved before TopSpeedAt was recorded
		best := -1.0
		for _, x := range samples {
			if x.SpeedKmh != nil && *x.SpeedKmh > best {
				best, top = *x.SpeedKmh, x.Time
			}
		}
	}
	at := trackAt(out, top)
	ptrs := []*Track{at}
	for i := range out {
		ptrs = append(ptrs, &out[i])
	}
	s.coverArt(ctx, ptrs...)
	return out, at
}

// trackAt is the track playing at t, or nil when nothing was.
func trackAt(ts []Track, t time.Time) *Track {
	var at *Track
	for i := range ts {
		if ts[i].Start.After(t) {
			break
		}
		at = &ts[i]
	}
	if at == nil || (!at.Ended.IsZero() && !at.Ended.After(t)) || t.Sub(at.Start) > time.Hour {
		return nil
	}
	return at
}

// topSongs finds the track playing at each trip's top speed.
func (s *Server) topSongs(ctx context.Context, ts []store.SavedTrip) map[int64]*Track {
	if len(ts) == 0 {
		return nil
	}
	from, to := ts[0].Start, ts[0].End
	for _, t := range ts {
		if t.Start.Before(from) {
			from = t.Start
		}
		if t.End.After(to) {
			to = t.End
		}
	}
	ev, err := s.Store.MediaEvents(ctx, s.Poller.VIN(), from.Add(-2*time.Hour), to)
	if err != nil || len(ev) == 0 {
		return nil
	}
	all := playlist(ev)
	out := map[int64]*Track{}
	var shown []*Track
	for _, t := range ts {
		if !t.TopSpeedAt.IsZero() {
			if at := trackAt(all, t.TopSpeedAt); at != nil {
				out[t.ID] = at
				shown = append(shown, at)
			}
		}
	}
	s.coverArt(ctx, shown...)
	return out
}

// gauge is a speedometer arc filled to v of max, coloured along the speed scale.
func gauge(v, max float64) template.HTML {
	f := math.Max(0, math.Min(1, v/max))
	// A 270° arc, open at the bottom; pathLength lets the dash pattern use percent.
	const arc = `M 25.86 74.14 A 34 34 0 1 1 74.14 74.14`
	var stops strings.Builder
	for _, st := range speedStops {
		fmt.Fprintf(&stops, `<stop offset="%.3f" stop-color="%s"/>`, math.Min(1, st.Kmh/max), hexColor(st.Color))
	}
	return template.HTML(fmt.Sprintf(`<svg class="gauge" viewBox="0 0 100 100" aria-hidden="true">`+
		`<defs><linearGradient id="gauge-grad" x1="0" y1="1" x2="1" y2="1">%s</linearGradient></defs>`+
		`<path d="%s" pathLength="100" class="track"/>`+
		`<path d="%s" pathLength="100" class="fill" stroke="url(#gauge-grad)" stroke-dasharray="%.1f 100"/></svg>`,
		stops.String(), arc, arc, f*100))
}
