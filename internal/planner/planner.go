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

type Input struct {
	Now           time.Time
	Departure     time.Time // when you'll get in the car
	CabinC        *float64  // car's interior sensor (may be stale or nil)
	OutsideC      *float64  // prefer the weather service: the car's sensor is stale while it sleeps
	Precipitating bool      // rain/snow now or soon: allow extra time to clear the glass
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
	p := Plan{Action: None, TempC: ComfortC}
	var why []string
	if in.OutsideC != nil {
		why = append(why, fmt.Sprintf("it's %.0f° outside", *in.OutsideC))
	}
	if in.CabinC != nil {
		why = append(why, fmt.Sprintf("%.0f° in the cabin", *in.CabinC))
	}

	outside, cabin := in.OutsideC, in.CabinC
	switch {
	case outside == nil && cabin == nil:
		p.Action = Heat // no data: set a comfortable temperature and let the car decide
		why = append(why, "no temperature data")
	case (outside != nil && *outside < 10) || (cabin != nil && *cabin < 15):
		p.Action = Heat
		if outside != nil && *outside < 0 {
			p.TempC = ComfortC + 1
		}
		p.SeatHeat = outside == nil || *outside < 5
		p.SteeringHeat = outside != nil && *outside < 0
	case (outside != nil && *outside > 24) || (cabin != nil && *cabin > 28):
		p.Action = Cool
		p.TempC = ComfortC - 1
	}

	if p.Action == None {
		p.StartAt = in.Departure
		p.Reason = capitalize(strings.Join(why, " and ")) + ". That's comfortable, so no climate needed."
		return p
	}

	// Lead time: roughly 45 s per degree of change, 5–25 min, plus 5 min to clear
	// windows when it's below freezing and wet.
	ref := ComfortC - 8 // assume a cold-ish cabin when we don't know
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
	p.Reason = fmt.Sprintf("%s. %s to %.0f°%s, starting %s.", capitalize(strings.Join(why, " and ")), verb, p.TempC, extras, when)
	return p
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
