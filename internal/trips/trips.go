// Package trips cuts the car's data into drives and works out their stats:
// distance, speeds, energy, g-forces, 0–100 km/h and the route. It's pure so it
// can be tested with made-up drives; the builder feeds it stored data.
package trips

import (
	"math"
	"time"
)

const gravity = 9.80665

// Sample is the car's state at one moment, with fields carried forward from
// earlier records (telemetry only sends what changed). Nil means never seen.
type Sample struct {
	Time       time.Time
	Gear       string // P, R, N, D or ""
	SpeedKmh   *float64
	Latitude   *float64
	Longitude  *float64
	OdometerKm *float64
	Soc        *float64
	PowerKW    *float64
	LatAccel   *float64 // m/s²
	LongAccel  *float64 // m/s², negative when braking
}

func (s Sample) driving() bool {
	return s.Gear == "D" || s.Gear == "R" || s.Gear == "N" || (s.Gear == "" && s.SpeedKmh != nil && *s.SpeedKmh > 1)
}

// Segment splits samples (oldest first) into drives. A drive ends when the car
// is put in Park or the data stops for longer than gap (it went to sleep or lost signal).
func Segment(samples []Sample, gap time.Duration) [][]Sample {
	var out [][]Sample
	var cur []Sample
	flush := func() {
		if len(cur) >= 2 {
			out = append(out, cur)
		}
		cur = nil
	}
	for _, s := range samples {
		if len(cur) > 0 && s.Time.Sub(cur[len(cur)-1].Time) > gap {
			flush()
		}
		if s.driving() {
			cur = append(cur, s)
		} else if len(cur) > 0 {
			cur = append(cur, s) // include the stop so the trip ends where the car parked
			flush()
		}
	}
	flush()
	return out
}

// Trip is one drive's summary.
type Trip struct {
	Start, End         time.Time
	DistanceKm         float64
	Duration           time.Duration
	Moving             time.Duration
	MaxSpeedKmh        float64
	AvgSpeedKmh        float64 // over moving time
	EnergyKWh          float64 // drawn from the battery
	RegenKWh           float64 // put back by regenerative braking
	SocStart, SocEnd   *float64
	PeakPowerKW        float64
	PeakRegenKW        float64
	MaxLatG            float64
	MaxAccelG          float64
	MaxBrakeG          float64
	ZeroTo100          *time.Duration // fastest 0–100 km/h run, if there was one
	StartLat, StartLon *float64
	EndLat, EndLon     *float64
	Path               []PathPoint
	Samples            int
}

// PathPoint is a point on the route with the speed there, for colouring the line.
type PathPoint struct {
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	Speed float64 `json:"s"`
}

// WhPerKm is the trip's net energy use per kilometre, or 0 without enough data.
func (t Trip) WhPerKm() float64 {
	if t.DistanceKm < 0.5 || t.EnergyKWh == 0 {
		return 0
	}
	return (t.EnergyKWh - t.RegenKWh) * 1000 / t.DistanceKm
}

