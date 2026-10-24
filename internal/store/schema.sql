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
