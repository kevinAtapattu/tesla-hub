// Package store persists tokens, telemetry, rules and API usage in Postgres.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevin/tesla-hub/internal/telemetry"
	"github.com/kevin/tesla-hub/internal/tesla"
	"github.com/kevin/tesla-hub/internal/trips"
)

//go:embed schema.sql
var schema string

type Store struct {
	db *pgxpool.Pool
}

// Open connects and applies the schema, retrying while Postgres starts up.
func Open(ctx context.Context, url string) (*Store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		err = db.Ping(ctx)
		if err == nil || attempt == 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() { s.db.Close() }

// --- OAuth token (implements tesla.TokenStore) ---

func (s *Store) LoadToken(ctx context.Context) (*tesla.Token, error) {
	var t tesla.Token
	err := s.db.QueryRow(ctx, `SELECT access_token, refresh_token, expiry FROM oauth_token WHERE id = 1`).
		Scan(&t.AccessToken, &t.RefreshToken, &t.Expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

func (s *Store) SaveToken(ctx context.Context, t *tesla.Token) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO oauth_token (id, access_token, refresh_token, expiry, updated_at)
		VALUES (1, $1, $2, $3, now())
		ON CONFLICT (id) DO UPDATE SET access_token = $1, refresh_token = $2, expiry = $3, updated_at = now()`,
		t.AccessToken, t.RefreshToken, t.Expiry)
	return err
}

func (s *Store) DeleteToken(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM oauth_token`)
	return err
}

// --- Telemetry ---

func (s *Store) InsertSnapshot(ctx context.Context, v *tesla.Snapshot) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO vehicle_states (time, vin, state, battery_level, range_km, charging_state, charge_limit,
			charger_power_kw, inside_temp_c, outside_temp_c, climate_on, driver_temp_setting, latitude, longitude,
			heading, speed_kmh, shift_state, power_kw, odometer_km, locked, sentry, car_version, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		v.Time, v.VIN, v.State, v.BatteryLevel, v.RangeKm, v.ChargingState, v.ChargeLimit,
		v.ChargerPowerKW, v.InsideTempC, v.OutsideTempC, v.ClimateOn, v.DriverTempSetting, v.Latitude, v.Longitude,
		v.Heading, v.SpeedKmh, v.ShiftState, v.PowerKW, v.OdometerKm, v.Locked, v.Sentry, v.CarVersion, v.Raw)
	return err
}

// LatestSnapshot returns the most recent sample for vin, or nil if there is none.
func (s *Store) LatestSnapshot(ctx context.Context, vin string) (*tesla.Snapshot, error) {
	v := &tesla.Snapshot{VIN: vin}
	var charging, shift, version *string
	err := s.db.QueryRow(ctx, `
		SELECT time, battery_level, range_km, charging_state, charge_limit, charger_power_kw, inside_temp_c,
			outside_temp_c, climate_on, driver_temp_setting, latitude, longitude, speed_kmh, shift_state,
			odometer_km, locked, sentry, car_version, raw
		FROM vehicle_states WHERE vin = $1 ORDER BY time DESC LIMIT 1`, vin).
		Scan(&v.Time, &v.BatteryLevel, &v.RangeKm, &charging, &v.ChargeLimit, &v.ChargerPowerKW, &v.InsideTempC,
			&v.OutsideTempC, &v.ClimateOn, &v.DriverTempSetting, &v.Latitude, &v.Longitude, &v.SpeedKmh, &shift,
			&v.OdometerKm, &v.Locked, &v.Sentry, &version, &v.Raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.ChargingState, v.ShiftState, v.CarVersion = deref(charging), deref(shift), deref(version)
	// Fields without their own column (heading, charging details) come from the raw response.
	if full, err := tesla.ParseVehicleData(v.Raw); err == nil {
		v.Heading, v.EnergyAddedKWh, v.MinutesToFull, v.ChargeRateKmh =
			full.Heading, full.EnergyAddedKWh, full.MinutesToFull, full.ChargeRateKmh
	}
	return v, nil
}

// RecordPresence stores the online/asleep state when it differs from the last one.
func (s *Store) RecordPresence(ctx context.Context, vin, state string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO vehicle_presence (vin, state)
		SELECT $1, $2
		WHERE $2 IS DISTINCT FROM (SELECT state FROM vehicle_presence WHERE vin = $1 ORDER BY time DESC LIMIT 1)`,
		vin, state)
	return err
}

// --- API usage ---

func (s *Store) CountRequest(ctx context.Context, kind string) error {
	return s.CountRequests(ctx, kind, 1)
}

// CountRequests adds n billable units of kind (e.g. n streamed telemetry signals).
func (s *Store) CountRequests(ctx context.Context, kind string, n int) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO api_requests (month, kind, count) VALUES (date_trunc('month', now())::date, $1, $2)
		ON CONFLICT (month, kind) DO UPDATE SET count = api_requests.count + $2`, kind, n)
	return err
}

