# TesRace setup

Already done for you:

- The Docker stack runs: dashboard at http://localhost:8095 and Grafana at http://localhost:3095.
- Your key pair is generated: `secrets/private-key.pem` (secret) and `public-key-site/` (public).
- `.env` exists with every value except your Tesla credentials.

All `docker` commands run from this folder (`~/Developer/tesla-hub`). If your terminal says `docker: command not found`, run this once:

```sh
echo 'export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

---

## 1. Publish your public key (GitHub Pages, free)

Tesla downloads your public key from `https://<your domain>/.well-known/appspecific/com.tesla.3p.public-key.pem`. A GitHub Pages site gives you a free domain to host it on.

1. On GitHub, create a **public** repo named exactly `<your-github-username>.github.io`.
2. Push the key folder to it. Use git rather than the web uploader: Finder hides the `.well-known` folder, so drag-and-drop skips it.
   ```sh
   cd public-key-site
   git init && git add -A && git commit -m "Tesla public key"
   git branch -M main
   git remote add origin https://github.com/<user>/<user>.github.io.git
   git push -u origin main
   cd ..
   ```
   The `.nojekyll` file is already included. Without it, GitHub Pages won't serve the `.well-known` folder.
3. After a minute or so, this should print the key:
   ```sh
   curl https://<user>.github.io/.well-known/appspecific/com.tesla.3p.public-key.pem
   ```

Your domain is `<user>.github.io`.

## 2. Create the Tesla developer app

Go to https://developer.tesla.com and sign in with the Tesla account that owns the car. You need multi-factor authentication turned on.

1. **Create application.** For an individual developer, personal details are fine. Name it something unique, e.g. "kevin-tesla-hub". Taken names get rejected automatically.
2. **OAuth settings:**
   - **Allowed Origin(s):** `https://<user>.github.io`
   - **Allowed Redirect URI(s):** `http://localhost:8095/auth/callback`
   - **Allowed Returned URL(s):** `http://localhost:8095` (if asked)
3. **Scopes:** turn on Vehicle Information, Vehicle Location, Vehicle Commands, and Vehicle Charging Management. Profile Information is optional.
4. **Billing:** add a payment method and set a monthly billing limit, e.g. $5 or $10. Tesla's billing limit defaults to $0, and an app with no payment method gets **disabled automatically**. The $10 monthly credit is applied before you're charged anything.
5. Copy the **Client ID** and **Client Secret** from the app's credentials page.

If Tesla rejects the `localhost` redirect URI, tell me. The fallback is to put the dashboard behind a Cloudflare Tunnel and use its https URL instead.

## 3. Fill in `.env`

Open `~/Developer/tesla-hub/.env` and set:

```
TESLA_CLIENT_ID=<client id>
TESLA_CLIENT_SECRET=<client secret>
TESLA_DOMAIN=<user>.github.io
```

Keep `TESLA_REGION=na`, which covers Canada and the US. Then restart the hub so it picks up the new values:

```sh
docker compose up -d --force-recreate hub
```

## 4. Register your domain with Tesla (one time)

```sh
docker compose run --rm hub register
```

This command:

- checks that the key hosted on your domain matches your local key,
- gets a partner token,
- registers your domain with Tesla.

Each step prints ✓ when it succeeds.

## 5. Connect your Tesla account

Open http://localhost:8095, click **Connect Tesla**, log in, and approve **all** the permissions. You'll come back to the dashboard with "Tesla account connected".

## 6. Pair the virtual key (needed for preheat and other commands)

On your **phone**, with the Tesla app installed and logged in, open:

```
https://tesla.com/_ak/<user>.github.io
```

Approve adding the key. The car may ask you to confirm on its screen with your key card. This step is what lets the hub send signed commands.

## 7. First data

Data only comes in while the car is awake. To get the first reading, open the Tesla app on your phone (that wakes the car), then click **Refresh** on the dashboard. You should see battery, range, temperatures and location.

Then try **Preheat now**, and add a schedule. The schedule form fills in the car's current location, so if the car is at home, "Only if the car is here" is already set to home.

---

## Day to day

| What | Where |
|---|---|
| Dashboard and controls | http://localhost:8095 |
| Charts and map | http://localhost:3095 (Grafana; edit login is admin / admin) |
| Logs | `docker compose logs -f hub` |
| Stop / start | `docker compose stop` / `docker compose start` |
| Rebuild after code changes | `docker compose up -d --build hub` |
| Run from source instead | `docker compose stop hub && set -a && source .env && set +a && go run ./cmd/hub` |

**Your Mac has to be awake** for polling and schedules to run. Docker pauses when the Mac sleeps. For reliable morning preheats, either keep the Mac plugged in with sleep disabled (System Settings → Battery → Options), or move the stack to an always-on machine later.

## How spending is kept low

Tesla bills every API request, at roughly $0.002 per data request, $0.001 per command and $0.02 per wake. The poller keeps that low:

- **Car asleep:** it only checks whether the car is awake, every 20 minutes (`POLL_SLEEP_CHECK`). That check never wakes the car.
- **Driving:** it samples every 2 minutes. **Charging or climate running:** every 5 minutes.
- **Parked:** it takes one sample, then waits 20 minutes so the car can fall asleep. Constant polling would keep the car awake and drain the battery.
- **Budget cap:** polling pauses for the month at `MONTHLY_REQUEST_BUDGET` requests (default 4,000, about $8). The dashboard shows a running estimate.

Tradeoff: with these settings, a drive can take a while to show up after it starts. Fleet Telemetry streaming would fix that and costs less, but it needs your server to be reachable from the internet, so it's a later step.

## Troubleshooting

- **"token endpoint 401" when registering:** wrong client ID or secret in `.env`, or you didn't restart the hub after editing it.
- **The register step fails on "hosted key":** GitHub Pages isn't serving the file yet, or `.nojekyll` is missing.
- **"start session (is the virtual key paired?)":** redo step 6.
- **No location on the dashboard:** you didn't approve Vehicle Location at login. Disconnect at the bottom of the dashboard, then connect again.
- **Losing the private key:** if `secrets/private-key.pem` is ever lost, run `hub keygen` again, publish the new public key, register again, and pair again.
