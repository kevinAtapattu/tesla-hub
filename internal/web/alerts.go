package web

import (
	"context"
	"net/http"
)

func (s *Server) alertData(ctx context.Context, d *pageData) {
	if s.Alerts == nil || s.Telemetry == nil {
		return
	}
	d.AlertsOn = true
	d.Alerts, _ = s.Alerts.Settings(ctx)
}

func (s *Server) renderAlerts(w http.ResponseWriter, r *http.Request, note string) {
	d := s.data(r.Context(), "")
	s.alertData(r.Context(), &d)
	d.AlertsNote = note
	s.render(w, "alerts-card", d)
}

func (s *Server) setAlertMode(w http.ResponseWriter, r *http.Request) {
	if s.Alerts == nil {
		http.NotFound(w, r)
		return
	}
	note := ""
	if err := s.Alerts.SetMode(r.Context(), r.FormValue("mode")); err != nil {
		note = "Couldn't save: " + err.Error()
	}
	s.renderAlerts(w, r, note)
}

func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) {
	if s.Alerts == nil {
		http.NotFound(w, r)
		return
	}
	note := "Test sent. It should show up on your phone in a few seconds."
	st, err := s.Alerts.Settings(r.Context())
	if err == nil {
		err = s.Alerts.Send(r.Context(), st.Topic, "Test from Tesla Hub", "Drive alerts work. You'll get one like this when the car starts moving.")
	}
	if err != nil {
		note = "Test failed: " + err.Error()
	}
	s.renderAlerts(w, r, note)
}
