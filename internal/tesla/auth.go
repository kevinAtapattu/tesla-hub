// Package tesla talks to Tesla's Fleet API: OAuth, vehicle data, and signed commands.
package tesla

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Variables rather than constants so tests can point them at an httptest server.
var (
	AuthorizeURL = "https://auth.tesla.com/oauth2/v3/authorize"
	TokenURL     = "https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token"
)

// Scopes requested from the car owner. vehicle_location is needed for GPS data.
const UserScopes = "openid offline_access user_data vehicle_device_data vehicle_location vehicle_cmds vehicle_charging_cmds"

// ErrNotConnected means no Tesla account has been linked yet (or the refresh token died).
var ErrNotConnected = errors.New("tesla account not connected: open the dashboard and click Connect")

type Token struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

type TokenStore interface {
	LoadToken(ctx context.Context) (*Token, error) // returns nil, nil when none saved
	SaveToken(ctx context.Context, t *Token) error
	DeleteToken(ctx context.Context) error
}

// Auth handles the OAuth flow and keeps a fresh access token.
type Auth struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Audience     string
	Store        TokenStore
	HTTP         *http.Client

	mu  sync.Mutex
	tok *Token
}

func (a *Auth) AuthCodeURL(state string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.ClientID},
		"redirect_uri":          {a.RedirectURI},
		"scope":                 {UserScopes},
		"state":                 {state},
		"prompt_missing_scopes": {"true"},
	}
	return AuthorizeURL + "?" + q.Encode()
}

// Exchange trades the ?code= from the OAuth callback for tokens and saves them.
func (a *Auth) Exchange(ctx context.Context, code string) error {
	tok, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
		"code":          {code},
		"audience":      {a.Audience},
		"redirect_uri":  {a.RedirectURI},
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = tok
	return a.Store.SaveToken(ctx, tok)
}

// AccessToken returns a valid access token, refreshing it if it expires within 5 minutes.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.tok == nil {
		tok, err := a.Store.LoadToken(ctx)
		if err != nil {
			return "", err
		}
		if tok == nil {
			return "", ErrNotConnected
		}
		a.tok = tok
	}
	if time.Until(a.tok.Expiry) > 5*time.Minute {
		return a.tok.AccessToken, nil
	}

	// Tesla refresh tokens are single-use: the response carries a new one that
	// must be saved before anything else, or the next refresh fails.
	tok, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {a.ClientID},
		"refresh_token": {a.tok.RefreshToken},
	})
	if err != nil {
		return "", fmt.Errorf("refresh token: %w", err)
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = a.tok.RefreshToken
	}
	if err := a.Store.SaveToken(ctx, tok); err != nil {
		return "", fmt.Errorf("save refreshed token: %w", err)
	}
	a.tok = tok
	return tok.AccessToken, nil
}

func (a *Auth) Connected(ctx context.Context) bool {
	_, err := a.AccessToken(ctx)
	return err == nil
}

func (a *Auth) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = nil
	return a.Store.DeleteToken(ctx)
}

// PartnerToken gets an app-level token (client_credentials), used only for registration.
func (a *Auth) PartnerToken(ctx context.Context) (string, error) {
	tok, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
		"audience":      {a.Audience},
		"scope":         {"openid vehicle_device_data vehicle_location vehicle_cmds vehicle_charging_cmds"},
	})
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (a *Auth) tokenRequest(ctx context.Context, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint %s: %s", resp.Status, body)
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if r.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned no access_token: %s", body)
	}
	return &Token{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}, nil
}

func (a *Auth) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return http.DefaultClient
}
