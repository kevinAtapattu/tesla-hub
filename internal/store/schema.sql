-- Applied on every startup; every statement must be idempotent.

CREATE TABLE IF NOT EXISTS oauth_token (
    id            int PRIMARY KEY DEFAULT 1 CHECK (id = 1), -- single row
    access_token  text NOT NULL,
    refresh_token text NOT NULL,
    expiry        timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- One row per vehicle_data sample. Units are metric. raw keeps the full response
-- so you can pull out more fields later without re-collecting.
CREATE TABLE IF NOT EXISTS vehicle_states (
    time                timestamptz NOT NULL,
    vin                 text NOT NULL,
    state               text,
    battery_level       int,
    range_km            double precision,
    charging_state      text,
    charge_limit        int,
    charger_power_kw    double precision,
    inside_temp_c       double precision,
    outside_temp_c      double precision,
    climate_on          boolean,
    driver_temp_setting double precision,
    latitude            double precision,
    longitude           double precision,
    heading             int,
    speed_kmh           double precision,
    shift_state         text,
    power_kw            double precision,
    odometer_km         double precision,
    locked              boolean,
    sentry              boolean,
    car_version         text,
    raw                 jsonb
);
CREATE INDEX IF NOT EXISTS vehicle_states_vin_time ON vehicle_states (vin, time DESC);

-- online/asleep/offline transitions (only written when the state changes).
CREATE TABLE IF NOT EXISTS vehicle_presence (
    time  timestamptz NOT NULL DEFAULT now(),
    vin   text NOT NULL,
    state text NOT NULL
);
CREATE INDEX IF NOT EXISTS vehicle_presence_vin_time ON vehicle_presence (vin, time DESC);

-- Billable Fleet API requests per month, so you can watch spend against the $10 credit.
CREATE TABLE IF NOT EXISTS api_requests (
    month date NOT NULL,
    kind  text NOT NULL, -- data, wake, command
    count int  NOT NULL DEFAULT 0,
    PRIMARY KEY (month, kind)
);

CREATE TABLE IF NOT EXISTS rules (
    id                 serial PRIMARY KEY,
    name               text NOT NULL,
    enabled            boolean NOT NULL DEFAULT true,
    days               text NOT NULL,          -- weekdays, 0=Sunday, e.g. "1,2,3,4,5"
    at_time            text NOT NULL,          -- local "HH:MM"
    latitude           double precision,       -- geofence centre; NULL = anywhere
    longitude          double precision,
    radius_m           double precision,
    max_outside_temp_c double precision,       -- only run if colder than this; NULL = always
    temp_c             double precision NOT NULL DEFAULT 21,
    seat_heat          boolean NOT NULL DEFAULT false,
    steering_heat      boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rule_runs (
    id         serial PRIMARY KEY,
    rule_id    int NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
    local_date date NOT NULL,
    ran_at     timestamptz NOT NULL DEFAULT now(),
    status     text NOT NULL,  -- running, done, skipped, failed
    message    text NOT NULL DEFAULT '',
    UNIQUE (rule_id, local_date) -- at most one run per rule per day
);

-- One-off "I'm leaving at X" requests. start_at is when climate should switch on.
CREATE TABLE IF NOT EXISTS departures (
    id            serial PRIMARY KEY,
    depart_at     timestamptz NOT NULL,
    start_at      timestamptz NOT NULL,
    temp_c        double precision NOT NULL,
    seat_heat     boolean NOT NULL DEFAULT false,
    steering_heat boolean NOT NULL DEFAULT false,
    reason        text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'pending', -- pending, running, done, failed, cancelled, missed
    message       text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS departures_pending ON departures (start_at) WHERE status = 'pending';

-- Small key/value settings: comfort preferences, the assistant's access token.
CREATE TABLE IF NOT EXISTS settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every message sent to the assistant (e.g. from Siri) and what it replied.
CREATE TABLE IF NOT EXISTS assistant_log (
    id       bigserial PRIMARY KEY,
    time     timestamptz NOT NULL DEFAULT now(),
    message  text NOT NULL,
    reply    text NOT NULL DEFAULT '',
    error    text NOT NULL DEFAULT ''
);

-- Fleet Telemetry: one row per record streamed by the car (about every second
-- while driving). Records only carry fields that changed, so most columns are
-- null; data keeps every field exactly as received, in metric where converted.
CREATE TABLE IF NOT EXISTS telemetry (
    time            timestamptz NOT NULL,
    vin             text NOT NULL,
    resend          boolean NOT NULL DEFAULT false,
    speed_kmh       double precision,
    latitude        double precision,
    longitude       double precision,
    heading         double precision,
    gear            text,
    soc             double precision,
    odometer_km     double precision,
    power_kw        double precision,
    lat_accel_ms2   double precision,
    long_accel_ms2  double precision,
    pedal           double precision,
    range_km        double precision,
    inside_temp_c   double precision,
    outside_temp_c  double precision,
    data            jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS telemetry_vin_time ON telemetry (vin, time);

-- Connectivity changes, errors and alerts the car reports over telemetry.
CREATE TABLE IF NOT EXISTS telemetry_events (
    time  timestamptz NOT NULL,
    vin   text NOT NULL,
    kind  text NOT NULL,
    data  jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS telemetry_events_time ON telemetry_events (time DESC);

-- Finished drives with their stats (built from telemetry, or from polling before
-- telemetry existed). summary holds the full stats and the route.
CREATE TABLE IF NOT EXISTS trips (
    id          bigserial PRIMARY KEY,
    vin         text NOT NULL,
    start_at    timestamptz NOT NULL,
    end_at      timestamptz NOT NULL,
    source      text NOT NULL, -- telemetry or polling
    distance_km double precision NOT NULL,
    summary     jsonb NOT NULL,
    UNIQUE (vin, start_at)
);
CREATE INDEX IF NOT EXISTS trips_vin_start ON trips (vin, start_at DESC);

-- Who drove. The car doesn't report its driver profile, so each driver's phone
-- tells the hub ("Kevin is driving") when it connects to the car's Bluetooth,
-- and a trip takes the latest such event from shortly before it started.
CREATE TABLE IF NOT EXISTS driver_events (
    time timestamptz NOT NULL DEFAULT now(),
    name text NOT NULL
);
CREATE INDEX IF NOT EXISTS driver_events_time ON driver_events (time DESC);
ALTER TABLE trips ADD COLUMN IF NOT EXISTS driver text;

-- Album art for songs the car played, looked up by title and artist. url is ''
-- when the lookup found nothing, so it isn't retried on every page view.
CREATE TABLE IF NOT EXISTS media_art (
    title      text NOT NULL,
    artist     text NOT NULL,
    url        text NOT NULL,
    looked_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (title, artist)
);
-- 30-second preview clip; NULL for songs cached before previews were looked up
ALTER TABLE media_art ADD COLUMN IF NOT EXISTS preview text;

-- Sections: part of a drive the owner recorded on purpose ("record from now"
-- until "stop"), e.g. a twisty road without the drive there and back. end_at
-- is NULL while recording; only one can be open per car. summary is the
-- section's stats, filled in when it stops.
CREATE TABLE IF NOT EXISTS sections (
    id        bigserial PRIMARY KEY,
    vin       text NOT NULL,
    name      text NOT NULL DEFAULT '',
    start_at  timestamptz NOT NULL,
    end_at    timestamptz,
    summary   jsonb
);
CREATE UNIQUE INDEX IF NOT EXISTS sections_one_open ON sections (vin) WHERE end_at IS NULL;
CREATE INDEX IF NOT EXISTS sections_vin_start ON sections (vin, start_at);
