package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/trips"
)

func (s *Server) tripsPage(w http.ResponseWriter, r *http.Request) {
	d := s.data(r.Context(), "")
	d.Page = "trips"
	d.DriverFilter = r.URL.Query().Get("driver")
	if vin := s.Poller.VIN(); vin != "" {
		d.Trips, _ = s.Store.ListTrips(r.Context(), vin, d.DriverFilter, 100)
	}
	d.Drivers, _ = s.Store.Drivers(r.Context())
	d.TripTotals = totals(d.Trips)
	s.previews(&d)
	d.TripSongs = s.topSongs(r.Context(), d.Trips)
	d.Live = s.liveTrip(r)
	d.NowPlaying = s.nowPlaying(r.Context())
	d.Recording = s.recording(r.Context(), d.Live)
	s.render(w, "index.html", d)
}

// previews makes the route previews for the trip list.
func (s *Server) previews(d *pageData) {
	d.TripPreviews = map[int64]template.HTML{}
	d.TripPreviewPaths = map[int64]string{}
	for i := range d.Trips {
		t := &d.Trips[i]
		d.TripPreviews[t.ID] = routeSVG(t.Path) // shown until the map loads, or if it can't
		if b, err := json.Marshal(thin(t.Path, 200)); err == nil {
			d.TripPreviewPaths[t.ID] = string(b)
		}
		t.Path = nil // the page only needs the previews
	}
}

func (s *Server) tripPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := s.Store.GetTrip(r.Context(), id)
	if err != nil || t == nil {
		http.NotFound(w, r)
		return
	}
	d := s.data(r.Context(), "")
	d.Page = "trip"
	d.Trip = t
	d.Drivers, _ = s.Store.Drivers(r.Context())
	var filled []trips.Sample
	if t.Source == "telemetry" {
		samples, _ := s.Store.TripSamples(r.Context(), s.Poller.VIN(), t.Start, t.End)
		filled = trips.Fill(samples)
	}
	s.tripDetail(r.Context(), &d, filled)
	all, _ := s.Store.TripStats(r.Context(), s.Poller.VIN())
	d.Board = tripBoard(t, all, circuitPoints(t.Path, filled), "/trips/")
	d.TripSections, _ = s.Store.Sections(r.Context(), s.Poller.VIN(), t.Start, t.End)
	s.render(w, "index.html", d)
}

// tripDetail fills in the route, charts, g-forces and soundtrack for d.Trip
// from its 1 Hz samples (none for polled drives).
func (s *Server) tripDetail(ctx context.Context, d *pageData, filled []trips.Sample) {
	t := d.Trip
	path, _ := json.Marshal(t.Path)
	d.TripPath = template.JS(path)
	if len(filled) > 0 {
		pts := make([]store.Point, 0, len(filled))
		for _, x := range filled {
			pts = append(pts, store.Point{Time: x.Time, SpeedKmh: x.SpeedKmh, PowerKW: x.PowerKW})
		}
		zero := 0.0
		d.TripSpeed = buildChart(pts, t.Start, t.End, s.Cfg.Location, "", &zero, nil,
			series{Name: "Speed km/h", Class: "s-speed", Value: speedOf, Area: true})
		d.TripPower = buildChart(pts, t.Start, t.End, s.Cfg.Location, "", nil, nil,
			series{Name: "Power kW", Class: "s-inside", Value: func(p store.Point) *float64 { return p.PowerKW }})
		d.TripG = frictionCircle(filled)
		d.TripTracks, d.TopSpeedTrack = s.soundtrack(ctx, t, filled)
	}
}

// livePartial is polled every few seconds by the dashboard while a drive is under way.
func (s *Server) livePartial(w http.ResponseWriter, r *http.Request) {
	s.render(w, "live-drive", s.liveData(r))
}