// MonthUsage returns this month's request counts by kind.
func (s *Store) MonthUsage(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.Query(ctx, `SELECT kind, count FROM api_requests WHERE month = date_trunc('month', now())::date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		usage[k] = n
	}
	return usage, rows.Err()
}

// --- Rules ---

type Rule struct {
	ID              int
	Name            string
	Enabled         bool
	Days            string // "1,2,3,4,5", 0=Sunday
	AtTime          string // "07:40"
	Latitude        *float64
	Longitude       *float64
	RadiusM         *float64
	MaxOutsideTempC *float64
	TempC           float64
	SeatHeat        bool
	SteeringHeat    bool
}

const ruleColumns = `id, name, enabled, days, at_time, latitude, longitude, radius_m, max_outside_temp_c, temp_c, seat_heat, steering_heat`

func scanRule(row pgx.Row) (*Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.Name, &r.Enabled, &r.Days, &r.AtTime, &r.Latitude, &r.Longitude, &r.RadiusM,
		&r.MaxOutsideTempC, &r.TempC, &r.SeatHeat, &r.SteeringHeat)
	return &r, err
}

func (s *Store) ListRules(ctx context.Context) ([]*Rule, error) {
	rows, err := s.db.Query(ctx, `SELECT `+ruleColumns+` FROM rules ORDER BY at_time, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []*Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (s *Store) CreateRule(ctx context.Context, r *Rule) error {
	return s.db.QueryRow(ctx, `
		INSERT INTO rules (name, enabled, days, at_time, latitude, longitude, radius_m, max_outside_temp_c, temp_c, seat_heat, steering_heat)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		r.Name, r.Enabled, r.Days, r.AtTime, r.Latitude, r.Longitude, r.RadiusM, r.MaxOutsideTempC, r.TempC,
		r.SeatHeat, r.SteeringHeat).Scan(&r.ID)
}

func (s *Store) SetRuleEnabled(ctx context.Context, id int, enabled bool) error {
	_, err := s.db.Exec(ctx, `UPDATE rules SET enabled = $2 WHERE id = $1`, id, enabled)
	return err
}

func (s *Store) DeleteRule(ctx context.Context, id int) error {
	_, err := s.db.Exec(ctx, `DELETE FROM rules WHERE id = $1`, id)
	return err
}

// ClaimRuleRun records that rule is running on localDate. It returns false if the
// rule already ran (or was skipped) that day, which makes firing idempotent.
func (s *Store) ClaimRuleRun(ctx context.Context, ruleID int, localDate time.Time, status, message string) (int, bool, error) {
	var id int
	err := s.db.QueryRow(ctx, `
		INSERT INTO rule_runs (rule_id, local_date, status, message) VALUES ($1, $2, $3, $4)
		ON CONFLICT (rule_id, local_date) DO NOTHING RETURNING id`,
		ruleID, localDate.Format("2006-01-02"), status, message).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (s *Store) FinishRuleRun(ctx context.Context, runID int, status, message string) error {
	_, err := s.db.Exec(ctx, `UPDATE rule_runs SET status = $2, message = $3 WHERE id = $1`, runID, status, message)
	return err
}

type RuleRun struct {
	RuleName string
	RanAt    time.Time
	Status   string
	Message  string
}

func (s *Store) RecentRuns(ctx context.Context, limit int) ([]RuleRun, error) {
	rows, err := s.db.Query(ctx, `
		SELECT r.name, rr.ran_at, rr.status, rr.message FROM rule_runs rr JOIN rules r ON r.id = rr.rule_id
		ORDER BY rr.ran_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []RuleRun
	for rows.Next() {
		var r RuleRun
		if err := rows.Scan(&r.RuleName, &r.RanAt, &r.Status, &r.Message); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- Charts ---

type Point struct {
	Time         time.Time
	BatteryLevel *int
	RangeKm      *float64
	InsideTempC  *float64
	OutsideTempC *float64
	SpeedKmh     *float64
	PowerKW      *float64
}

// Series returns samples for vin since the given time, oldest first.
func (s *Store) Series(ctx context.Context, vin string, since time.Time) ([]Point, error) {
	rows, err := s.db.Query(ctx, `
		SELECT time, battery_level, range_km, inside_temp_c, outside_temp_c, speed_kmh
		FROM vehicle_states WHERE vin = $1 AND time >= $2 ORDER BY time LIMIT 5000`, vin, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pts []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Time, &p.BatteryLevel, &p.RangeKm, &p.InsideTempC, &p.OutsideTempC, &p.SpeedKmh); err != nil {
			return nil, err
		}
		pts = append(pts, p)
	}
	return pts, rows.Err()
}

// DistanceSince is how far the odometer moved since the given time, or nil without data.
func (s *Store) DistanceSince(ctx context.Context, vin string, since time.Time) (*float64, error) {
	var d *float64
	err := s.db.QueryRow(ctx, `
		SELECT max(odometer_km) - min(odometer_km) FROM vehicle_states
		WHERE vin = $1 AND time >= $2 AND odometer_km IS NOT NULL`, vin, since).Scan(&d)
	return d, err
}

// --- Departures ---

type Departure struct {
	ID           int
	DepartAt     time.Time
	StartAt      time.Time
	TempC        float64
	SeatHeat     bool
	SteeringHeat bool
	Reason       string
	Status       string
	Message      string
}

const departureColumns = `id, depart_at, start_at, temp_c, seat_heat, steering_heat, reason, status, message`

func scanDepartures(rows pgx.Rows) ([]Departure, error) {
	defer rows.Close()
	var ds []Departure
	for rows.Next() {
		var d Departure
		if err := rows.Scan(&d.ID, &d.DepartAt, &d.StartAt, &d.TempC, &d.SeatHeat, &d.SteeringHeat, &d.Reason, &d.Status, &d.Message); err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

func (s *Store) CreateDeparture(ctx context.Context, d *Departure) error {
	if d.Status == "" {
		d.Status = "pending"
	}
	return s.db.QueryRow(ctx, `
		INSERT INTO departures (depart_at, start_at, temp_c, seat_heat, steering_heat, reason, status, message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		d.DepartAt, d.StartAt, d.TempC, d.SeatHeat, d.SteeringHeat, d.Reason, d.Status, d.Message).Scan(&d.ID)
}

// DueDepartures returns pending departures whose start time has arrived.
func (s *Store) DueDepartures(ctx context.Context, now time.Time) ([]Departure, error) {
	rows, err := s.db.Query(ctx, `SELECT `+departureColumns+` FROM departures
		WHERE status = 'pending' AND start_at <= $1 ORDER BY start_at`, now)
	if err != nil {
		return nil, err
	}
	return scanDepartures(rows)
}

// RecentDepartures returns pending ones plus anything from the last 12 hours, newest first.
func (s *Store) RecentDepartures(ctx context.Context) ([]Departure, error) {
	rows, err := s.db.Query(ctx, `SELECT `+departureColumns+` FROM departures
		WHERE status = 'pending' OR depart_at > now() - interval '12 hours' ORDER BY depart_at DESC LIMIT 10`)
	if err != nil {
		return nil, err
	}
	return scanDepartures(rows)
}

// ClaimDeparture moves a departure from pending to running; false if someone else got it first.
func (s *Store) ClaimDeparture(ctx context.Context, id int) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE departures SET status = 'running' WHERE id = $1 AND status = 'pending'`, id)
	return tag.RowsAffected() == 1, err
}

func (s *Store) FinishDeparture(ctx context.Context, id int, status, message string) error {
	_, err := s.db.Exec(ctx, `UPDATE departures SET status = $2, message = $3 WHERE id = $1`, id, status, message)
	return err
}

func (s *Store) CancelDeparture(ctx context.Context, id int) error {
	_, err := s.db.Exec(ctx, `UPDATE departures SET status = 'cancelled' WHERE id = $1 AND status = 'pending'`, id)
	return err
}

// CancelPendingDepartures cancels every plan that hasn't started and returns how many.
func (s *Store) CancelPendingDepartures(ctx context.Context) (int, error) {
	tag, err := s.db.Exec(ctx, `UPDATE departures SET status = 'cancelled' WHERE status = 'pending'`)
	return int(tag.RowsAffected()), err
}

// --- settings ---

// GetSetting decodes the setting into v. It reports false when the key isn't set.
func (s *Store) GetSetting(ctx context.Context, key string, v any) (bool, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

func (s *Store) SetSetting(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, raw)
	return err
}

// --- assistant log ---

type AssistantEntry struct {
	Time    time.Time
	Message string
	Reply   string
	Error   string
}

func (s *Store) LogAssistant(ctx context.Context, e AssistantEntry) error {
	_, err := s.db.Exec(ctx, `INSERT INTO assistant_log (message, reply, error) VALUES ($1, $2, $3)`, e.Message, e.Reply, e.Error)
	return err
}

func (s *Store) RecentAssistant(ctx context.Context, limit int) ([]AssistantEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT time, message, reply, error FROM assistant_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantEntry
	for rows.Next() {
		var e AssistantEntry
		if err := rows.Scan(&e.Time, &e.Message, &e.Reply, &e.Error); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- telemetry ---

func (s *Store) InsertTelemetry(ctx context.Context, p *telemetry.Point) error {
	data, err := json.Marshal(p.Data)
	if err != nil {
		return err
	}
	var gear *string
	if p.Gear != "" {
		gear = &p.Gear
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO telemetry (time, vin, resend, speed_kmh, latitude, longitude, heading, gear, soc, odometer_km,
			power_kw, lat_accel_ms2, long_accel_ms2, pedal, range_km, inside_temp_c, outside_temp_c, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		p.Time, p.VIN, p.Resend, p.SpeedKmh, p.Latitude, p.Longitude, p.Heading, gear, p.Soc, p.OdometerKm,
		p.PowerKW, p.LatAccelMS2, p.LongAccelMS2, p.Pedal, p.RangeKm, p.InsideC, p.OutsideC, data)
	return err
}

func (s *Store) InsertTelemetryEvent(ctx context.Context, vin, kind string, t time.Time, data json.RawMessage) error {
	_, err := s.db.Exec(ctx, `INSERT INTO telemetry_events (time, vin, kind, data) VALUES ($1, $2, $3, $4)`, t, vin, kind, data)
	return err
}

// --- trips ---

// TelemetrySamples returns telemetry records after the given time, oldest first.
// Fields a record didn't carry are nil; trips.Fill carries values forward.
func (s *Store) TelemetrySamples(ctx context.Context, vin string, after time.Time, limit int) ([]trips.Sample, error) {
	rows, err := s.db.Query(ctx, `
		SELECT time, coalesce(gear, ''), speed_kmh, latitude, longitude, odometer_km, soc, power_kw, lat_accel_ms2, long_accel_ms2
		FROM telemetry WHERE vin = $1 AND time > $2 AND NOT resend ORDER BY time LIMIT $3`, vin, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []trips.Sample
	for rows.Next() {
		var x trips.Sample
		if err := rows.Scan(&x.Time, &x.Gear, &x.SpeedKmh, &x.Latitude, &x.Longitude, &x.OdometerKm, &x.Soc, &x.PowerKW, &x.LatAccel, &x.LongAccel); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// FirstTelemetry is when streaming began for vin (zero if it never has).
func (s *Store) FirstTelemetry(ctx context.Context, vin string) (time.Time, error) {
	var t *time.Time
	err := s.db.QueryRow(ctx, `SELECT min(time) FROM telemetry WHERE vin = $1`, vin).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

// PollingSamples returns polled vehicle_data samples in [from, to), oldest first.
func (s *Store) PollingSamples(ctx context.Context, vin string, from, to time.Time) ([]trips.Sample, error) {
	rows, err := s.db.Query(ctx, `
		SELECT time, coalesce(shift_state, ''), speed_kmh, latitude, longitude, odometer_km, battery_level::double precision, power_kw
		FROM vehicle_states WHERE vin = $1 AND time > $2 AND time < $3 ORDER BY time`, vin, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []trips.Sample
	for rows.Next() {
		var x trips.Sample
		if err := rows.Scan(&x.Time, &x.Gear, &x.SpeedKmh, &x.Latitude, &x.Longitude, &x.OdometerKm, &x.Soc, &x.PowerKW); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// LastTripEnd is when the newest saved trip for vin ended (zero if none).
func (s *Store) LastTripEnd(ctx context.Context, vin string) (time.Time, error) {
	var t *time.Time
	err := s.db.QueryRow(ctx, `SELECT max(end_at) FROM trips WHERE vin = $1`, vin).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

func (s *Store) SaveTrip(ctx context.Context, vin, source string, t trips.Trip) error {
	summary, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO trips (vin, start_at, end_at, source, distance_km, summary) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (vin, start_at) DO NOTHING`, vin, t.Start, t.End, source, t.DistanceKm, summary)
	return err
}

// SavedTrip is a trip as stored.
type SavedTrip struct {
	ID     int64
	Source string
	trips.Trip
}

func (s *Store) ListTrips(ctx context.Context, vin string, limit int) ([]SavedTrip, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, source, summary - 'Path' FROM trips WHERE vin = $1 ORDER BY start_at DESC LIMIT $2`, vin, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedTrip
	for rows.Next() {
		var t SavedTrip
		var raw []byte
		if err := rows.Scan(&t.ID, &t.Source, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &t.Trip); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTrip returns one trip with its route, or nil if it doesn't exist.
func (s *Store) GetTrip(ctx context.Context, id int64) (*SavedTrip, error) {
	t := &SavedTrip{ID: id}
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT source, summary FROM trips WHERE id = $1`, id).Scan(&t.Source, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, json.Unmarshal(raw, &t.Trip)
}

// TripSamples returns the full-resolution telemetry for a time range, for a trip's charts.
func (s *Store) TripSamples(ctx context.Context, vin string, from, to time.Time) ([]trips.Sample, error) {
	all, err := s.TelemetrySamples(ctx, vin, from.Add(-time.Nanosecond), 20000)
	if err != nil {
		return nil, err
	}
	for i, x := range all {
		if x.Time.After(to) {
			return all[:i], nil
		}
	}
	return all, nil
}
