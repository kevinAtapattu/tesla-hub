// Package rules decides when to precondition the car and runs those decisions.
package rules

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/tesla"
)

// Window is how long after a rule's time it may still fire, so a restart or a
// slow tick doesn't make it miss the day.
const Window = 10 * time.Minute

type Decision struct {
	Due        bool   // the rule's day and time match now
	SkipReason string // non-empty if due but a condition failed
}

// Evaluate checks a rule against the current local time and the latest known car
// state. It's a pure function so it's easy to test. latest may be nil.
//
// When the car is asleep we use its last known location: it can't have moved
// without waking up, and waking it just to check would cost money.
func Evaluate(r *store.Rule, now time.Time, latest *tesla.Snapshot) Decision {
	if !r.Enabled || !onDay(r.Days, now.Weekday()) {
		return Decision{}
	}
	at, err := time.Parse("15:04", r.AtTime)
	if err != nil {
		return Decision{}
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	if now.Before(start) || !now.Before(start.Add(Window)) {
		return Decision{}
	}

	d := Decision{Due: true}
	if r.Latitude != nil && r.Longitude != nil && r.RadiusM != nil {
		if latest == nil || latest.Latitude == nil || latest.Longitude == nil {
			d.SkipReason = "car location unknown"
			return d
		}
		dist := DistanceM(*r.Latitude, *r.Longitude, *latest.Latitude, *latest.Longitude)
		if dist > *r.RadiusM {
			d.SkipReason = fmt.Sprintf("car is %.1f km from the rule's location", dist/1000)
			return d
		}
	}
	// Unknown outside temperature means we run anyway: better a warm car than a cold one.
	if r.MaxOutsideTempC != nil && latest != nil && latest.OutsideTempC != nil && *latest.OutsideTempC > *r.MaxOutsideTempC {
		d.SkipReason = fmt.Sprintf("outside is %.1f°C, above %.1f°C", *latest.OutsideTempC, *r.MaxOutsideTempC)
	}
	return d
}

func onDay(days string, wd time.Weekday) bool {
	for _, f := range strings.Split(days, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && time.Weekday(n) == wd {
			return true
		}
	}
	return false
}

// DistanceM is the great-circle distance in metres (haversine).
func DistanceM(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusM = 6371000
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := rad(lat2-lat1), rad(lon2-lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(a))
}
