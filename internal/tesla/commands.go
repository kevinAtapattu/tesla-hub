package tesla

import (
	"context"
	"fmt"
	"time"

	"github.com/teslamotors/vehicle-command/pkg/account"
	"github.com/teslamotors/vehicle-command/pkg/protocol"
	"github.com/teslamotors/vehicle-command/pkg/vehicle"
)

// Commander sends signed commands using Tesla's vehicle-command library. The car
// only accepts them if the public key matching PrivateKeyPath has been paired
// (https://tesla.com/_ak/<your domain>).
type Commander struct {
	Fleet          *Client
	PrivateKeyPath string
}

type ClimateRequest struct {
	TempC        float32 // 0 means leave the current setting
	SeatHeat     bool    // driver seat heater to high
	SteeringHeat bool
}

func (c *Commander) ClimateOn(ctx context.Context, vin string, req ClimateRequest) error {
	return c.run(ctx, vin, func(ctx context.Context, car *vehicle.Vehicle) error {
		if req.TempC > 0 {
			if err := car.ChangeClimateTemp(ctx, req.TempC, req.TempC); err != nil {
				return fmt.Errorf("set temperature: %w", err)
			}
			c.count()
		}
		if err := car.ClimateOn(ctx); err != nil {
			return fmt.Errorf("climate on: %w", err)
		}
		c.count()
		// Seat and wheel heat only apply once climate is running; failures here aren't fatal.
		if req.SeatHeat {
			_ = car.SetSeatHeater(ctx, map[vehicle.SeatPosition]vehicle.Level{vehicle.SeatFrontLeft: vehicle.LevelHigh})
			c.count()
		}
		if req.SteeringHeat {
			_ = car.SetSteeringWheelHeater(ctx, true)
			c.count()
		}
		return nil
	})
}

func (c *Commander) ClimateOff(ctx context.Context, vin string) error {
	return c.run(ctx, vin, func(ctx context.Context, car *vehicle.Vehicle) error {
		c.count()
		return car.ClimateOff(ctx)
	})
}

// simpleCommands are one-shot actions with no parameters, keyed by the name the web UI posts.
var simpleCommands = map[string]func(*vehicle.Vehicle, context.Context) error{
	"lock":         (*vehicle.Vehicle).Lock,
	"unlock":       (*vehicle.Vehicle).Unlock,
	"flash":        (*vehicle.Vehicle).FlashLights,
	"honk":         (*vehicle.Vehicle).HonkHorn,
	"charge_start": (*vehicle.Vehicle).ChargeStart,
	"charge_stop":  (*vehicle.Vehicle).ChargeStop,
	"port_open":    (*vehicle.Vehicle).ChargePortOpen,
	"port_close":   (*vehicle.Vehicle).ChargePortClose,
	"sentry_on":    func(v *vehicle.Vehicle, ctx context.Context) error { return v.SetSentryMode(ctx, true) },
	"sentry_off":   func(v *vehicle.Vehicle, ctx context.Context) error { return v.SetSentryMode(ctx, false) },
}

// Simple runs one of simpleCommands by name.
func (c *Commander) Simple(ctx context.Context, vin, name string) error {
	cmd, ok := simpleCommands[name]
	if !ok {
		return fmt.Errorf("unknown command %q", name)
	}
	return c.run(ctx, vin, func(ctx context.Context, car *vehicle.Vehicle) error {
		c.count()
		return cmd(car, ctx)
	})
}

// run wakes the car if needed, opens an authenticated session, then calls fn.
func (c *Commander) run(ctx context.Context, vin string, fn func(context.Context, *vehicle.Vehicle) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	v, err := c.Fleet.Vehicle(ctx, vin)
	if err != nil {
		return err
	}
	if v.State != "online" {
		if err := c.Fleet.WakeAndWait(ctx, vin, 75*time.Second); err != nil {
			return err
		}
	}

	key, err := protocol.LoadPrivateKey(c.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("load private key %s: %w", c.PrivateKeyPath, err)
	}
	tok, err := c.Fleet.Auth.AccessToken(ctx)
	if err != nil {
		return err
	}
	// The account is built per command because it captures the token string.
	acct, err := account.New(tok, "tesla-hub/0.1")
	if err != nil {
		return err
	}
	car, err := acct.GetVehicle(ctx, vin, key, nil)
	if err != nil {
		return err
	}
	if err := car.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer car.Disconnect()
	if err := car.StartSession(ctx, nil); err != nil {
		return fmt.Errorf("start session (is the virtual key paired?): %w", err)
	}
	return fn(ctx, car)
}

func (c *Commander) count() {
	if c.Fleet.OnRequest != nil {
		c.Fleet.OnRequest("command")
	}
}
