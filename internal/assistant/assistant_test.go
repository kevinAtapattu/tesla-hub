package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

type fakeActions struct{ prepared []PrepareRequest }

func (f *fakeActions) Status(context.Context) (string, error) { return "Battery 80%.", nil }
func (f *fakeActions) Prepare(_ context.Context, r PrepareRequest) (string, error) {
	f.prepared = append(f.prepared, r)
	return "It's 3° outside. Heating to 23° with seat heat, starting at 6:05 PM. Ready by 18:15.", nil
}
func (f *fakeActions) CancelPlans(context.Context) (string, error) {
	return "Cancelled 1 scheduled plan(s).", nil
}
func (f *fakeActions) ClimateOff(context.Context) (string, error) {
	return "Turning the climate off now.", nil
}

// TestHandleRunsTools plays the Messages API: the first reply calls
// prepare_cabin, the second (after the tool result) is the spoken answer.
func TestHandleRunsTools(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use",
				"content":[{"type":"tool_use","id":"tu_1","name":"prepare_cabin","input":{"want":"heat","minutes_from_now":15}}],
				"usage":{"input_tokens":10,"output_tokens":10}}`)
			return
		}
		io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"m","stop_reason":"end_turn",
			"content":[{"type":"text","text":"Warming to 23° with seat heat. It'll be ready at 6:15."}],
			"usage":{"input_tokens":10,"output_tokens":10}}`)
	}))
	defer srv.Close()

	acts := &fakeActions{}
	a := New("test-key", "", acts, time.UTC, option.WithBaseURL(srv.URL))
	reply, err := a.Handle(context.Background(), "warm it up I'll be there in 15")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "ready at 6:15") {
		t.Errorf("reply = %q", reply)
	}
	if len(acts.prepared) != 1 || acts.prepared[0].Want != "heat" || acts.prepared[0].MinutesFromNow == nil || *acts.prepared[0].MinutesFromNow != 15 {
		t.Fatalf("prepare calls = %+v", acts.prepared)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d API calls, want 2", len(requests))
	}
	if requests[0]["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v", requests[0]["model"])
	}
	// The tool result goes back to the model in the second request.
	if b, _ := json.Marshal(requests[1]["messages"]); !strings.Contains(string(b), "Heating to 23") {
		t.Errorf("second request lacks the tool result: %s", b)
	}
}

func TestNewWithoutKeyIsOff(t *testing.T) {
	if New("", "", &fakeActions{}, time.UTC) != nil {
		t.Fatal("expected nil assistant without an API key")
	}
}
