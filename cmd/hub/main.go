// Command hub runs the Tesla Hub service.
//
//	hub            run the dashboard, poller and scheduler (default)
//	hub keygen     create the key pair for signed commands
//	hub register   register your domain with Tesla (one-time)
//	hub telemetry-certs <host>   create the TLS certificates for the telemetry server
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/kevin/tesla-hub/internal/alerts"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // so TZ works in a minimal container

	"github.com/kevin/tesla-hub/internal/assistant"
	"github.com/kevin/tesla-hub/internal/config"
	"github.com/kevin/tesla-hub/internal/poller"
	"github.com/kevin/tesla-hub/internal/rules"
	"github.com/kevin/tesla-hub/internal/store"
	"github.com/kevin/tesla-hub/internal/telemetry"
	"github.com/kevin/tesla-hub/internal/tesla"
	"github.com/kevin/tesla-hub/internal/trips"
	"github.com/kevin/tesla-hub/internal/weather"
	"github.com/kevin/tesla-hub/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		err = serve(ctx, cfg)
	case "keygen":
		err = keygen(cfg)
	case "register":
		err = register(ctx, cfg)
	case "telemetry-certs":
		if len(os.Args) < 3 {
			err = errors.New("usage: hub telemetry-certs <public hostname>")
			break
		}
		if err = telemetry.GenerateCerts(cfg.TelemetryCertsDir, os.Args[2]); err == nil {
			fmt.Println("wrote certificates to", cfg.TelemetryCertsDir)
		}
	default:
		err = fmt.Errorf("unknown command %q (want serve, keygen, register or telemetry-certs)", cmd)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func serve(ctx context.Context, cfg *config.Config) error {
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	auth := &tesla.Auth{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		RedirectURI: cfg.RedirectURI(), Audience: cfg.FleetBaseURL(), Store: st,
	}
	fleet := &tesla.Client{
		Auth: auth, BaseURL: cfg.FleetBaseURL(),
		OnRequest: func(kind string) {
			if err := st.CountRequest(context.Background(), kind); err != nil {
				slog.Warn("count request", "err", err)
			}
		},
	}
	commander := &tesla.Commander{Fleet: fleet, PrivateKeyPath: cfg.PrivateKeyPath}
	p := poller.New(cfg, fleet, st)
	sched := &rules.Scheduler{Store: st, Commander: commander, Location: cfg.Location, VIN: p.VIN, AfterRun: p.Nudge}

	go p.Run(ctx)
	go sched.Run(ctx)
	tripBuilder := &trips.Builder{Store: st, VIN: p.VIN}
	go tripBuilder.Run(ctx)
	driveAlerts := &alerts.Watcher{Store: st, Location: cfg.Location}
	if cfg.TelemetryHost != "" {
		driveAlerts.ClickURL = "https://" + cfg.TelemetryHost + ":8444/" // the dashboard over Tailscale
	}
	go driveAlerts.Run(ctx)
	var feed *telemetry.Receiver
	if cfg.TelemetryHost != "" {
		feed = &telemetry.Receiver{Addr: cfg.TelemetryZMQ, Store: st, OnPoint: driveAlerts.Observe}
		go feed.Run(ctx)
	}

	dash := &web.Server{Cfg: cfg, Auth: auth, Store: st, Poller: p, Commander: commander, Weather: &weather.Client{}, RunScheduler: sched.Nudge, Telemetry: feed, Trips: tripBuilder, Alerts: driveAlerts}
	dash.Assistant = assistant.New(cfg.AnthropicAPIKey, cfg.AssistantModel, dash.AssistantActions(), cfg.Location)
	if dash.Assistant == nil {
		slog.Info("assistant off: set ANTHROPIC_API_KEY to enable it")
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           dash.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	// The public listener serves only the app's public key, which Tesla fetches
	// from https://<TESLA_DOMAIN>/.well-known/... It's what Tailscale Funnel exposes
	// to the internet, so it must never route to the dashboard.
	pub := &http.Server{Addr: cfg.PublicKeyListenAddr, Handler: publicKeyHandler(cfg.PublicKeyPath), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := pub.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("public key server", "err", err)
		}
	}()
	go func() { <-ctx.Done(); pub.Close() }()

	if missing := cfg.MissingTeslaApp(); len(missing) > 0 {
		slog.Warn("Tesla app not configured yet", "missing", strings.Join(missing, ", "))
	}
	slog.Info("dashboard ready", "url", cfg.PublicBaseURL, "timezone", cfg.Location.String())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func publicKeyHandler(path string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/appspecific/com.tesla.3p.public-key.pem", func(w http.ResponseWriter, r *http.Request) {
		key, err := os.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(key)
	})
	return mux
}

func keygen(cfg *config.Config) error {
	if err := tesla.GenerateKeyPair(cfg.PrivateKeyPath, cfg.PublicKeyPath); err != nil {
		return err
	}
	fmt.Println("private key:", cfg.PrivateKeyPath, "(keep secret, never commit)")
	fmt.Println("public key: ", cfg.PublicKeyPath, "(publish this at your domain)")
	return nil
}

// register checks the hosted public key, then registers the domain with Tesla.
func register(ctx context.Context, cfg *config.Config) error {
	if missing := cfg.MissingTeslaApp(); len(missing) > 0 {
		return fmt.Errorf("set %s in .env first", strings.Join(missing, ", "))
	}
	local, err := os.ReadFile(cfg.PublicKeyPath)
	if err != nil {
		return fmt.Errorf("read local public key (run keygen first?): %w", err)
	}

	keyURL := "https://" + cfg.Domain + "/.well-known/appspecific/com.tesla.3p.public-key.pem"
	fmt.Println("1/3 checking", keyURL)
	resp, err := http.Get(keyURL)
	if err != nil {
		return err
	}
	hosted, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s; is the key published (and .nojekyll present on GitHub Pages)?", keyURL, resp.Status)
	}
	if !bytes.Equal(bytes.TrimSpace(hosted), bytes.TrimSpace(local)) {
		return fmt.Errorf("the key at %s doesn't match %s", keyURL, cfg.PublicKeyPath)
	}
	fmt.Println("    ✓ hosted key matches")

	auth := &tesla.Auth{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Audience: cfg.FleetBaseURL()}
	fmt.Println("2/3 getting partner token")
	tok, err := auth.PartnerToken(ctx)
	if err != nil {
		return err
	}
	fleet := &tesla.Client{BaseURL: cfg.FleetBaseURL()}
	fmt.Println("3/3 registering", cfg.Domain, "in region", cfg.Region)
	body, err := fleet.Register(ctx, tok, cfg.Domain)
	if err != nil {
		return err
	}
	fmt.Println("    ✓", string(body))
	if body, err := fleet.RegisteredPublicKey(ctx, tok, cfg.Domain); err == nil {
		fmt.Println("    Tesla has this key on file:", string(body))
	}
	fmt.Println("\nDone. Next: open", cfg.PublicBaseURL, "and click Connect Tesla.")
	return nil
}
