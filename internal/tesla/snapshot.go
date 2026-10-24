package tesla

import (
	"encoding/json"
	"fmt"
	"time"
)

const kmPerMile = 1.609344

// Snapshot is one vehicle_data sample, converted to metric. Pointer fields are nil
// when Tesla didn't return them (e.g. location without the vehicle_location scope).
type Snapshot struct {
	Time  time.Time
	VIN   string
	State string

	BatteryLevel   *int
	RangeKm        *float64
	ChargingState  string // Charging, Complete, Disconnected, Stopped, ...
	ChargeLimit    *int
	ChargerPowerKW *float64

	InsideTempC       *float64
	OutsideTempC      *float64
	ClimateOn         *bool
	DriverTempSetting *float64

	Latitude   *float64
	Longitude  *float64
	Heading    *int
	SpeedKmh   *float64
	ShiftState string // P, D, R, N, or "" when parked and asleep
	PowerKW    *float64

	OdometerKm *float64
	Locked     *bool
	Sentry     *bool
	CarVersion string

	Raw json.RawMessage
}

// Driving reports whether the car is in gear.
func (s *Snapshot) Driving() bool {
	return s.ShiftState == "D" || s.ShiftState == "R" || s.ShiftState == "N"
}

func (s *Snapshot) Charging() bool {
	return s.ChargingState == "Charging" || s.ChargingState == "Starting"
}

func (s *Snapshot) ClimateRunning() bool { return s.ClimateOn != nil && *s.ClimateOn }

// ParseVehicleData parses the "response" object of GET vehicle_data.
func ParseVehicleData(raw json.RawMessage) (*Snapshot, error) {
	var r struct {
		VIN         string `json:"vin"`
		State       string `json:"state"`
		ChargeState struct {
			BatteryLevel   *int     `json:"battery_level"`
			BatteryRange   *float64 `json:"battery_range"` // miles
			ChargingState  string   `json:"charging_state"`
			ChargeLimitSOC *int     `json:"charge_limit_soc"`
			ChargerPower   *float64 `json:"charger_power"`
		} `json:"charge_state"`
		ClimateState struct {
			InsideTemp        *float64 `json:"inside_temp"`
			OutsideTemp       *float64 `json:"outside_temp"`
			IsClimateOn       *bool    `json:"is_climate_on"`
			DriverTempSetting *float64 `json:"driver_temp_setting"`
		} `json:"climate_state"`
		DriveState struct {
			Latitude   *float64 `json:"latitude"`
			Longitude  *float64 `json:"longitude"`
			Heading    *int     `json:"heading"`
			Speed      *float64 `json:"speed"` // mph
			ShiftState *string  `json:"shift_state"`
			Power      *float64 `json:"power"`
			Timestamp  int64    `json:"timestamp"` // ms
		} `json:"drive_state"`
		VehicleState struct {
			Odometer   *float64 `json:"odometer"` // miles
			Locked     *bool    `json:"locked"`
			SentryMode *bool    `json:"sentry_mode"`
			CarVersion string   `json:"car_version"`
		} `json:"vehicle_state"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse vehicle_data: %w", err)
	}

	s := &Snapshot{
		Time:              time.Now().UTC(),
		VIN:               r.VIN,
		State:             r.State,
		BatteryLevel:      r.ChargeState.BatteryLevel,
		RangeKm:           scale(r.ChargeState.BatteryRange, kmPerMile),
		ChargingState:     r.ChargeState.ChargingState,
		ChargeLimit:       r.ChargeState.ChargeLimitSOC,
		ChargerPowerKW:    r.ChargeState.ChargerPower,
		InsideTempC:       r.ClimateState.InsideTemp,
		OutsideTempC:      r.ClimateState.OutsideTemp,
		ClimateOn:         r.ClimateState.IsClimateOn,
		DriverTempSetting: r.ClimateState.DriverTempSetting,
		Latitude:          r.DriveState.Latitude,
		Longitude:         r.DriveState.Longitude,
		Heading:           r.DriveState.Heading,
		SpeedKmh:          scale(r.DriveState.Speed, kmPerMile),
		PowerKW:           r.DriveState.Power,
		OdometerKm:        scale(r.VehicleState.Odometer, kmPerMile),
		Locked:            r.VehicleState.Locked,
		Sentry:            r.VehicleState.SentryMode,
		CarVersion:        r.VehicleState.CarVersion,
		Raw:               raw,
	}
	if r.DriveState.ShiftState != nil {
		s.ShiftState = *r.DriveState.ShiftState
	}
	if r.DriveState.Timestamp > 0 {
		s.Time = time.UnixMilli(r.DriveState.Timestamp).UTC()
	}
	return s, nil
}

func scale(v *float64, f float64) *float64 {
	if v == nil {
		return nil
	}
	x := *v * f
	return &x
}