// liveData is what the live-drive card shows: the drive under way, what's
// playing, and the section being recorded.
func (s *Server) liveData(r *http.Request) pageData {
	d := pageData{Live: s.liveTrip(r), NowPlaying: s.nowPlaying(r.Context())}
	d.Recording = s.recording(r.Context(), d.Live)
	return d
}

func (s *Server) liveTrip(r *http.Request) *trips.Trip {
	if s.Trips == nil || s.Poller.VIN() == "" {
		return nil
	}
	t, _ := s.Trips.Current(r.Context(), s.Poller.VIN(), time.Now())
	return t
}

// overlayLive puts the streamed speed and position over the last poll, which
// can be 20 minutes old when a drive has just started.
func (s *Server) overlayLive(ctx context.Context, d *pageData) {
	if s.Trips == nil || d.Latest == nil {
		return
	}
	t, _ := s.Trips.Current(ctx, d.Status.VIN, time.Now())
	if t == nil || len(t.Path) == 0 {
		return
	}
	snap := *d.Latest
	end := t.Path[len(t.Path)-1]
	speed, lat, lon := end.Speed, end.Lat, end.Lon
	snap.SpeedKmh, snap.Latitude, snap.Longitude = &speed, &lat, &lon
	if !snap.Driving() {
		snap.ShiftState = "D"
	}
	if t.SocEnd != nil {
		n := int(math.Round(*t.SocEnd))
		snap.BatteryLevel = &n
	}
	d.Latest = &snap
	d.Status.State = "online"
}

// frictionCircle plots every lateral/longitudinal g reading as a dot inside
// 0.5 g and 1 g rings: the classic "how hard did I push" racing chart.
func frictionCircle(samples []trips.Sample) template.HTML {
	const size, maxG = 240.0, 1.0
	c := size / 2
	scale := (size/2 - 12) / maxG
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="gcircle" role="img" aria-label="g-force plot">`, size, size)
	for _, r := range []float64{0.25, 0.5, 0.75, 1} {
		fmt.Fprintf(&b, `<circle class="ring" cx="%.0f" cy="%.0f" r="%.1f"/>`, c, c, r*scale)
	}
	fmt.Fprintf(&b, `<line class="ring" x1="%.0f" y1="6" x2="%.0f" y2="%.0f"/><line class="ring" x1="6" y1="%.0f" x2="%.0f" y2="%.0f"/>`, c, c, size-6, c, size-6, c)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f">0.5g</text><text x="%.0f" y="%.1f">1g</text>`, c+4, c-0.5*scale-3, c+4, c-scale+10)
	fmt.Fprintf(&b, `<text x="%.0f" y="14" text-anchor="middle">accel</text><text x="%.0f" y="%.0f" text-anchor="middle">brake</text>`, c, c, size-4)
	last := map[[2]int]bool{}
	for _, s := range samples {
		if s.LatAccel == nil || s.LongAccel == nil {
			continue
		}
		x := c + math.Max(-maxG, math.Min(maxG, *s.LatAccel/9.80665))*scale
		y := c - math.Max(-maxG, math.Min(maxG, *s.LongAccel/9.80665))*scale
		k := [2]int{int(x), int(y)} // one dot per pixel is plenty
		if last[k] {
			continue
		}
		last[k] = true
		fmt.Fprintf(&b, `<circle class="pt" cx="%.1f" cy="%.1f" r="2"/>`, x, y)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// speedStops is the route colour scale: green through city speeds, yellow and
// orange into highway speeds, red from about 115 and darkening to 165+.
var speedStops = []struct {
	Kmh   float64
	Color [3]int
}{
	{0, [3]int{0x22, 0xc5, 0x5e}},
	{60, [3]int{0x22, 0xc5, 0x5e}},
	{78, [3]int{0xea, 0xb3, 0x08}},
	{98, [3]int{0xf9, 0x73, 0x16}},
	{115, [3]int{0xef, 0x44, 0x44}},
	{135, [3]int{0xdc, 0x26, 0x26}},
	{150, [3]int{0xb9, 0x1c, 0x1c}},
	{165, [3]int{0x6b, 0x10, 0x10}},
}

// speedColor blends between the stops so the colour changes smoothly with speed.
func speedColor(kmh float64) string {
	stops := speedStops
	if kmh <= stops[0].Kmh {
		return hexColor(stops[0].Color)
	}
	for i := 1; i < len(stops); i++ {
		a, b := stops[i-1], stops[i]
		if kmh <= b.Kmh {
			f := (kmh - a.Kmh) / (b.Kmh - a.Kmh)
			var c [3]int
			for j := range c {
				c[j] = int(math.Round(float64(a.Color[j]) + f*float64(b.Color[j]-a.Color[j])))
			}
			return hexColor(c)
		}
	}
	return hexColor(stops[len(stops)-1].Color)
}

func hexColor(c [3]int) string { return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2]) }

