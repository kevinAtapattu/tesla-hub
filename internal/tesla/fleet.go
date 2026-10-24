package tesla

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrAsleep is returned by VehicleData when the car is asleep or offline (HTTP 408).
var ErrAsleep = errors.New("vehicle is asleep or offline")

// Client makes Fleet API REST calls with the user's token.
type Client struct {
	Auth    *Auth
	BaseURL string
	HTTP    *http.Client
	// OnRequest is called once per billable request with its kind ("data", "wake", "command").
	OnRequest func(kind string)
}

type Vehicle struct {
	ID          int64  `json:"id"`
	VIN         string `json:"vin"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"` // online, asleep, offline
}

func (c *Client) ListVehicles(ctx context.Context) ([]Vehicle, error) {
	var vs []Vehicle
	_, err := c.get(ctx, "/api/1/vehicles", &vs)
	return vs, err
}

// Vehicle returns basic info including online/asleep state. It does not wake the car.
func (c *Client) Vehicle(ctx context.Context, vin string) (*Vehicle, error) {
	var v Vehicle
	_, err := c.get(ctx, "/api/1/vehicles/"+url.PathEscape(vin), &v)
	return &v, err
}

// VehicleData fetches a full snapshot. Only call it when the car is online:
// it doesn't wake the car, but requests to an awake car keep it from sleeping.
func (c *Client) VehicleData(ctx context.Context, vin string) (*Snapshot, error) {
	// location_data is required for GPS on firmware 2023.38+.
	path := "/api/1/vehicles/" + url.PathEscape(vin) + "/vehicle_data?endpoints=" +
		url.QueryEscape("charge_state;climate_state;drive_state;location_data;vehicle_state")
	raw, err := c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	return ParseVehicleData(raw)
}

// WakeAndWait sends one wake_up (the most expensive call) and then polls the cheap
// vehicle endpoint until the car reports online. vehicle-command's own Wakeup
// re-sends wake_up in a loop, which would bill several wakes.
func (c *Client) WakeAndWait(ctx context.Context, vin string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tok, err := c.Auth.AccessToken(ctx)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "/api/1/vehicles/"+url.PathEscape(vin)+"/wake_up", tok, nil)
	if c.OnRequest != nil {
		c.OnRequest("wake")
	}
	if err != nil {
		return fmt.Errorf("wake_up: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("car did not come online within %s", timeout)
		case <-time.After(5 * time.Second):
		}
		v, err := c.Vehicle(ctx, vin)
		if err == nil && v.State == "online" {
			return nil
		}
	}
}

// Register registers this app's domain with Tesla using a partner token (one-time setup).
func (c *Client) Register(ctx context.Context, partnerToken, domain string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"domain": domain})
	return c.do(ctx, http.MethodPost, "/api/1/partner_accounts", partnerToken, body)
}

// RegisteredPublicKey asks Tesla which public key it has on file for domain.
func (c *Client) RegisteredPublicKey(ctx context.Context, partnerToken, domain string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/1/partner_accounts/public_key?domain="+url.QueryEscape(domain), partnerToken, nil)
}

// get calls an endpoint with the user token. If out is non-nil the "response" field is decoded into it.
// It returns the raw "response" JSON.
func (c *Client) get(ctx context.Context, path string, out any) (json.RawMessage, error) {
	tok, err := c.Auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := c.do(ctx, http.MethodGet, path, tok, nil)
	if c.OnRequest != nil && !errors.Is(err, errServer) {
		c.OnRequest("data") // Tesla bills every response below 500
	}
	if err != nil {
		return nil, err
	}
	var env struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if out != nil {
		if err := json.Unmarshal(env.Response, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return env.Response, nil
}

var errServer = errors.New("tesla server error")

func (c *Client) do(ctx context.Context, method, path, token string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusRequestTimeout:
		return nil, ErrAsleep
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s %s: %s", errServer, method, path, respBody)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, respBody)
	}
	return respBody, nil
}
