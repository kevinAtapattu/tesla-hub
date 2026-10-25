package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/kevin/tesla-hub/internal/telemetry"
	"github.com/kevin/tesla-hub/internal/tesla"
)

// configureTelemetry signs a streaming config and sends it to the car. The car
// picks it up the next time it's awake.
func (s *Server) configureTelemetry(w http.ResponseWriter, r *http.Request) {
	note := func(msg string) {
		d := s.data(r.Context(), "")
		d.TelemetryNote = msg
		s.render(w, "telemetry-card", d)
	}
	vin := s.Poller.VIN()
	if vin == "" || s.Telemetry == nil {
		note("Set TELEMETRY_HOST and make sure the car has been found first.")
		return
	}
	ca, err := tesla.ReadCA(filepath.Join(s.Cfg.TelemetryCertsDir, telemetry.CAFile))
	if err != nil {
		note("Couldn't read the telemetry CA: " + err.Error())
		return
	}
	resp, err := s.Commander.Fleet.ConfigureTelemetry(r.Context(), vin, s.Cfg.PrivateKeyPath, tesla.TelemetryConfig{
		Hostname: s.Cfg.TelemetryHost,
		Port:     s.Cfg.TelemetryPort,
		CA:       ca,
		Fields:   tesla.TelemetryFields(),
		Expires:  time.Now().AddDate(0, 0, 360), // Tesla accepts up to ~364 days; re-send yearly
	})
	if err != nil {
		note("Tesla rejected the config: " + err.Error())
		return
	}
	note("Sent. Tesla replied: " + string(resp) + ". The car applies it next time it's awake; press Check in a few minutes.")
}

// checkTelemetry asks Tesla whether the car has applied the config and for any errors it reported.
func (s *Server) checkTelemetry(w http.ResponseWriter, r *http.Request) {
	d := s.data(r.Context(), "")
	vin := s.Poller.VIN()
	var parts []string
	if st, err := s.Commander.Fleet.TelemetryStatus(r.Context(), vin); err != nil {
		parts = append(parts, "Status check failed: "+err.Error())
	} else {
		switch {
		case st.Config == nil:
			parts = append(parts, "The car has no telemetry config yet.")
		case st.Synced:
			parts = append(parts, fmt.Sprintf("The car has applied the config and streams to %s:%d.", st.Config.Hostname, st.Config.Port))
		default:
			parts = append(parts, "Config sent but not applied yet. Wake the car (open the Tesla app) and check again.")
		}
		if st.LimitReached {
			parts = append(parts, "The car already streams to the maximum number of apps.")
		}
	}
	if errs, err := s.Commander.Fleet.TelemetryErrors(r.Context(), vin); err == nil && len(errs) > 0 && !strings.Contains(string(errs), `"fleet_telemetry_errors":[]`) && string(errs) != "null" {
		parts = append(parts, "Car-reported errors: "+string(errs))
	}
	d.TelemetryNote = strings.Join(parts, " ")
	s.render(w, "telemetry-card", d)
}
