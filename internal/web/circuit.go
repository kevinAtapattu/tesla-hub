package web

import (
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/trips"
)

// TripBoard is a drive's race-report extras: the route drawn as a circuit with
// three sectors, 1–5 ratings against the other drives, and where it ranks.
type TripBoard struct {
	Number    int // the drive's place in the history, 1 for the first
	Circuit   template.HTML
	Sectors   []Sector
	Ratings   []Rating
	Basis     int // telemetry drives the ratings compare against
	Compounds []Compound
	Ranks     []Rank
}

// Sector is a third of the route by distance.
type Sector struct {
	N      int
	Km     float64
	Time   time.Duration // 0 when the route has no timestamps (polled drives)
	AvgKmh float64
	TopKmh float64
	Class  string // purple for the fastest sector, then green, then yellow
}

// Rating is a stat scored 1–5 against your other drives; Score 0 means no data.
type Rating struct {
	Label, Value string
	Score        int
}

// Compound sums up how hard the drive was, as a tyre: soft is flat out.
type Compound struct {
	Code, Name, Note, Class string
	On                      bool
}

// Rank is where one stat places among your drives, with recent drives as bars.
type Rank struct {
	Label, Value, Unit string
	Pos, Of            int
	Bars               template.HTML
}

// circuitPoint is a point on the route; T is zero without timestamps.
type circuitPoint struct {
	Lat, Lon, Kmh float64
	T             time.Time
}

// circuitPoints prefers the 1 Hz samples, whose times give sector times, and
// falls back to the stored route.
func circuitPoints(path []trips.PathPoint, samples []trips.Sample) []circuitPoint {
	var out []circuitPoint
	for _, s := range samples {
		if s.Latitude == nil || s.Longitude == nil {
			continue
		}
		p := circuitPoint{Lat: *s.Latitude, Lon: *s.Longitude, T: s.Time}
		if s.SpeedKmh != nil {
			p.Kmh = *s.SpeedKmh
		}
		out = append(out, p)
	}
	if len(out) >= 2 {
		return out
	}
	out = out[:0]
	for _, p := range path {
		out = append(out, circuitPoint{Lat: p.Lat, Lon: p.Lon, Kmh: p.Speed})
	}
	return out
}

