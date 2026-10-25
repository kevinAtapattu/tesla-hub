// Command imessage-relay runs on the Mac (not in Docker) and sends iMessages for
// the hub, which can't reach the Messages app from its container. It listens on
// localhost only; the hub reaches it at host.docker.internal.
//
//	go build -o ~/.local/bin/imessage-relay ./cmd/imessage-relay
//	imessage-relay -env /path/to/tesla-hub/.env
//
// It reads IMESSAGE_TO (the phone number or Apple ID to text) and
// IMESSAGE_RELAY_TOKEN from the .env file.
package main

import (
	"bufio"
	"crypto/subtle"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The message goes in as an argument, never into the script text.
const script = `on run argv
	tell application "Messages"
		set svc to 1st account whose service type = iMessage
		send (item 2 of argv) to participant (item 1 of argv) of svc
	end tell
end run`

func main() {
	envFile := flag.String("env", ".env", "the hub's .env file")
	addr := flag.String("addr", "127.0.0.1:8097", "listen address")
	flag.Parse()

	env, err := readEnv(*envFile)
	if err != nil {
		slog.Error("read env", "err", err)
		os.Exit(1)
	}
	to, token := env["IMESSAGE_TO"], env["IMESSAGE_RELAY_TOKEN"]
	if to == "" || token == "" {
		slog.Error("set IMESSAGE_TO and IMESSAGE_RELAY_TOKEN in " + *envFile)
		os.Exit(1)
	}

	http.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4000))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			http.Error(w, "empty message", http.StatusBadRequest)
			return
		}
		out, err := exec.CommandContext(r.Context(), "osascript", "-e", script, to, msg).CombinedOutput()
		if err != nil {
			slog.Error("send", "err", err, "out", string(out))
			http.Error(w, strings.TrimSpace(string(out)), http.StatusBadGateway)
			return
		}
		slog.Info("sent")
	})
	srv := &http.Server{Addr: *addr, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("imessage relay listening", "addr", *addr)
	slog.Error("serve", "err", srv.ListenAndServe())
	os.Exit(1)
}

// readEnv reads KEY=value lines, ignoring comments and blank lines.
func readEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, sc.Err()
}
