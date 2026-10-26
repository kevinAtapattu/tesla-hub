package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	kmPerMile = 1.609344
	Gravity   = 9.80665 // m/s² per g
)

// Point is one telemetry record, converted to metric. A record only carries the
// fields that changed, so most pointers are nil; Data keeps every field as sent.
type Point struct {
	Time         time.Time
	VIN          string
	Resend       bool
	SpeedKmh     *float64
	Latitude     *float64
	Longitude    *float64
	Heading      *float64
	Gear         string // P, R, N, D or ""
	Soc          *float64
	OdometerKm   *float64
	PowerKW      *float64 // battery output; negative while regenerating or charging
	LatAccelMS2  *float64
	LongAccelMS2 *float64
	Pedal        *float64
	RangeKm      *float64
	InsideC      *float64
	OutsideC     *float64
	Data         map[string]any
}

// Store is where received telemetry goes.
type Store interface {
	InsertTelemetry(ctx context.Context, p *Point) error
	InsertTelemetryEvent(ctx context.Context, vin, kind string, t time.Time, data json.RawMessage) error
	CountRequests(ctx context.Context, kind string, n int) error
}

// Receiver subscribes to the fleet-telemetry server's ZMQ feed and stores what arrives.
type Receiver struct {
	Addr    string // e.g. tcp://telemetry:5284
	Store   Store
	OnPoint func(*Point) // called with each stored record; must not block

	mu     sync.Mutex
	status Status
	packV  map[string]float64 // last pack voltage per VIN, for power when only current changes
}

type Status struct {
	Connected   bool      // subscribed to the fleet-telemetry server
	LastRecord  time.Time // last vehicle data record
	Records     int       // since the hub started
	CarStatus   string    // last connectivity event from the car, e.g. CONNECTED
	CarStatusAt time.Time
	LastError   string // last error the car reported
	LastErrorAt time.Time
}

func (r *Receiver) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Receiver) update(f func(*Status)) {
	r.mu.Lock()
	f(&r.status)
	r.mu.Unlock()
}

// Run keeps a subscription open until ctx ends, reconnecting after failures.
func (r *Receiver) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.subscribe(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("telemetry feed", "err", err)
		}
		r.update(func(s *Status) { s.Connected = false })
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
}

func (r *Receiver) subscribe(ctx context.Context) error {
	sub := zmq4.NewSub(ctx)
	defer sub.Close()
	if err := sub.Dial(r.Addr); err != nil {
		return err
	}
	if err := sub.SetOption(zmq4.OptionSubscribe, ""); err != nil {
		return err
	}
	r.update(func(s *Status) { s.Connected = true })
	slog.Info("telemetry feed connected", "addr", r.Addr)
	for {
		msg, err := sub.Recv()
		if err != nil {
			return err
		}
		if len(msg.Frames) < 2 {
			continue
		}
		r.handle(ctx, string(msg.Frames[0]), msg.Frames[1])
	}
}

// handle routes one message by topic: <namespace>_V, _connectivity, _errors, _alerts.
func (r *Receiver) handle(ctx context.Context, topic string, payload []byte) {
	kind := topic[strings.LastIndex(topic, "_")+1:]
	switch kind {
	case "V":
		p, err := r.Decode(payload)
		if err != nil {
			slog.Warn("telemetry decode", "err", err)
			return
		}
		if err := r.Store.InsertTelemetry(ctx, p); err != nil {
			slog.Error("store telemetry", "err", err)
		}
		if r.OnPoint != nil {
			r.OnPoint(p)
		}
		// Each value received is one billable "streaming signal".
		r.Store.CountRequests(ctx, "signal", len(p.Data))
		r.update(func(s *Status) { s.LastRecord, s.Records = p.Time, s.Records+1 })
	case "connectivity":
		var c protos.VehicleConnectivity
		if err := protojson.Unmarshal(payload, &c); err == nil {
			t := c.GetCreatedAt().AsTime()
			r.update(func(s *Status) { s.CarStatus, s.CarStatusAt = c.GetStatus().String(), t })
			r.Store.InsertTelemetryEvent(ctx, c.GetVin(), kind, t, payload)
			slog.Info("telemetry connectivity", "status", c.GetStatus().String())
		}
	case "errors":
		var e protos.VehicleErrors
		if err := protojson.Unmarshal(payload, &e); err == nil {
			for _, ve := range e.GetErrors() {
				msg := ve.GetName() + ": " + ve.GetBody()
				r.update(func(s *Status) { s.LastError, s.LastErrorAt = msg, ve.GetCreatedAt().AsTime() })
			}
			r.Store.InsertTelemetryEvent(ctx, e.GetVin(), kind, time.Now(), payload)
		}
	case "alerts":
		var a protos.VehicleAlerts
		if err := protojson.Unmarshal(payload, &a); err == nil {
			r.Store.InsertTelemetryEvent(ctx, a.GetVin(), kind, time.Now(), payload)
		}
	}
}

