package mcpio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

type fakeDecider struct {
	p     float64
	calls int
}

func (f *fakeDecider) Decide(_ context.Context, r decider.Request) (decider.Response, error) {
	f.calls++
	answers := map[string]decider.Answer{}
	for name := range r.Questions {
		p := f.p
		answers[name] = decider.Answer{Yes: &p}
	}
	return decider.Response{Answers: answers}, nil
}

func decideGrant(mode decisiongate.Mode, maxCalls int) *DecideAccess {
	return &DecideAccess{
		MaxCalls: maxCalls,
		Settings: decisiongate.Settings{
			Mode: mode, BaseURLEnv: "A", ModelEnv: "B", Auth: decisiongate.AuthEntra, Fallback: decisiongate.FallbackAgent,
			Gate: decisiongate.Config{Thresholds: map[string]decisiongate.Threshold{"complete": {Accept: 0.9, Reject: 0.1}}},
		},
	}
}

func newDecideServer(t *testing.T, grant *DecideAccess, d *fakeDecider) *Server {
	t.Helper()
	server, _ := newTestServer(t)
	server.tools.cfg.Decide = grant
	server.tools.resolveDecideGate = func(s *decisiongate.Settings, observe func(decisiongate.Event)) (*decisiongate.Gate, error) {
		return decisiongate.New(d, s.Gate, observe)
	}
	return server
}

const decideCall = `{"name":"complete","question":"Is the report complete?","state":{"findings":[]}}`

func decideResultOf(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	if result["isError"] == true {
		t.Fatalf("unexpected tool error: %v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecideToolDeniedByDefault(t *testing.T) {
	server, _ := newTestServer(t)
	for _, def := range server.toolDefs() {
		if def.Name == "decide" {
			t.Fatal("ungranted decide tool advertised")
		}
	}
	result, _ := childRPC(t, server, "decide", json.RawMessage(decideCall))
	if result["isError"] != true {
		t.Fatal("ungranted decide succeeded")
	}
	if got := DecideToolNames(decideGrant(decisiongate.ModeOff, 3)); got != nil {
		t.Fatalf("gate-off grant exposes %v", got)
	}
}

func TestDecideEnforceBudgetAndDigestOnly(t *testing.T) {
	d := &fakeDecider{p: 0.97}
	server := newDecideServer(t, decideGrant(decisiongate.ModeEnforce, 2), d)
	advertised := false
	for _, def := range server.toolDefs() {
		advertised = advertised || def.Name == "decide"
	}
	if !advertised {
		t.Fatal("granted decide tool not advertised")
	}
	out := decideResultOf(t, firstResult(t, server, decideCall))
	if out["decision"] != "yes" || out["callsRemaining"].(float64) != 1 || out["stateDigest"] == "" {
		t.Fatalf("unexpected result %v", out)
	}
	if strings.Contains(out["stateDigest"].(string), "findings") {
		t.Fatal("state echoed in digest")
	}
	decideResultOf(t, firstResult(t, server, `{"name":"complete","question":"Is it done?","state":{"a":1}}`))
	budget, _ := childRPC(t, server, "decide", json.RawMessage(decideCall))
	if budget["isError"] != true || d.calls != 2 {
		t.Fatalf("budget not enforced: calls=%d", d.calls)
	}
}

func TestDecideShadowAlwaysUncertain(t *testing.T) {
	server := newDecideServer(t, decideGrant(decisiongate.ModeShadow, 3), &fakeDecider{p: 0.99})
	out := decideResultOf(t, firstResult(t, server, decideCall))
	if out["decision"] != "uncertain" || out["mode"] != "shadow" || out["fallback"] != "agent" {
		t.Fatalf("shadow leaked a decision: %v", out)
	}
}

func TestDecideRejectsUnconfiguredNameAndBadArgs(t *testing.T) {
	d := &fakeDecider{p: 0.5}
	server := newDecideServer(t, decideGrant(decisiongate.ModeEnforce, 5), d)
	for _, args := range []string{
		`{"name":"other","question":"q","state":{}}`,
		`{"name":"complete","question":"","state":{}}`,
		`{"name":"complete","question":"q"}`,
		`{"name":"complete","question":"q","state":{},"extra":1}`,
	} {
		if result, _ := childRPC(t, server, "decide", json.RawMessage(args)); result["isError"] != true {
			t.Fatalf("accepted %s", args)
		}
	}
	if d.calls != 0 {
		t.Fatal("invalid call reached the model")
	}
}

func firstResult(t *testing.T, server *Server, args string) map[string]any {
	t.Helper()
	result, _ := childRPC(t, server, "decide", json.RawMessage(args))
	return result
}
