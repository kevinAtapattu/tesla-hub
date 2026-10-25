// Package planner decides how to get the cabin comfortable for a departure.
//
// Plan is deliberately a pure function with plain inputs and a human-readable
// Reason: it's what a "leaving the gym in 20 minutes" assistant will call as a tool.
package planner

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const ComfortC = 21.0

// Prefs are the owner's comfort settings. Zero fields fall back to DefaultPrefs.
type Prefs struct {
	WarmC           float64 `json:"warm_c"`             // heat to this
	CoolC           float64 `json:"cool_c"`             // cool to this
	HeatBelowC      float64 `json:"heat_below_c"`       // heat when it's colder than this outside
	CoolAboveC      float64 `json:"cool_above_c"`       // cool when it's warmer than this outside
	SeatHeatBelowC  float64 `json:"seat_heat_below_c"`  // add seat heat below this
	WheelHeatBelowC float64 `json:"wheel_heat_below_c"` // add steering wheel heat below this
}

var DefaultPrefs = Prefs{WarmC: ComfortC, CoolC: ComfortC - 1, HeatBelowC: 10, CoolAboveC: 24, SeatHeatBelowC: 5, WheelHeatBelowC: 0}

// WithDefaults fills unset fields. Thresholds can legitimately be 0, so only the
// setpoints (never 0 in practice) decide whether prefs were saved at all.
func (p Prefs) WithDefaults() Prefs {
	if p.WarmC == 0 && p.CoolC == 0 {
		return DefaultPrefs
	}
	return p
}

type Input struct {
	Now           time.Time
	Departure     time.Time // when you'll get in the car
	CabinC        *float64  // car's interior sensor (may be stale or nil)
	OutsideC      *float64  // prefer the weather service: the car's sensor is stale while it sleeps
	Precipitating bool      // rain/snow now or soon: allow extra time to clear the glass
	Prefs         Prefs

	// What the owner asked for. Want is Heat, Cool or "" (decide from the weather).
	// The weather still wins when the request makes no sense: "warm it up" on a 28°
	// day cools instead, and the Reason says so.
	Want         Action
	TempC        *float64 // explicit setpoint, clamped to what the car accepts
	SeatHeat     *bool
	SteeringHeat *bool
}

type Action string

const (
	Heat Action = "heat"
	Cool Action = "cool"
	None Action = "none"
)

type Plan struct {
	Action       Action
	TempC        float64
	SeatHeat     bool
	SteeringHeat bool
	StartAt      time.Time // when to switch climate on (never before Now)
	Lead         time.Duration
	Reason       string
}

func Make(in Input) Plan {
	pr := in.Prefs.WithDefaults()
	p := Plan{Action: None, TempC: pr.WarmC}
	var why []string
	if in.OutsideC != nil {
		why = append(why, fmt.Sprintf("it's %.0f° outside", *in.OutsideC))
	}
	if in.CabinC != nil {
		why = append(why, fmt.Sprintf("%.0f° in the cabin", *in.CabinC))
	}

	outside, cabin := in.OutsideC, in.CabinC
	cold := (outside != nil && *outside < pr.HeatBelowC) || (cabin != nil && *cabin < 15)
	hot := (outside != nil && *outside > pr.CoolAboveC) || (cabin != nil && *cabin > 28)
	switch {
	case outside == nil && cabin == nil:
		p.Action = Heat // no data: set a comfortable temperature and let the car decide
		why = append(why, "no temperature data")
		if in.Want == Cool {
			p.Action, p.TempC = Cool, pr.CoolC
		}
	case cold && in.Want != Cool, in.Want == Heat && !hot:
		p.Action = Heat
		if outside != nil && *outside < 0 && pr.WarmC < 28 {
			p.TempC = pr.WarmC + 1
		}
		p.SeatHeat = outside == nil || *outside < pr.SeatHeatBelowC
		p.SteeringHeat = outside != nil && *outside < pr.WheelHeatBelowC
	case hot, in.Want == Cool && !cold:
		p.Action, p.TempC = Cool, pr.CoolC
	}
	instead := ""
	switch {
	case in.Want == Heat && p.Action == Cool:
		instead = " instead of heating"
	case in.Want == Cool && cold:
		p.Action = None
	}
	if in.TempC != nil && p.Action != None {
		p.TempC = math.Min(math.Max(*in.TempC, 15), 28)
	}
	if in.SeatHeat != nil && p.Action == Heat {
		p.SeatHeat = *in.SeatHeat
	}
	if in.SteeringHeat != nil && p.Action == Heat {
		p.SteeringHeat = *in.SteeringHeat
	}

	if p.Action == None {
		p.StartAt = in.Departure
		p.Reason = capitalize(strings.Join(why, " and ")) + ". That's comfortable, so no climate needed."
		if in.Want == Cool {
			p.Reason = capitalize(strings.Join(why, " and ")) + ". No cooling needed in this weather."
		}
		return p
	}

	// Lead time: roughly 45 s per degree of change, 5–25 min, plus 5 min to clear
	// windows when it's below freezing and wet.
	ref := p.TempC - 8 // assume a cold-ish cabin when we don't know
	if cabin != nil {
		ref = *cabin
	} else if outside != nil {
		ref = *outside
	}
	mins := math.Abs(p.TempC-ref) * 0.75
	mins = math.Min(math.Max(mins, 5), 25)
	if in.Precipitating && outside != nil && *outside < 2 {
		mins += 5
	}
	p.Lead = time.Duration(math.Round(mins)) * time.Minute
	p.StartAt = in.Departure.Add(-p.Lead)
	if p.StartAt.Before(in.Now) {
		p.StartAt = in.Now
	}

	verb := map[Action]string{Heat: "Heating", Cool: "Cooling"}[p.Action]
	extras := ""
	switch {
	case p.SeatHeat && p.SteeringHeat:
		extras = " with seat and steering wheel heat"
	case p.SeatHeat:
		extras = " with seat heat"
	}
	when := "now"
	if p.StartAt.Sub(in.Now) > time.Minute {
		when = "at " + p.StartAt.Format("3:04 PM")
	}
	p.Reason = fmt.Sprintf("%s. %s to %s°%s%s, starting %s.", capitalize(strings.Join(why, " and ")), verb,
		strings.TrimSuffix(fmt.Sprintf("%.1f", p.TempC), ".0"), extras, instead, when)
	return p
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
