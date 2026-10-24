package web

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
)

// series picks one value out of each sample; nil means "no reading".
type series struct {
	Name  string
	Class string // CSS class that sets the stroke colour
	Value func(store.Point) *float64
}

type chart struct {
	SVG     template.HTML
	Top     string // label for the top of the y-axis
	Bottom  string
	XLabels []string
	Legend  []series
	HasData bool
}

// gap is the longest silence drawn as a continuous line. Longer gaps (the car
// was asleep) break the line instead of drawing a misleading straight segment.
const gap = 90 * time.Minute

// buildChart draws series as an SVG line chart. It stretches to its container
// (preserveAspectRatio="none" with non-scaling strokes), so labels live in HTML.
func buildChart(pts []store.Point, from, to time.Time, loc *time.Location, unit string, fixedMin, fixedMax *float64, ss ...series) chart {
	const w, h, pad = 1000.0, 200.0, 10.0
	c := chart{Legend: ss}

	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		for _, s := range ss {
			if v := s.Value(p); v != nil {
				lo, hi = math.Min(lo, *v), math.Max(hi, *v)
			}
		}
	}
	if math.IsInf(lo, 1) {
		return c
	}
	c.HasData = true
	if fixedMin != nil {
		lo = *fixedMin
	}
	if fixedMax != nil {
		hi = *fixedMax
	}
	if hi-lo < 4 { // keep flat lines from filling the whole height with noise
		mid := (hi + lo) / 2
		lo, hi = mid-2, mid+2
	}
	c.Top, c.Bottom = fmt.Sprintf("%.0f%s", hi, unit), fmt.Sprintf("%.0f%s", lo, unit)

	span := to.Sub(from).Seconds()
	x := func(t time.Time) float64 { return t.Sub(from).Seconds() / span * w }
	y := func(v float64) float64 { return pad + (hi-v)/(hi-lo)*(h-2*pad) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" aria-hidden="true">`, w, h)
	for i := 1; i < 4; i++ { // faint horizontal guides
		gy := pad + float64(i)*(h-2*pad)/4
		fmt.Fprintf(&b, `<line class="grid" x1="0" x2="%.0f" y1="%.1f" y2="%.1f"/>`, w, gy, gy)
	}
	for _, s := range ss {
		var d strings.Builder
		var last time.Time
		run := 0
		for _, p := range pts {
			v := s.Value(p)
			if v == nil {
				continue
			}
			if run > 0 && p.Time.Sub(last) > gap {
				if run == 1 {
					d.WriteString(" l0.1 0") // a lone sample still shows as a dot
				}
				run = 0
			}
			cmd := "L"
			if run == 0 {
				cmd = " M"
			}
			fmt.Fprintf(&d, "%s%.1f %.1f", cmd, x(p.Time), y(*v))
			last = p.Time
			run++
		}
		if run == 1 {
			d.WriteString(" l0.1 0")
		}
		fmt.Fprintf(&b, `<path class="%s" d="%s"/>`, s.Class, strings.TrimSpace(d.String()))
	}
	b.WriteString(`</svg>`)
	c.SVG = template.HTML(b.String())

	layout := "3 PM"
	if to.Sub(from) > 36*time.Hour {
		layout = "Mon"
	}
	for i := 0; i <= 4; i++ {
		t := from.Add(time.Duration(float64(to.Sub(from)) * float64(i) / 4))
		c.XLabels = append(c.XLabels, t.In(loc).Format(layout))
	}
	return c
}

func batteryOf(p store.Point) *float64 {
	if p.BatteryLevel == nil {
		return nil
	}
	v := float64(*p.BatteryLevel)
	return &v
}
func insideOf(p store.Point) *float64  { return p.InsideTempC }
func outsideOf(p store.Point) *float64 { return p.OutsideTempC }
func speedOf(p store.Point) *float64   { return p.SpeedKmh }
