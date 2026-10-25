package telemetry

import (
	"math"
	"testing"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func datum(f protos.Field, v *protos.Value) *protos.Datum { return &protos.Datum{Key: f, Value: v} }
func dbl(f float64) *protos.Value {
	return &protos.Value{Value: &protos.Value_DoubleValue{DoubleValue: f}}
}

// encode marshals like fleet-telemetry does with transmit_decoded_records.
func encode(t *testing.T, p *protos.Payload) []byte {
	b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeConvertsToMetric(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	r := &Receiver{}
	first := encode(t, &protos.Payload{Vin: "V1", CreatedAt: timestamppb.New(at), Data: []*protos.Datum{
		datum(protos.Field_PackVoltage, dbl(400)),
		datum(protos.Field_Gear, &protos.Value{Value: &protos.Value_ShiftStateValue{ShiftStateValue: protos.ShiftState_ShiftStateD}}),
	}})
	if _, err := r.Decode(first); err != nil {
		t.Fatal(err)
	}
	second := encode(t, &protos.Payload{Vin: "V1", CreatedAt: timestamppb.New(at.Add(time.Second)), Data: []*protos.Datum{
		datum(protos.Field_VehicleSpeed, dbl(62.137)),                                                      // mph
		datum(protos.Field_Odometer, &protos.Value{Value: &protos.Value_StringValue{StringValue: "1000"}}), // older string form
		datum(protos.Field_Location, &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: 43.69, Longitude: -79.85}}}),
		datum(protos.Field_LongitudinalAcceleration, dbl(-4.9)),
		datum(protos.Field_PackCurrent, dbl(150)), // amps, with the 400 V from the earlier record
	}})
	p, err := r.Decode(second)
	if err != nil {
		t.Fatal(err)
	}
	near := func(name string, got *float64, want float64) {
		t.Helper()
		if got == nil || math.Abs(*got-want) > 0.01 {
			t.Errorf("%s = %v, want %.2f", name, got, want)
		}
	}
	near("speed km/h", p.SpeedKmh, 100)
	near("odometer km", p.OdometerKm, 1609.34)
	near("latitude", p.Latitude, 43.69)
	near("longitude", p.Longitude, -79.85)
	near("braking m/s²", p.LongAccelMS2, -4.9)
	near("power kW", p.PowerKW, 60)
	if !p.Time.Equal(at.Add(time.Second)) || p.VIN != "V1" {
		t.Errorf("time/vin = %v %q", p.Time, p.VIN)
	}

	g, _ := r.Decode(first)
	if g.Gear != "D" {
		t.Errorf("gear = %q, want D", g.Gear)
	}
}
