// Package assistant turns a short message like "warm it up, I'll be there in 15"
// into car actions. Claude reads the message and calls a few narrow tools; the
// planner (not the model) picks temperatures from the weather and the owner's
// preferences, and nothing here can unlock the car or open the charge port.
package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// Actions is what the assistant may do. The web server implements it.
type Actions interface {
	Status(ctx context.Context) (string, error)
	Prepare(ctx context.Context, r PrepareRequest) (string, error)
	CancelPlans(ctx context.Context) (string, error)
	ClimateOff(ctx context.Context) (string, error)
}

// PrepareRequest is the prepare_cabin tool input. Unset fields mean "decide for me".
type PrepareRequest struct {
	MinutesFromNow *int     `json:"minutes_from_now,omitempty" jsonschema_description:"Minutes until the owner gets in the car. 0 means right now. Use this or leave_at."`
	LeaveAt        string   `json:"leave_at,omitempty" jsonschema_description:"Clock time the owner gets in the car as 24-hour HH:MM in local time. Use this or minutes_from_now."`
	Want           string   `json:"want" jsonschema:"required,enum=heat,enum=cool,enum=auto" jsonschema_description:"heat if they asked to warm the car up and cool if they asked to cool it down. auto if they just want it ready."`
	TempC          *float64 `json:"temp_c,omitempty" jsonschema_description:"Only when the owner names a temperature in Celsius."`
	SeatHeat       *bool    `json:"seat_heat,omitempty" jsonschema_description:"Only when the owner explicitly asks for or against seat heat."`
	SteeringHeat   *bool    `json:"steering_heat,omitempty" jsonschema_description:"Only when the owner explicitly asks for or against steering wheel heat."`
}

type empty struct{}

const systemPrompt = `You control the climate of the owner's Tesla through tools. Messages usually come from Siri dictation on the owner's phone, so they are brief and sometimes mis-transcribed; read them generously.

When the owner wants the car ready, work out when they will get in and whether they asked for heat, cooling, or just "ready" (auto), then call prepare_cabin once. The tool picks the temperature, seat heat and start time from the weather and the owner's saved preferences, so pass temp_c, seat_heat or steering_heat only when the owner explicitly asks for them. Vague timing like "in a bit" or "after this set" means about 10 minutes.

Use get_car_status for questions about the car (battery, range, locks, how warm it is). Use cancel_plans to cancel a scheduled warm-up and climate_off to stop climate that is already running. If they ask for something none of the tools do, say in one sentence what you can do instead.

Siri reads your reply aloud. Write one or two short plain sentences with no markdown or emoji, and say times like "6:15". Base the reply on what the tools returned: say what will happen and when, and if the tool says the weather changed the plan, mention it briefly.`

// fallbackModels accept server-side refusal fallbacks; others (e.g. Haiku) reject the parameter.
var fallbackModels = map[string]bool{
	"claude-opus-5-5": true, "claude-opus-5": true, "claude-fable-5-1": true, "claude-sonnet-5-5": true,
}

type Assistant struct {
	client  anthropic.Client
	model   string
	actions Actions
	loc     *time.Location
}

// New returns nil when apiKey is empty, so callers can treat the assistant as optional.
// opts are extra client options (tests point the client at a fake server).
func New(apiKey, model string, actions Actions, loc *time.Location, opts ...option.RequestOption) *Assistant {
	if apiKey == "" {
		return nil
	}
	if model == "" {
		model = "claude-opus-5-5"
	}
	return &Assistant{
		// Siri gives up on a request after about a minute, so keep each call short.
		client: anthropic.NewClient(append([]option.RequestOption{option.WithAPIKey(apiKey),
			option.WithRequestTimeout(25 * time.Second), option.WithMaxRetries(1)}, opts...)...),
		model:   model,
		actions: actions,
		loc:     loc,
	}
}

func (a *Assistant) Model() string { return a.model }

// Handle answers one message and returns the reply to speak.
func (a *Assistant) Handle(ctx context.Context, message string) (string, error) {
	tools, err := a.tools()
	if err != nil {
		return "", err
	}
	now := time.Now().In(a.loc)
	user := fmt.Sprintf("Local time: %s.\n\nOwner's message: %s", now.Format("Monday 15:04"), message)

	params := anthropic.BetaMessageNewParams{
		Model:        anthropic.Model(a.model),
		MaxTokens:    4000,
		System:       []anthropic.BetaTextBlockParam{{Text: systemPrompt}},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortLow},
		Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
	}
	if fallbackModels[a.model] {
		// If a safety classifier declines, retry on a fallback model instead of failing.
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	runner := a.client.Beta.Messages.NewToolRunner(tools, anthropic.BetaToolRunnerParams{BetaMessageNewParams: params, MaxIterations: 5})
	msg, err := runner.RunToCompletion(ctx)
	if err != nil {
		return "", err
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return "Sorry, I can't help with that one.", nil
	}
	var parts []string
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok && strings.TrimSpace(t.Text) != "" {
			parts = append(parts, strings.TrimSpace(t.Text))
		}
	}
	if len(parts) == 0 {
		return "", errors.New("the assistant returned no reply")
	}
	return strings.Join(parts, " "), nil
}

func (a *Assistant) tools() ([]anthropic.BetaTool, error) {
	status, err := toolrunner.NewBetaToolFromJSONSchema("get_car_status",
		"Current battery, range, cabin and outside temperature, lock state, whether climate is on, and any scheduled warm-ups.",
		func(ctx context.Context, _ empty) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return text(a.actions.Status(ctx))
		})
	if err != nil {
		return nil, err
	}
	prepare, err := toolrunner.NewBetaToolFromJSONSchema("prepare_cabin",
		"Schedule the climate so the cabin is comfortable when the owner gets in. Returns what was scheduled, including when climate starts and why.",
		func(ctx context.Context, r PrepareRequest) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return text(a.actions.Prepare(ctx, r))
		})
	if err != nil {
		return nil, err
	}
	cancel, err := toolrunner.NewBetaToolFromJSONSchema("cancel_plans",
		"Cancel every scheduled warm-up or cool-down that hasn't started yet.",
		func(ctx context.Context, _ empty) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return text(a.actions.CancelPlans(ctx))
		})
	if err != nil {
		return nil, err
	}
	off, err := toolrunner.NewBetaToolFromJSONSchema("climate_off",
		"Turn the car's climate off now.",
		func(ctx context.Context, _ empty) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return text(a.actions.ClimateOff(ctx))
		})
	if err != nil {
		return nil, err
	}
	return []anthropic.BetaTool{status, prepare, cancel, off}, nil
}

func text(s string, err error) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	if err != nil {
		return anthropic.BetaToolResultBlockParamContentUnion{}, err
	}
	return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: s}}, nil
}
