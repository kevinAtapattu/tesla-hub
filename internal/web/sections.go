package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/trips"
)

// Sections are parts of a drive the owner records on purpose: "record from
// now" on a twisty road, "stop" at the end, and that stretch gets its own
// report without the drive there and back. The open section lives in the
// database, so restarting the hub mid-drive doesn't lose it.

// Recording is the section being recorded, for the live card.
type Recording struct {
	ID    int64
	Start time.Time
}

// recording is the open section, or nil. A section left open after the drive
// ended is closed at the drive's end.
func (s *Server) recording(ctx context.Context, live *trips.Trip) *Recording {
	vin := s.Poller.VIN()
	if vin == "" {
		return nil
	}
	open, err := s.Store.OpenSection(ctx, vin)
	if err != nil || open == nil {
		return nil
	}
	if live == nil && time.Since(open.Start) > 3*time.Minute && s.closeForgotten(ctx, open) {
		return nil
	}
	return &Recording{ID: open.ID, Start: open.Start}
}

// closeForgotten ends an open section once its drive has been saved, at the
// drive's end, or after half a day with no drive at all.
func (s *Server) closeForgotten(ctx context.Context, open *store.Section) bool {
	end := time.Now()
	t, err := s.Store.TripOverlapping(ctx, s.Poller.VIN(), open.Start, end)
	switch {
	case err != nil:
		return false
	case t != nil && t.End.After(open.Start):
		end = t.End
	case time.Since(open.Start) < 12*time.Hour:
		return false // the drive hasn't been saved yet
	}
	_, err = s.finishSection(ctx, open, end)
	return err == nil
}

// sectionSummary works out a section's stats up to end, with the samples it
// used (for the charts). ok is false when there's no data in it.
func (s *Server) sectionSummary(ctx context.Context, sec *store.Section, end time.Time) (trips.Trip, []trips.Sample, bool) {
	raw, err := s.Store.TripSamples(ctx, s.Poller.VIN(), sec.Start.Add(-trips.SectionLead), end)
	if err != nil {
		return trips.Trip{}, nil, false
	}
	t, ok := trips.Section(raw, sec.Start, end)
	var cut []trips.Sample
	for _, x := range trips.Fill(raw) {
		if !x.Time.Before(sec.Start) && !x.Time.After(end) {
			cut = append(cut, x)
		}
	}
	return t, cut, ok
}

func (s *Server) finishSection(ctx context.Context, sec *store.Section, end time.Time) (*store.Section, error) {
	t, _, ok := s.sectionSummary(ctx, sec, end)
	var p *trips.Trip
	if ok {
		p = &t
	}
	if err := s.Store.EndSection(ctx, sec.ID, end, p); err != nil {
		return nil, err
	}
	sec.End, sec.Trip = end, p
	return sec, nil
}

// startSection is the live card's record button.
func (s *Server) startSection(w http.ResponseWriter, r *http.Request) {
	if vin := s.Poller.VIN(); vin != "" {
		s.Store.StartSection(r.Context(), vin, time.Now())
	}
	s.render(w, "live-drive", s.liveData(r))
}

// stopSection is the live card's stop button; it opens the section's report.
func (s *Server) stopSection(w http.ResponseWriter, r *http.Request) {
	open, _ := s.Store.OpenSection(r.Context(), s.Poller.VIN())
	if open != nil {
		if sec, err := s.finishSection(r.Context(), open, time.Now()); err == nil {
			w.Header().Set("HX-Redirect", fmt.Sprintf("/sections/%d", sec.ID))
		}
	}
	s.render(w, "live-drive", s.liveData(r))
}

// sectionShortcut is for an iPhone Shortcut or Siri, so recording needs no
// screen while driving. POST "start", "stop", or nothing to toggle; the reply
// is a sentence to show or speak.
func (s *Server) sectionShortcut(w http.ResponseWriter, r *http.Request) {
	if !s.phoneAuthorized(w, r) {
		return
	}
	vin := s.Poller.VIN()
	if vin == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "The hub isn't connected to the car yet.")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 256))
	want := strings.ToLower(strings.Trim(strings.TrimSpace(string(body)), `"{}`))
	now := time.Now()
	open, err := s.Store.OpenSection(r.Context(), vin)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "Couldn't reach the database.")
		return
	}
	switch {
	case open == nil && strings.Contains(want, "stop"):
		io.WriteString(w, "Nothing is being recorded.")
	case open == nil:
		if _, err := s.Store.StartSection(r.Context(), vin, now); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "Couldn't start recording.")
			return
		}
		io.WriteString(w, "Recording. Run this again to stop.")
	case strings.Contains(want, "start"):
		fmt.Fprintf(w, "Already recording, for %s so far.", spoken(now.Sub(open.Start)))
	default:
		sec, err := s.finishSection(r.Context(), open, now)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "Couldn't save the section.")
			return
		}
		io.WriteString(w, sectionSentence(sec))
	}
}

// sectionSentence sums up a finished section for Siri to read out.
func sectionSentence(sec *store.Section) string {
	t := sec.Trip
	if t == nil {
		return "Stopped. The car didn't send any data during that section."
	}
	out := fmt.Sprintf("Saved: %.1f km in %s, top speed %.0f km/h", t.DistanceKm, spoken(t.Duration), t.MaxSpeedKmh)
	if t.ZeroTo100 != nil {
		out += fmt.Sprintf(", 0 to 100 in %.1f seconds", t.ZeroTo100.Seconds())
	}
	if t.MaxLatG > 0 {
		out += fmt.Sprintf(", %.2f g cornering", t.MaxLatG)
	}
	return out + "."
}

func spoken(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
	m := int(d.Round(time.Minute).Minutes())
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

func (s *Server) sectionPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sec, err := s.Store.GetSection(ctx, id)
	if err != nil || sec == nil {
		http.NotFound(w, r)
		return
	}
	end := sec.End
	if sec.Open() {
		end = time.Now()
	}
	sum, filled, ok := s.sectionSummary(ctx, sec, end)
	if !ok {
		sum = trips.Trip{Start: sec.Start, End: end, Duration: end.Sub(sec.Start)}
	}
	if !sec.Open() && sec.Trip != nil {
		sum = *sec.Trip // what was saved when it stopped
	}
	d := s.data(ctx, "")
	d.Page = "trip"
	d.Section = sec
	cur := &store.SavedTrip{ID: sec.ID, Source: "telemetry", Trip: sum}
	d.Trip = cur
	d.ParentTrip, _ = s.Store.TripOverlapping(ctx, s.Poller.VIN(), sec.Start, end)
	s.tripDetail(ctx, &d, filled)

	// ranked against the other sections, not whole drives
	secs, _ := s.Store.Sections(ctx, s.Poller.VIN(), time.Time{}, time.Time{})
	var all []store.SavedTrip
	for _, x := range secs {
		switch {
		case x.ID == sec.ID:
			all = append(all, *cur)
		case x.Trip != nil:
			all = append(all, store.SavedTrip{ID: x.ID, Source: "telemetry", Trip: *x.Trip})
		}
	}
	d.Board = tripBoard(cur, all, circuitPoints(sum.Path, filled), "/sections/")
	s.render(w, "index.html", d)
}

func (s *Server) renameSection(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if len(name) > 40 {
		name = name[:40]
	}
	s.Store.RenameSection(r.Context(), id, name)
	http.Redirect(w, r, fmt.Sprintf("/sections/%d", id), http.StatusSeeOther)
}
