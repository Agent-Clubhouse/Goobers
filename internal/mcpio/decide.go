package mcpio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

const (
	decideToolName      = "decide"
	maxDecideStateBytes = 256 << 10
	maxDecideTextBytes  = 2 << 10
	decideCallTimeout   = 30 * time.Second
)

// DecideAccess is the trusted launcher's grant of the decide tool to one
// stage. It carries only environment-variable NAMES and thresholds, never a
// credential: the server resolves the endpoint and identity from its own
// process environment, which the agent cannot read. Denied by default: a nil
// grant, or a gate that is off, exposes no tool.
type DecideAccess struct {
	Settings decisiongate.Settings `json:"settings"`
	// MaxCalls bounds model calls for this stage invocation.
	MaxCalls int `json:"maxCalls"`
}

// Validate rejects a grant that cannot be honored.
func (a DecideAccess) Validate() error {
	if a.MaxCalls < 1 || a.MaxCalls > decisiongate.MaxAgentToolMaxCalls {
		return fmt.Errorf("mcpio: decide maxCalls must be within 1..%d", decisiongate.MaxAgentToolMaxCalls)
	}
	if a.Settings.EffectiveMode() == decisiongate.ModeOff {
		return errors.New("mcpio: decide grant requires decisionGate shadow or enforce")
	}
	if len(a.Settings.Gate.Thresholds) == 0 {
		return errors.New("mcpio: decide grant requires at least one configured question threshold")
	}
	if err := a.Settings.Validate(); err != nil {
		return fmt.Errorf("mcpio: decide grant: %w", err)
	}
	return nil
}

// DecideToolNames returns the tool names a valid grant exposes.
func DecideToolNames(a *DecideAccess) []string {
	if a == nil || a.Validate() != nil {
		return nil
	}
	return []string{decideToolName}
}

type decideState struct {
	mu    sync.Mutex
	gate  *decisiongate.Gate
	err   error
	ready bool
	calls int
	// digest is the last call's state digest; MCP calls are served serially.
	digest string
}

func (t *Toolset) recordDecideDigest(d string) {
	t.decide.mu.Lock()
	t.decide.digest = d
	t.decide.mu.Unlock()
}

func (t *Toolset) takeDecideDigest() string {
	t.decide.mu.Lock()
	defer t.decide.mu.Unlock()
	d := t.decide.digest
	t.decide.digest = ""
	return d
}

func (t *Toolset) decideQuestionNames() []string {
	names := make([]string, 0, len(t.cfg.Decide.Settings.Gate.Thresholds))
	for name := range t.cfg.Decide.Settings.Gate.Thresholds {
		if name == decisiongate.ClaimQuestion || name == decisiongate.PublicationLeakQuestion {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func decideToolDef(names []string) toolDef {
	text := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "maxLength": maxDecideTextBytes, "description": desc}
	}
	name := map[string]interface{}{"type": "string", "description": "A configured question name."}
	if len(names) > 0 {
		name["enum"] = names
	}
	return toolDef{
		Name: decideToolName,
		Description: "Ask the instance's decision model one yes/no question about a JSON state and get a thresholded, repeatable answer (yes, no or uncertain). " +
			"Use it only for semantic judgments, never to check structure or facts that code can verify. Calls are budgeted; an uncertain answer is not a no, so fall back to your own judgment or escalate. " +
			"In shadow mode the answer is always uncertain.",
		InputSchema: childToolSchema([]string{"name", "question", "state"}, map[string]interface{}{
			"name":     name,
			"question": text("The yes/no question."),
			"true":     text("What yes means (optional)."),
			"false":    text("What no means (optional)."),
			"state":    map[string]interface{}{"type": "object", "description": "JSON document the question is about, at most 256 KiB."},
		}),
	}
}

type decideArgs struct {
	Name     string          `json:"name"`
	Question string          `json:"question"`
	True     string          `json:"true"`
	False    string          `json:"false"`
	State    json.RawMessage `json:"state"`
}

func (s *Server) callDecideTool(raw json.RawMessage) (map[string]interface{}, error) {
	t := s.tools
	if len(DecideToolNames(t.cfg.Decide)) == 0 {
		return nil, errors.New("decide is unavailable for this stage")
	}
	var args decideArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil || dec.More() {
		return nil, errors.New("invalid decide arguments")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("one decide argument object required")
	}
	if _, configured := t.cfg.Decide.Settings.Gate.Thresholds[args.Name]; !configured || args.Name == decisiongate.ClaimQuestion || args.Name == decisiongate.PublicationLeakQuestion {
		return nil, errors.New("name must be a configured decide question: " + strings.Join(t.decideQuestionNames(), ", "))
	}
	if strings.TrimSpace(args.Question) == "" || len(args.Question) > maxDecideTextBytes || len(args.True) > maxDecideTextBytes || len(args.False) > maxDecideTextBytes {
		return nil, errors.New("question is required; question, true and false are each at most 2048 bytes")
	}
	if len(args.State) == 0 || len(args.State) > maxDecideStateBytes {
		return nil, errors.New("state is required and must be at most 262144 bytes")
	}
	var state any
	if err := json.Unmarshal(args.State, &state); err != nil {
		return nil, errors.New("state must be valid JSON")
	}

	gate, err := t.decideGate()
	if err != nil {
		return nil, errors.New("decide is unavailable: the decision gate could not be initialised")
	}
	remaining, ok := t.reserveDecideCall()
	if !ok {
		return nil, fmt.Errorf("decide call budget of %d for this stage is exhausted", t.cfg.Decide.MaxCalls)
	}

	q := decider.Noul(args.Question, &decider.NoulCriteria{True: args.True, False: args.False})
	ctx, cancel := context.WithTimeout(context.Background(), decideCallTimeout)
	defer cancel()
	outcome, jerr := gate.JudgeNoul(ctx, args.Name, state, q)

	mode := t.cfg.Decide.Settings.EffectiveMode()
	result := map[string]interface{}{"decision": string(outcome.Decision), "mode": string(mode), "callsRemaining": remaining}
	if jerr == nil {
		result["probability"] = strconv.FormatFloat(outcome.Probability, 'f', 4, 64)
	}
	if mode == decisiongate.ModeShadow || jerr != nil {
		result["decision"] = string(decisiongate.Uncertain)
	}
	if d := t.takeDecideDigest(); d != "" {
		result["stateDigest"] = d
	}
	if result["decision"] == string(decisiongate.Uncertain) {
		result["fallback"] = t.cfg.Decide.Settings.Fallback
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, errors.New("cannot encode decide result")
	}
	return textResult(string(data)), nil
}

// decideGate resolves the gate once from the server's own environment.
func (t *Toolset) decideGate() (*decisiongate.Gate, error) {
	st := &t.decide
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ready {
		return st.gate, st.err
	}
	st.ready = true
	settings := t.cfg.Decide.Settings
	resolve := t.resolveDecideGate
	if resolve == nil {
		resolve = func(s *decisiongate.Settings, observe func(decisiongate.Event)) (*decisiongate.Gate, error) {
			return s.Resolve(os.Getenv, observe)
		}
	}
	st.gate, st.err = resolve(&settings, func(e decisiongate.Event) { t.recordDecideDigest(e.StateDigest) })
	return st.gate, st.err
}

func (t *Toolset) reserveDecideCall() (int, bool) {
	st := &t.decide
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.calls >= t.cfg.Decide.MaxCalls {
		return 0, false
	}
	st.calls++
	return t.cfg.Decide.MaxCalls - st.calls, true
}
