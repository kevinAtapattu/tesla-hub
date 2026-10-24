// Package weather fetches current conditions and a short forecast from Open-Meteo
// (free, no API key). Responses are cached so the dashboard can call it freely.
package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

var BaseURL = "https://api.open-meteo.com/v1/forecast"

type Hour struct {
	Time       time.Time
	TempC      float64
	PrecipProb int
	Code       int
}

type Report struct {
	FetchedAt  time.Time
	TempC      float64
	FeelsLikeC float64
	PrecipMM   float64
	WindKmh    float64
	Code       int
	Hours      []Hour // next 12 hours
}

func (r *Report) Summary() string { return Describe(r.Code) }

// Precipitating reports whether rain/snow is falling or likely in the next couple of hours.
func (r *Report) Precipitating() bool {
	if r.PrecipMM > 0 {
		return true
	}
	for i, h := range r.Hours {
		if i >= 2 {
			break
		}
		if h.PrecipProb >= 60 {
			return true
		}
	}
	return false
}

// Client caches one report per ~1 km grid cell for TTL.
type Client struct {
	HTTP *http.Client
	TTL  time.Duration

	mu    sync.Mutex
	key   string
	cache *Report
}

func (c *Client) Get(ctx context.Context, lat, lon float64) (*Report, error) {
	key := fmt.Sprintf("%.2f,%.2f", lat, lon)
	ttl := c.TTL
	if ttl == 0 {
		ttl = 15 * time.Minute
	}
	c.mu.Lock()
	if c.cache != nil && c.key == key && time.Since(c.cache.FetchedAt) < ttl {
		r := c.cache
		c.mu.Unlock()
		return r, nil
	}
	c.mu.Unlock()

	r, err := c.fetch(ctx, lat, lon)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.key, c.cache = key, r
	c.mu.Unlock()
	return r, nil
}

func (c *Client) fetch(ctx context.Context, lat, lon float64) (*Report, error) {
	q := url.Values{
		"latitude":       {strconv.FormatFloat(lat, 'f', 4, 64)},
		"longitude":      {strconv.FormatFloat(lon, 'f', 4, 64)},
		"current":        {"temperature_2m,apparent_temperature,precipitation,weather_code,wind_speed_10m"},
		"hourly":         {"temperature_2m,precipitation_probability,weather_code"},
		"forecast_hours": {"12"},
		"timezone":       {"UTC"}, // times are then plain UTC; the UI converts to local
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open-meteo: %s", resp.Status)
	}
	var body struct {
		Current struct {
			Temperature float64 `json:"temperature_2m"`
			Apparent    float64 `json:"apparent_temperature"`
			Precip      float64 `json:"precipitation"`
			Code        int     `json:"weather_code"`
			Wind        float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Hourly struct {
			Time       []string  `json:"time"`
			Temp       []float64 `json:"temperature_2m"`
			PrecipProb []*int    `json:"precipitation_probability"`
			Code       []int     `json:"weather_code"`
		} `json:"hourly"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode open-meteo: %w", err)
	}
	r := &Report{
		FetchedAt: time.Now(), TempC: body.Current.Temperature, FeelsLikeC: body.Current.Apparent,
		PrecipMM: body.Current.Precip, WindKmh: body.Current.Wind, Code: body.Current.Code,
	}
	h := body.Hourly
	for i := range h.Time {
		t, err := time.Parse("2006-01-02T15:04", h.Time[i])
		if err != nil || i >= len(h.Temp) || i >= len(h.Code) {
			continue
		}
		hr := Hour{Time: t, TempC: h.Temp[i], Code: h.Code[i]}
		if i < len(h.PrecipProb) && h.PrecipProb[i] != nil {
			hr.PrecipProb = *h.PrecipProb[i]
		}
		r.Hours = append(r.Hours, hr)
	}
	return r, nil
}

// Describe turns a WMO weather code into words.
func Describe(code int) string {
	switch {
	case code == 0:
		return "Clear"
	case code == 1:
		return "Mainly clear"
	case code == 2:
		return "Partly cloudy"
	case code == 3:
		return "Overcast"
	case code == 45 || code == 48:
		return "Fog"
	case code == 56 || code == 57 || code == 66 || code == 67:
		return "Freezing rain"
	case code >= 51 && code <= 55:
		return "Drizzle"
	case code >= 61 && code <= 65, code >= 80 && code <= 82:
		return "Rain"
	case code >= 71 && code <= 77, code == 85 || code == 86:
		return "Snow"
	case code >= 95:
		return "Thunderstorm"
	}
	return "—"
}
