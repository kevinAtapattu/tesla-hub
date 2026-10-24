package tesla

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/teslamotors/vehicle-command/pkg/protocol"
)

const vehicleDataFixture = `{
  "vin": "5YJ3E1EA7KF000000", "state": "online",
  "charge_state": {"battery_level": 78, "battery_range": 200.5, "charging_state": "Disconnected", "charge_limit_soc": 80, "charger_power": 0},
  "climate_state": {"inside_temp": 18.5, "outside_temp": -2, "is_climate_on": false, "driver_temp_setting": 21},
  "drive_state": {"latitude": 43.6532, "longitude": -79.3832, "heading": 90, "speed": null, "shift_state": null, "power": 0, "timestamp": 1761040000000},
  "vehicle_state": {"odometer": 12345.6, "locked": true, "sentry_mode": false, "car_version": "2026.32.5"}
}`

func TestParseVehicleData(t *testing.T) {
	s, err := ParseVehicleData(json.RawMessage(vehicleDataFixture))
	if err != nil {
		t.Fatal(err)
	}
	if *s.BatteryLevel != 78 || s.ChargingState != "Disconnected" || *s.OutsideTempC != -2 {
		t.Errorf("charge/climate parsed wrong: %+v", s)
	}
	if got := *s.RangeKm; got < 322.6 || got > 322.8 {
		t.Errorf("range %.2f km, want ~322.7", got)
	}
	if *s.Latitude != 43.6532 || s.SpeedKmh != nil || s.ShiftState != "" {
		t.Errorf("drive state parsed wrong: %+v", s)
	}
	if s.Driving() || s.Charging() || s.ClimateRunning() {
		t.Error("parked car reported as active")
	}
	if !s.Time.Equal(time.UnixMilli(1761040000000)) {
		t.Errorf("time = %v", s.Time)
	}
}

type memStore struct {
	mu  sync.Mutex
	tok *Token
}

func (m *memStore) LoadToken(context.Context) (*Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tok, nil
}
func (m *memStore) SaveToken(_ context.Context, t *Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok = t
	return nil
}
func (m *memStore) DeleteToken(context.Context) error { m.tok = nil; return nil }

func TestAccessTokenRefreshesAndSavesRotatedToken(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			http.Error(w, "bad request: "+r.Form.Encode(), 400)
			return
		}
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800}`)
	}))
	defer srv.Close()
	TokenURL = srv.URL

	st := &memStore{tok: &Token{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: time.Now().Add(time.Minute)}}
	a := &Auth{ClientID: "id", Store: st}

	tok, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "new-access" || st.tok.RefreshToken != "new-refresh" {
		t.Fatalf("got %q, stored refresh %q", tok, st.tok.RefreshToken)
	}
	// Second call uses the cached, still-valid token.
	if _, err := a.AccessToken(context.Background()); err != nil || calls != 1 {
		t.Fatalf("expected 1 token call, got %d (err %v)", calls, err)
	}
}

func TestAccessTokenNotConnected(t *testing.T) {
	a := &Auth{Store: &memStore{}}
	if _, err := a.AccessToken(context.Background()); err != ErrNotConnected {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateKeyPairLoadsInVehicleCommand(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "priv.pem"), filepath.Join(dir, "site", "pub.pem")
	if err := GenerateKeyPair(priv, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.LoadPrivateKey(priv); err != nil {
		t.Fatalf("vehicle-command can't load private key: %v", err)
	}
	if _, err := protocol.LoadPublicKey(pub); err != nil {
		t.Fatalf("vehicle-command can't load public key: %v", err)
	}
	if err := GenerateKeyPair(priv, pub); err == nil {
		t.Fatal("expected refusal to overwrite existing key")
	}
}

func TestVehicleData408IsAsleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"vehicle unavailable"}`, http.StatusRequestTimeout)
	}))
	defer srv.Close()
	st := &memStore{tok: &Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}}
	var counted []string
	c := &Client{Auth: &Auth{Store: st}, BaseURL: srv.URL, OnRequest: func(k string) { counted = append(counted, k) }}
	if _, err := c.VehicleData(context.Background(), "VIN"); err != ErrAsleep {
		t.Fatalf("err = %v, want ErrAsleep", err)
	}
	if len(counted) != 1 {
		t.Fatalf("billable requests counted = %v", counted)
	}
}