func haversineKm(a, b circuitPoint) float64 {
	const r = 6371.0
	la1, la2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dla, dlo := la2-la1, (b.Lon-a.Lon)*math.Pi/180
	h := math.Sin(dla/2)*math.Sin(dla/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dlo/2)*math.Sin(dlo/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

// sectorBounds splits the points into three runs of equal distance; each run
// shares its first point with the previous one's last so the lines join.
func sectorBounds(pts []circuitPoint) [][]circuitPoint {
	if len(pts) < 4 {
		return nil
	}
	cum := make([]float64, len(pts))
	for i := 1; i < len(pts); i++ {
		cum[i] = cum[i-1] + haversineKm(pts[i-1], pts[i])
	}
	total := cum[len(cum)-1]
	if total < 0.1 {
		return nil
	}
	var out [][]circuitPoint
	start := 0
	for k := 1; k <= 3; k++ {
		end := len(pts) - 1
		if k < 3 {
			end = sort.SearchFloat64s(cum, total*float64(k)/3)
			end = max(end, start+1)
			end = min(end, len(pts)-1)
		}
		out = append(out, pts[start:end+1])
		start = end
	}
	return out
}

func sectors(pts []circuitPoint) []Sector {
	var out []Sector
	for i, run := range sectorBounds(pts) {
		s := Sector{N: i + 1}
		for j, p := range run {
			s.TopKmh = math.Max(s.TopKmh, p.Kmh)
			if j > 0 {
				s.Km += haversineKm(run[j-1], p)
			}
		}
		if a, z := run[0].T, run[len(run)-1].T; !a.IsZero() && z.After(a) {
			s.Time = z.Sub(a)
			s.AvgKmh = s.Km / s.Time.Hours()
		}
		out = append(out, s)
	}
	// F1 colours: purple for the quickest sector, green next, yellow slowest.
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	pace := func(s Sector) float64 {
		if s.AvgKmh > 0 {
			return s.AvgKmh
		}
		return s.TopKmh
	}
	sort.SliceStable(order, func(a, b int) bool { return pace(out[order[a]]) > pace(out[order[b]]) })
	for rank, i := range order {
		out[i].Class = []string{"purple", "green", "yellow"}[min(rank, 2)]
	}
	return out
}

// circuitSVG draws the route as a race-track outline: a white line over a
// dropped shadow, one path per sector, sector labels, the start flag and a
// marker where the drive hit its top speed.
func circuitSVG(pts []circuitPoint, secs []Sector) template.HTML {
	runs := sectorBounds(pts)
	if len(runs) == 0 {
		return ""
	}
	const w, h, pad = 360.0, 240.0, 30.0
	minX, maxX, minY, maxY := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	k := math.Cos(pts[0].Lat * math.Pi / 180)
	proj := func(p circuitPoint) (float64, float64) { return p.Lon * k, -p.Lat }
	for _, p := range pts {
		x, y := proj(p)
		minX, maxX, minY, maxY = math.Min(minX, x), math.Max(maxX, x), math.Min(minY, y), math.Max(maxY, y)
	}
	scale := math.Min((w-2*pad)/math.Max(maxX-minX, 1e-9), (h-2*pad)/math.Max(maxY-minY, 1e-9))
	ox := (w - (maxX-minX)*scale) / 2
	oy := (h - (maxY-minY)*scale) / 2
	xy := func(p circuitPoint) (float64, float64) {
		x, y := proj(p)
		return ox + (x-minX)*scale, oy + (y-minY)*scale
	}
	line := func(run []circuitPoint) string {
		var b strings.Builder
		step := max(1, len(run)/150) // plenty for a 360px drawing
		for i := 0; i < len(run); i += step {
			x, y := xy(run[i])
			fmt.Fprintf(&b, "%s%.1f %.1f", map[bool]string{true: "M", false: " L"}[i == 0], x, y)
		}
		x, y := xy(run[len(run)-1])
		fmt.Fprintf(&b, " L%.1f %.1f", x, y)
		return b.String()
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="circuit" viewBox="0 0 %.0f %.0f" role="img" aria-label="Route drawn as a circuit in three sectors">`, w, h)
	all := line(pts)
	fmt.Fprintf(&b, `<path class="c-shadow" d="%s" transform="translate(5 6)"/>`, all)
	fmt.Fprintf(&b, `<path class="c-edge" d="%s"/>`, all)
	for i, run := range runs {
		fmt.Fprintf(&b, `<path class="c-sec" data-s="%d" d="%s" pathLength="100" style="animation-delay:%.2fs"/>`, i+1, line(run), float64(i)*0.55)
	}
	// labels sit off the line at each sector's halfway point, pushed away from the middle
	cx, cy := w/2, h/2
	for i, run := range runs {
		x, y := xy(run[len(run)/2])
		dx, dy := x-cx, y-cy
		if n := math.Hypot(dx, dy); n > 1 {
			dx, dy = dx/n, dy/n
		} else {
			dx, dy = 0, -1
		}
		lx := math.Max(22, math.Min(w-22, x+dx*20))
		ly := math.Max(12, math.Min(h-12, y+dy*20))
		cls := ""
		if i < len(secs) {
			cls = secs[i].Class
		}
		fmt.Fprintf(&b, `<g class="c-label %s" data-s="%d"><rect x="%.1f" y="%.1f" width="40" height="16" rx="3"/><text x="%.1f" y="%.1f">SECTOR %d</text></g>`,
			cls, i+1, lx-20, ly-8, lx, ly+3.5, i+1)
	}
	for _, run := range runs[1:] { // sector lines
		x, y := xy(run[0])
		fmt.Fprintf(&b, `<circle class="c-split" cx="%.1f" cy="%.1f" r="4"/>`, x, y)
	}
	// chequered flag at the start
	sx, sy := xy(pts[0])
	fmt.Fprintf(&b, `<g class="c-flag" transform="translate(%.1f %.1f)"><line x1="0" y1="0" x2="0" y2="-16"/>`, sx, sy)
	for r := 0; r < 2; r++ {
		for c := 0; c < 3; c++ {
			cls := "w"
			if (r+c)%2 == 1 {
				cls = "k"
			}
			fmt.Fprintf(&b, `<rect class="%s" x="%d" y="%d" width="4" height="4"/>`, cls, c*4, -16+r*4)
		}
	}
	b.WriteString(`</g>`)
	top := 0
	for i, p := range pts {
		if p.Kmh > pts[top].Kmh {
			top = i
		}
	}
	if pts[top].Kmh > 0 {
		x, y := xy(pts[top])
		fmt.Fprintf(&b, `<g class="c-top"><circle class="ping" cx="%.1f" cy="%.1f" r="6"/><circle cx="%.1f" cy="%.1f" r="4.5"/>`+
			`<text x="%.1f" y="%.1f">%.0f km/h</text></g>`, x, y, x, y, x, math.Max(14, y-10), pts[top].Kmh)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// percentile is the share of others below v (ties count half), so 0.5 is a
// typical drive.
func percentile(v float64, others []float64) float64 {
	if len(others) == 0 {
		return 0.5
	}
	var below float64
	for _, o := range others {
		switch {
		case o < v:
			below++
		case o == v:
			below += 0.5
		}
	}
	return below / float64(len(others))
}

// score turns a percentile into 1–5.
func score(p float64) int { return min(5, 1+int(p*5)) }

type stat struct {
	label, unit string
	digits      int
	tele        bool // needs the 1 Hz stream
	lower       bool // smaller is better
	get         func(trips.Trip) (float64, bool)
}

var (
	statTop    = stat{"Top speed", "km/h", 0, false, false, func(t trips.Trip) (float64, bool) { return t.MaxSpeedKmh, t.MaxSpeedKmh > 0 }}
	statLaunch = stat{"Launch", "g", 2, true, false, func(t trips.Trip) (float64, bool) { return t.MaxAccelG, t.MaxAccelG > 0 }}
	statBrake  = stat{"Braking", "g", 2, true, false, func(t trips.Trip) (float64, bool) { return t.MaxBrakeG, t.MaxBrakeG > 0 }}
	statCorner = stat{"Cornering", "g", 2, true, false, func(t trips.Trip) (float64, bool) { return t.MaxLatG, t.MaxLatG > 0 }}
	statPower  = stat{"Power", "kW", 0, true, false, func(t trips.Trip) (float64, bool) { return t.PeakPowerKW, t.PeakPowerKW > 0 }}
	statG      = stat{"Peak g", "g", 2, true, false, func(t trips.Trip) (float64, bool) { return t.MaxCombinedG, t.MaxCombinedG > 0 }}
	statEff    = stat{"Efficiency", "Wh/km", 0, false, true, func(t trips.Trip) (float64, bool) { return t.WhPerKm(), t.WhPerKm() > 0 }}
	stat0100   = stat{"0–100", "s", 1, true, true, func(t trips.Trip) (float64, bool) {
		if t.ZeroTo100 == nil {
			return 0, false
		}
		return t.ZeroTo100.Seconds(), true
	}}
)

// field is the drives that have this stat, oldest first.
func (st stat) field(all []store.SavedTrip) []store.SavedTrip {
	var out []store.SavedTrip
	for _, t := range all {
		if st.tele && t.Source != "telemetry" {
			continue
		}
		if _, ok := st.get(t.Trip); ok {
			out = append(out, t)
		}
	}
	return out
}

func (st stat) format(v float64) string { return fmt.Sprintf("%.*f", st.digits, v) }

// tripBoard builds the race report for t from every drive's stats (oldest first).
func tripBoard(t *store.SavedTrip, all []store.SavedTrip, pts []circuitPoint) TripBoard {
	var b TripBoard
	for i, o := range all {
		if o.ID == t.ID {
			b.Number = i + 1
		}
		if o.Source == "telemetry" {
			b.Basis++
		}
	}
	b.Sectors = sectors(pts)
	b.Circuit = circuitSVG(pts, b.Sectors)

	var sum, n float64
	for _, st := range []stat{statLaunch, statBrake, statCorner, statPower, statTop, statEff} {
		r := Rating{Label: st.label, Value: "—"}
		v, ok := st.get(t.Trip)
		if ok && (!st.tele || t.Source == "telemetry") {
			var others []float64
			for _, o := range st.field(all) {
				if o.ID != t.ID {
					x, _ := st.get(o.Trip)
					others = append(others, x)
				}
			}
			p := percentile(v, others)
			if st.lower {
				p = 1 - p
			}
			r.Score, r.Value = score(p), st.format(v)+" "+st.unit
			if st.label != statEff.label { // how hard you drove, not how gently
				sum += float64(r.Score)
				n++
			}
		}
		b.Ratings = append(b.Ratings, r)
	}
	if n > 0 && t.Source == "telemetry" { // polled drives only have a top speed to go on
		avg := sum / n
		b.Compounds = []Compound{
			{Code: "C3", Name: "Hard", Note: "Cruise", Class: "hard", On: avg < 2.6},
			{Code: "C4", Name: "Medium", Note: "Spirited", Class: "medium", On: avg >= 2.6 && avg < 3.6},
			{Code: "C5", Name: "Soft", Note: "Attack", Class: "soft", On: avg >= 3.6},
		}
	}

	for _, st := range []stat{statTop, stat0100, statG, statPower} {
		v, ok := st.get(t.Trip)
		if !ok || (st.tele && t.Source != "telemetry") {
			continue
		}
		f := st.field(all)
		r := Rank{Label: st.label, Value: st.format(v), Unit: st.unit, Pos: 1, Of: len(f)}
		for _, o := range f {
			x, _ := st.get(o.Trip)
			if o.ID != t.ID && ((st.lower && x < v) || (!st.lower && x > v)) {
				r.Pos++
			}
		}
		r.Bars = rankBars(f, st, t.ID)
		b.Ranks = append(b.Ranks, r)
	}
	return b
}

// rankBars is an F1-style mini bar chart of up to 24 drives around the one
// highlighted (oldest left). Better results are taller, so for times the
// bars are inverted; every bar links to its drive.
func rankBars(f []store.SavedTrip, st stat, on int64) template.HTML {
	const n, w, h = 24, 240.0, 56.0
	if len(f) == 0 {
		return ""
	}
	at := len(f) - 1
	for i, t := range f {
		if t.ID == on {
			at = i
		}
	}
	start := max(0, min(at-n+6, len(f)-n))
	win := f[start:min(len(f), start+n)]
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, t := range f {
		v, _ := st.get(t.Trip)
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	bw := w / n
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="bars" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s on recent drives">`, w, h+2, st.label)
	for i, t := range win {
		v, _ := st.get(t.Trip)
		f := 1.0
		if hi > lo {
			f = (v - lo) / (hi - lo)
		}
		if st.lower {
			f = 1 - f
		}
		bh := 6 + f*(h-6)
		cls := ""
		if t.ID == on {
			cls = ` class="on"`
		}
		fmt.Fprintf(&b, `<a href="/trips/%d"%s><title>%s · %s %s</title><rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="1.5" style="animation-delay:%dms"/></a>`,
			t.ID, cls, t.Start.Format("2 Jan"), st.format(v), st.unit, float64(i)*bw+bw*0.2, h-bh, bw*0.6, bh, i*25)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// Form is the bests page's F1-style form guide: an average with the recent
// drives as bars.
type Form struct {
	Label, Avg, Unit string
	Bars             template.HTML
}

// Honour is a career count, like wins or podiums.
type Honour struct {
	Label string
	N     int
}

func formGuide(all []store.SavedTrip) []Form {
	var out []Form
	for _, st := range []stat{statTop, stat0100, statG} {
		f := st.field(all)
		if len(f) == 0 {
			continue
		}
		recent := f[max(0, len(f)-24):]
		var sum float64
		for _, t := range recent {
			v, _ := st.get(t.Trip)
			sum += v
		}
		out = append(out, Form{Label: st.label, Avg: st.format(sum / float64(len(recent))), Unit: st.unit,
			Bars: rankBars(f, st, f[len(f)-1].ID)})
	}
	return out
}

// honours counts records broken (a new best 0–100 or top speed when it
// happened), 0–100 runs, and drives past 150 km/h.
func honours(all []store.SavedTrip) []Honour {
	var records, runs, fast int
	var best100 float64
	var top float64
	for _, t := range all {
		if v, ok := stat0100.get(t.Trip); ok && t.Source == "telemetry" {
			runs++
			if best100 == 0 || v < best100 {
				best100 = v
				records++
			}
		}
		if t.MaxSpeedKmh > top {
			top = t.MaxSpeedKmh
			records++
		}
		if t.MaxSpeedKmh >= 150 {
			fast++
		}
	}
	return []Honour{{"Records set", records}, {"0–100 runs", runs}, {"Over 150", fast}, {"Drives", len(all)}}
}