// Summarize works out a drive's stats. maxPath caps the stored route points.
func Summarize(seg []Sample, maxPath int) Trip {
	t := Trip{Start: seg[0].Time, End: seg[len(seg)-1].Time, Samples: len(seg)}
	t.Duration = t.End.Sub(t.Start)

	var pathKm float64
	var odoStart, odoEnd *float64
	for i, s := range seg {
		if s.OdometerKm != nil {
			if odoStart == nil {
				odoStart = s.OdometerKm
			}
			odoEnd = s.OdometerKm
		}
		if s.Soc != nil {
			if t.SocStart == nil {
				t.SocStart = s.Soc
			}
			t.SocEnd = s.Soc
		}
		if s.Latitude != nil && s.Longitude != nil {
			if t.StartLat == nil {
				t.StartLat, t.StartLon = s.Latitude, s.Longitude
			}
			t.EndLat, t.EndLon = s.Latitude, s.Longitude
		}
		if s.SpeedKmh != nil {
			t.MaxSpeedKmh = math.Max(t.MaxSpeedKmh, *s.SpeedKmh)
		}
		if s.PowerKW != nil {
			t.PeakPowerKW = math.Max(t.PeakPowerKW, *s.PowerKW)
			t.PeakRegenKW = math.Max(t.PeakRegenKW, -*s.PowerKW)
		}
		if s.LatAccel != nil {
			t.MaxLatG = math.Max(t.MaxLatG, math.Abs(*s.LatAccel)/gravity)
		}
		if s.LongAccel != nil {
			t.MaxAccelG = math.Max(t.MaxAccelG, *s.LongAccel/gravity)
			t.MaxBrakeG = math.Max(t.MaxBrakeG, -*s.LongAccel/gravity)
		}
		if i == 0 {
			continue
		}
		prev := seg[i-1]
		dt := s.Time.Sub(prev.Time)
		// Time and energy between samples use the earlier sample's values, which
		// is what the car was doing until it reported a change.
		if prev.SpeedKmh != nil && *prev.SpeedKmh > 1 {
			t.Moving += dt
		}
		if prev.PowerKW != nil && dt < 2*time.Minute {
			kwh := *prev.PowerKW * dt.Hours()
			if kwh > 0 {
				t.EnergyKWh += kwh
			} else {
				t.RegenKWh -= kwh
			}
		}
		if prev.Latitude != nil && s.Latitude != nil {
			pathKm += haversineKm(*prev.Latitude, *prev.Longitude, *s.Latitude, *s.Longitude)
		}
	}
	// The odometer is the most reliable distance; the GPS path fills in when it's missing.
	if odoStart != nil && odoEnd != nil && *odoEnd-*odoStart > 0 {
		t.DistanceKm = *odoEnd - *odoStart
	} else {
		t.DistanceKm = pathKm
	}
	if t.Moving > 0 {
		t.AvgSpeedKmh = t.DistanceKm / t.Moving.Hours()
	}
	t.ZeroTo100 = bestRun(seg, 100)
	t.Path = path(seg, maxPath)
	return t
}

// bestRun finds the quickest standing start to target km/h: from the last time
// the car was (almost) stopped to the first sample at or above target, with the
// crossing interpolated between samples. Runs over 20 s aren't launches.
func bestRun(seg []Sample, target float64) *time.Duration {
	var best *time.Duration
	var standing *time.Time
	for i, s := range seg {
		if s.SpeedKmh == nil {
			continue
		}
		v := *s.SpeedKmh
		if v <= 2 {
			t := s.Time
			standing = &t
			continue
		}
		if standing == nil || v < target {
			continue
		}
		crossed := s.Time
		if i > 0 && seg[i-1].SpeedKmh != nil && *seg[i-1].SpeedKmh < target {
			pv, pt := *seg[i-1].SpeedKmh, seg[i-1].Time
			frac := (target - pv) / (v - pv)
			crossed = pt.Add(time.Duration(frac * float64(s.Time.Sub(pt))))
		}
		d := crossed.Sub(*standing)
		if d > 0 && d <= 20*time.Second && (best == nil || d < *best) {
			best = &d
		}
		standing = nil
	}
	return best
}

// path keeps at most max points, evenly spread, always including both ends.
func path(seg []Sample, max int) []PathPoint {
	var all []PathPoint
	for _, s := range seg {
		if s.Latitude == nil || s.Longitude == nil {
			continue
		}
		p := PathPoint{Lat: *s.Latitude, Lon: *s.Longitude}
		if s.SpeedKmh != nil {
			p.Speed = math.Round(*s.SpeedKmh)
		}
		if n := len(all); n > 0 && all[n-1].Lat == p.Lat && all[n-1].Lon == p.Lon {
			all[n-1].Speed = math.Max(all[n-1].Speed, p.Speed)
			continue
		}
		all = append(all, p)
	}
	if max <= 1 || len(all) <= max {
		return all
	}
	out := make([]PathPoint, 0, max)
	step := float64(len(all)-1) / float64(max-1)
	for i := 0; i < max; i++ {
		out = append(out, all[int(math.Round(float64(i)*step))])
	}
	return out
}

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}