// Decode turns a decoded vehicle data record (protojson) into a metric Point.
func (r *Receiver) Decode(payload []byte) (*Point, error) {
	var pl protos.Payload
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, &pl); err != nil {
		return nil, err
	}
	p := &Point{Time: pl.GetCreatedAt().AsTime(), VIN: pl.GetVin(), Resend: pl.GetIsResend(), Data: map[string]any{}}
	var current *float64
	for _, d := range pl.GetData() {
		v := d.GetValue()
		name := d.GetKey().String()
		switch d.GetKey() {
		case protos.Field_Location:
			if loc := v.GetLocationValue(); loc != nil {
				lat, lon := loc.GetLatitude(), loc.GetLongitude()
				p.Latitude, p.Longitude = &lat, &lon
				p.Data[name] = map[string]float64{"lat": lat, "lon": lon}
			}
			continue
		case protos.Field_Gear:
			p.Gear = strings.TrimPrefix(v.GetShiftStateValue().String(), "ShiftState")
			if p.Gear == "Unknown" || p.Gear == "Invalid" || p.Gear == "SNA" {
				p.Gear = ""
			}
			p.Data[name] = p.Gear
			continue
		}
		f, ok := number(v)
		if !ok {
			p.Data[name] = valueString(v)
			continue
		}
		p.Data[name] = f
		switch d.GetKey() {
		case protos.Field_VehicleSpeed:
			p.SpeedKmh = ptr(f * kmPerMile)
		case protos.Field_Odometer:
			p.OdometerKm = ptr(f * kmPerMile)
		case protos.Field_EstBatteryRange:
			p.RangeKm = ptr(f * kmPerMile)
		case protos.Field_GpsHeading:
			p.Heading = ptr(f)
		case protos.Field_Soc:
			p.Soc = ptr(f)
		case protos.Field_LateralAcceleration:
			p.LatAccelMS2 = ptr(f)
		case protos.Field_LongitudinalAcceleration:
			p.LongAccelMS2 = ptr(f)
		case protos.Field_PedalPosition:
			p.Pedal = ptr(f)
		case protos.Field_InsideTemp:
			p.InsideC = ptr(f)
		case protos.Field_OutsideTemp:
			p.OutsideC = ptr(f)
		case protos.Field_PackVoltage:
			r.mu.Lock()
			if r.packV == nil {
				r.packV = map[string]float64{}
			}
			r.packV[p.VIN] = f
			r.mu.Unlock()
		case protos.Field_PackCurrent:
			current = ptr(f)
		}
	}
	if current != nil {
		r.mu.Lock()
		volts, ok := r.packV[p.VIN]
		r.mu.Unlock()
		if ok {
			// The car reports discharge current as negative; PowerKW is positive when driving.
			p.PowerKW = ptr(-volts * *current / 1000)
		}
	}
	return p, nil
}

// number reads numeric values, including the older string-encoded ones.
func number(v *protos.Value) (float64, bool) {
	switch x := v.GetValue().(type) {
	case *protos.Value_DoubleValue:
		return x.DoubleValue, true
	case *protos.Value_FloatValue:
		return float64(x.FloatValue), true
	case *protos.Value_IntValue:
		return float64(x.IntValue), true
	case *protos.Value_LongValue:
		return float64(x.LongValue), true
	case *protos.Value_StringValue:
		f, err := strconv.ParseFloat(x.StringValue, 64)
		return f, err == nil
	}
	return 0, false
}

func valueString(v *protos.Value) any {
	switch x := v.GetValue().(type) {
	case *protos.Value_BooleanValue:
		return x.BooleanValue
	case *protos.Value_StringValue:
		return x.StringValue
	case *protos.Value_Invalid:
		return nil
	}
	// Enums and other typed values: the oneof's JSON form is readable enough to keep.
	b, err := protojson.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		for _, val := range m {
			return val
		}
	}
	return string(b)
}

func ptr(f float64) *float64 { return &f }
