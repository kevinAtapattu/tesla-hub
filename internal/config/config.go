// Package config loads settings from environment variables (see .env.example).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ClientID     string
	ClientSecret string
	Domain       string // where the public key is hosted, e.g. yourname.github.io
	Region       string // na or eu
	VIN          string // optional; first vehicle on the account if empty

	PublicBaseURL string // how your browser reaches the hub, e.g. http://localhost:8095
	ListenAddr    string
	// PublicKeyListenAddr serves only the public key; this is what goes on the internet.
	PublicKeyListenAddr string
	DatabaseURL         string
	PrivateKeyPath      string
	PublicKeyPath       string

	Location          *time.Location
	DashboardPassword string // optional HTTP basic auth password

	// Polling. Every Fleet API request is billed, so these are deliberately slow.
	SleepCheckInterval time.Duration // how often to check whether an asleep car woke up
	DrivingInterval    time.Duration
	ChargingInterval   time.Duration
	ParkedInterval     time.Duration // parked but awake; after one sample we back off so it can sleep
	MonthlyBudget      int           // max billable requests per calendar month before throttling

	// Fleet Telemetry: the car streams to TelemetryHost:TelemetryPort (public, via
	// Tailscale Funnel), and the fleet-telemetry server republishes to TelemetryZMQ.
	TelemetryHost     string
	TelemetryPort     int
	TelemetryZMQ      string
	TelemetryCertsDir string

	AnthropicAPIKey string // enables the assistant ("warm it up, I'll be there in 15")
	AssistantModel  string
}

func Load() (*Config, error) {
	tz := env("TZ", "America/Toronto")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("TZ %q: %w", tz, err)
	}
	c := &Config{
		ClientID:            os.Getenv("TESLA_CLIENT_ID"),
		ClientSecret:        os.Getenv("TESLA_CLIENT_SECRET"),
		Domain:              strings.TrimSuffix(strings.TrimPrefix(os.Getenv("TESLA_DOMAIN"), "https://"), "/"),
		Region:              env("TESLA_REGION", "na"),
		VIN:                 os.Getenv("TESLA_VIN"),
		PublicBaseURL:       strings.TrimSuffix(env("PUBLIC_BASE_URL", "http://localhost:8095"), "/"),
		ListenAddr:          env("LISTEN_ADDR", ":8095"),
		PublicKeyListenAddr: env("PUBLIC_KEY_LISTEN_ADDR", ":8096"),
		DatabaseURL:         env("DATABASE_URL", "postgres://tesla:tesla@localhost:5495/tesla?sslmode=disable"),
		PrivateKeyPath:      env("PRIVATE_KEY_PATH", "secrets/private-key.pem"),
		PublicKeyPath:       env("PUBLIC_KEY_PATH", "public-key-site/.well-known/appspecific/com.tesla.3p.public-key.pem"),
		Location:            loc,
		DashboardPassword:   os.Getenv("DASHBOARD_PASSWORD"),
		SleepCheckInterval:  envDuration("POLL_SLEEP_CHECK", 20*time.Minute),
		DrivingInterval:     envDuration("POLL_DRIVING", 2*time.Minute),
		ChargingInterval:    envDuration("POLL_CHARGING", 5*time.Minute),
		ParkedInterval:      envDuration("POLL_PARKED", 20*time.Minute),
		MonthlyBudget:       envInt("MONTHLY_REQUEST_BUDGET", 4000),
		TelemetryHost:       os.Getenv("TELEMETRY_HOST"),
		TelemetryPort:       envInt("TELEMETRY_PORT", 8443),
		TelemetryZMQ:        env("TELEMETRY_ZMQ", "tcp://telemetry:5284"),
		TelemetryCertsDir:   env("TELEMETRY_CERTS_DIR", "secrets/telemetry"),
		AnthropicAPIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		AssistantModel:      env("ASSISTANT_MODEL", "claude-opus-5-5"),
	}
	if c.Region != "na" && c.Region != "eu" {
		return nil, fmt.Errorf("TESLA_REGION must be na or eu, got %q", c.Region)
	}
	return c, nil
}

// FleetBaseURL is the regional Fleet API host; it's also the OAuth audience.
func (c *Config) FleetBaseURL() string {
	return "https://fleet-api.prd." + c.Region + ".vn.cloud.tesla.com"
}

func (c *Config) RedirectURI() string { return c.PublicBaseURL + "/auth/callback" }

// MissingTeslaApp lists required env vars that are still empty.
func (c *Config) MissingTeslaApp() []string {
	var missing []string
	if c.ClientID == "" {
		missing = append(missing, "TESLA_CLIENT_ID")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "TESLA_CLIENT_SECRET")
	}
	if c.Domain == "" {
		missing = append(missing, "TESLA_DOMAIN")
	}
	return missing
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}