// speedStopsJS gives the maps' scripts the same scale as [[km/h, [r,g,b]], ...].
func speedStopsJS() template.JS {
	var b strings.Builder
	b.WriteString("[")
	for i, s := range speedStops {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "[%g,[%d,%d,%d]]", s.Kmh, s.Color[0], s.Color[1], s.Color[2])
	}
	b.WriteString("]")
	return template.JS(b.String())
}

// speedGradient is the scale as a CSS gradient for the map legend.
func speedGradient() template.CSS {
	max := speedStops[len(speedStops)-1].Kmh
	parts := make([]string, len(speedStops))
	for i, s := range speedStops {
		parts[i] = fmt.Sprintf("%s %.0f%%", hexColor(s.Color), s.Kmh/max*100)
	}
	return template.CSS("linear-gradient(90deg, " + strings.Join(parts, ", ") + ")")
}

// routeSVG draws a trip's route coloured by speed as a small preview, with no
// map tiles so a long list of trips stays light.
func routeSVG(path []trips.PathPoint) template.HTML {
	const w, h, pad = 200.0, 120.0, 10.0
	if len(path) < 2 {
		return template.HTML(`<svg class="route-preview" viewBox="0 0 200 120" aria-hidden="true"></svg>`)
	}
	// Equirectangular projection, squashed by cos(latitude) so shapes aren't stretched.
	k := math.Cos(path[0].Lat * math.Pi / 180)
	minX, maxX, minY, maxY := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for _, p := range path {
		x, y := p.Lon*k, p.Lat
		minX, maxX, minY, maxY = math.Min(minX, x), math.Max(maxX, x), math.Min(minY, y), math.Max(maxY, y)
	}
	span := math.Max(math.Max(maxX-minX, maxY-minY), 1e-6)
	scale := math.Min((w-2*pad)/math.Max(maxX-minX, span*0.05), (h-2*pad)/math.Max(maxY-minY, span*0.05))
	offX := (w - (maxX-minX)*scale) / 2
	offY := (h - (maxY-minY)*scale) / 2
	pt := func(p trips.PathPoint) (float64, float64) {
		return offX + (p.Lon*k-minX)*scale, h - offY - (p.Lat-minY)*scale
	}

	// Short trips keep every point; long ones are thinned so the SVG stays small.
	step := max(1, len(path)/250)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="route-preview" viewBox="0 0 %.0f %.0f" aria-hidden="true">`, w, h)
	// Runs of the same colour become one polyline.
	color := ""
	var pts strings.Builder
	flush := func() {
		if pts.Len() > 0 {
			fmt.Fprintf(&b, `<polyline points="%s" stroke="%s"/>`, strings.TrimSpace(pts.String()), color)
		}
		pts.Reset()
	}
	for i := 0; i < len(path); i += step {
		if i > 0 && i+step >= len(path) {
			i = len(path) - 1
		}
		c := speedColor(path[i].Speed)
		x, y := pt(path[i])
		if c != color && i > 0 {
			fmt.Fprintf(&pts, " %.1f,%.1f", x, y) // close the previous run where the new one starts
			flush()
			px, py := pt(path[max(0, i-step)])
			fmt.Fprintf(&pts, "%.1f,%.1f", px, py)
		}
		color = c
		fmt.Fprintf(&pts, " %.1f,%.1f", x, y)
	}
	flush()
	sx, sy := pt(path[0])
	ex, ey := pt(path[len(path)-1])
	fmt.Fprintf(&b, `<circle class="start" cx="%.1f" cy="%.1f" r="4"/><circle class="end" cx="%.1f" cy="%.1f" r="4"/></svg>`, sx, sy, ex, ey)
	return template.HTML(b.String())
}

// thin keeps at most n evenly spaced points, always including both ends.
func thin(path []trips.PathPoint, n int) []trips.PathPoint {
	if len(path) <= n {
		return path
	}
	out := make([]trips.PathPoint, 0, n)
	step := float64(len(path)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out = append(out, path[int(math.Round(float64(i)*step))])
	}
	return out
}

// TripTotals sums up a set of trips (all of them, or one driver's).
type TripTotals struct {
	Trips       int
	DistanceKm  float64
	Hours       float64
	TopSpeedKmh float64
	AvgSpeedKmh float64
	WhPerKm     float64
	ZeroTo100   *time.Duration
	MaxBrakeG   float64
}

func totals(ts []store.SavedTrip) TripTotals {
	var t TripTotals
	var moving time.Duration
	var energy, effKm float64
	for _, x := range ts {
		t.Trips++
		t.DistanceKm += x.DistanceKm
		t.Hours += x.Duration.Hours()
		moving += x.Moving
		t.TopSpeedKmh = math.Max(t.TopSpeedKmh, x.MaxSpeedKmh)
		t.MaxBrakeG = math.Max(t.MaxBrakeG, x.MaxBrakeG)
		if x.ZeroTo100 != nil && (t.ZeroTo100 == nil || *x.ZeroTo100 < *t.ZeroTo100) {
			t.ZeroTo100 = x.ZeroTo100
		}
		if x.WhPerKm() > 0 {
			energy += x.WhPerKm() * x.DistanceKm
			effKm += x.DistanceKm
		}
	}
	if moving > 0 {
		t.AvgSpeedKmh = t.DistanceKm / moving.Hours()
	}
	if effKm > 0 {
		t.WhPerKm = energy / effKm
	}
	return t
}

// setTripDriver is the "Who drove?" control on a trip page.
func (s *Server) setTripDriver(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name := strings.TrimSpace(r.FormValue("driver"))
	if len(name) > 40 {
		name = name[:40]
	}
	if err := s.Store.SetTripDriver(r.Context(), id, name); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/trips/%d?error=%s", id, urlq(err.Error())), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/trips/%d?flash=%s", id, urlq("Driver saved")), http.StatusSeeOther)
}

// driverCheckIn is called by each driver's iPhone automation when the phone
// connects to the car, e.g. POST {"name": "Kevin"}. The next trip gets that name.
func (s *Server) driverCheckIn(w http.ResponseWriter, r *http.Request) {
	if !s.phoneAuthorized(w, r) {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
	name := strings.TrimSpace(string(body))
	var v struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(body, &v) == nil && v.Name != "" {
		name = strings.TrimSpace(v.Name)
	} else if strings.HasPrefix(name, "name=") {
		name = strings.TrimSpace(strings.TrimPrefix(name, "name="))
	}
	if name == "" || len(name) > 40 {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `Send the driver's name, e.g. {"name": "Kevin"}.`)
		return
	}
	if err := s.Store.DriverCheckIn(r.Context(), name); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "Couldn't save: "+err.Error())
		return
	}
	fmt.Fprintf(w, "Got it, %s is driving.", name)
}
