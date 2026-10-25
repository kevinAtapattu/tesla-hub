package tesla

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/teslamotors/vehicle-command/pkg/protocol"
	"github.com/teslamotors/vehicle-command/pkg/sign"
)

// TelemetryField sets how often the car may send one field. It only sends when
// the value changed, and for Location only after moving MinimumDelta metres.
type TelemetryField struct {
	IntervalSeconds int      `json:"interval_seconds"`
	MinimumDelta    *float64 `json:"minimum_delta,omitempty"`
}

// TelemetryFields is what the car streams. The fast ones drive the trip stats;
// the rest change slowly. Every value sent is billed ($1 per 150,000), and the
// car only streams while awake, so this costs roughly $0.10–0.15 per hour of driving.
func TelemetryFields() map[string]TelemetryField {
	tenMetres := 10.0
	return map[string]TelemetryField{
		"VehicleSpeed":             {IntervalSeconds: 1},
		"Location":                 {IntervalSeconds: 1, MinimumDelta: &tenMetres},
		"Gear":                     {IntervalSeconds: 1},
		"LateralAcceleration":      {IntervalSeconds: 1},
		"LongitudinalAcceleration": {IntervalSeconds: 1},
		"PackCurrent":              {IntervalSeconds: 1},
		"PackVoltage":              {IntervalSeconds: 10}, // drifts slowly; power uses the last value
		"GpsHeading":               {IntervalSeconds: 5},
		"Odometer":                 {IntervalSeconds: 30},
		"Soc":                      {IntervalSeconds: 60},
		"EstBatteryRange":          {IntervalSeconds: 60},
		"InsideTemp":               {IntervalSeconds: 60},
		"OutsideTemp":              {IntervalSeconds: 120},
		"DestinationName":          {IntervalSeconds: 1}, // for drive alerts: "started driving to Home"
	}
}

// TelemetryConfig tells the car where to stream.
type TelemetryConfig struct {
	Hostname string
	Port     int
	CA       string // PEM of the CA that signed the server certificate
	Fields   map[string]TelemetryField
	Expires  time.Time
}

// ConfigureTelemetry signs cfg with the app's private key (the same key the car
// already trusts for commands) and sends it to the car via Fleet API.
func (c *Client) ConfigureTelemetry(ctx context.Context, vin, privateKeyPath string, cfg TelemetryConfig) (json.RawMessage, error) {
	key, err := protocol.LoadPrivateKey(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}
	fields, err := json.Marshal(cfg.Fields)
	if err != nil {
		return nil, err
	}
	var fieldClaims map[string]any
	json.Unmarshal(fields, &fieldClaims)
	token, err := sign.SignMessageForFleet(key, "TelemetryClient", jwt.MapClaims{
		"hostname":    cfg.Hostname,
		"port":        cfg.Port,
		"ca":          cfg.CA,
		"fields":      fieldClaims,
		"alert_types": []string{"service"},
		"exp":         cfg.Expires.Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("sign telemetry config: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"vins": []string{vin}, "token": token})
	return c.post(ctx, "/api/1/vehicles/fleet_telemetry_config_jws", body)
}

// TelemetryStatus is the car's view of the telemetry config.
type TelemetryStatus struct {
	Synced       bool `json:"synced"`
	LimitReached bool `json:"limit_reached"`
	Config       *struct {
		Hostname string `json:"hostname"`
		Port     int    `json:"port"`
		Exp      int64  `json:"exp"`
	} `json:"config"`
}

func (c *Client) TelemetryStatus(ctx context.Context, vin string) (*TelemetryStatus, error) {
	var st TelemetryStatus
	if _, err := c.get(ctx, "/api/1/vehicles/"+vin+"/fleet_telemetry_config", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TelemetryErrors returns recent errors the car hit while streaming (raw JSON).
func (c *Client) TelemetryErrors(ctx context.Context, vin string) (json.RawMessage, error) {
	return c.get(ctx, "/api/1/vehicles/"+vin+"/fleet_telemetry_errors", nil)
}

// post calls an endpoint with the user token and returns the "response" JSON.
func (c *Client) post(ctx context.Context, path string, body []byte) (json.RawMessage, error) {
	tok, err := c.Auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, path, tok, body)
	if c.OnRequest != nil && !errors.Is(err, errServer) {
		c.OnRequest("command")
	}
	if err != nil {
		return nil, err
	}
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return env.Response, nil
}

// ReadCA loads the telemetry CA certificate to embed in the config.
func ReadCA(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
