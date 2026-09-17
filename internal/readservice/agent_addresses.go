package readservice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// AgentAddressSchema is the portable address format version for a live agent
// in one run.
const AgentAddressSchema = journal.AgentAddressSchema

const addressAgentTokenPrefix = "v1~"

// AgentAddressKind distinguishes a stage's top-level goober from an
// adapter-projected nested agent.
type AgentAddressKind string

const (
	// AgentAddressTopLevel identifies the stage's own top-level goober.
	AgentAddressTopLevel AgentAddressKind = "top-level"
	// AgentAddressNested identifies one adapter-projected nested agent.
	AgentAddressNested AgentAddressKind = "nested"
)

// AgentResolutionKind is the typed answer to "what would this address target
// if a caller attempted delivery right now?".
type AgentResolutionKind string

const (
	// AgentResolutionResolved means the address names one uniquely live agent.
	AgentResolutionResolved AgentResolutionKind = "resolved"
	// AgentResolutionMalformed means the portable address itself is invalid.
	AgentResolutionMalformed AgentResolutionKind = "malformed"
	// AgentResolutionHistorical means the address never belonged to this run.
	AgentResolutionHistorical AgentResolutionKind = "historical"
	// AgentResolutionStale means the run advanced beyond the addressed visit.
	AgentResolutionStale AgentResolutionKind = "stale"
	// AgentResolutionTerminated means the addressed visit exists but is no longer live.
	AgentResolutionTerminated AgentResolutionKind = "terminated"
	// AgentResolutionUnaddressable means multiple live agents share the address.
	AgentResolutionUnaddressable AgentResolutionKind = "unaddressable"
)

// AgentAddress is the stable run/stage/attempt/agent identity shared by a
// top-level goober and a normalized nested-agent record. String encodes it as
// a portable, versioned path with percent-escaped segments.
type AgentAddress struct {
	Schema  string `json:"schema"`
	RunID   string `json:"runId"`
	Stage   string `json:"stage"`
	Attempt int    `json:"attempt"`
	Agent   string `json:"agent"`
}

// Validate rejects malformed or unsupported addresses before a caller acts on
// them.
func (a AgentAddress) Validate() error {
	switch {
	case a.Schema == "":
		return fmt.Errorf("agent address schema is required")
	case a.Schema != AgentAddressSchema:
		return fmt.Errorf("agent address schema %q is unsupported", a.Schema)
	case !apiv1.ValidRunID(a.RunID):
		return fmt.Errorf("agent address run id %q is invalid", a.RunID)
	case a.Stage == "":
		return fmt.Errorf("agent address stage is required")
	case a.Attempt < 1:
		return fmt.Errorf("agent address attempt must be >= 1")
	case a.Agent == "":
		return fmt.Errorf("agent address agent is required")
	default:
		return nil
	}
}

// String returns the portable versioned text form.
func (a AgentAddress) String() string {
	return journal.AgentAddress{
		RunID:   a.RunID,
		Stage:   a.Stage,
		Attempt: a.Attempt,
		AgentID: a.Agent,
	}.String()
}

// ParseAgentAddress decodes the portable text form.
func ParseAgentAddress(raw string) (AgentAddress, error) {
	parsed, err := journal.ParseAgentAddress(strings.TrimSpace(raw))
	if err != nil {
		return AgentAddress{}, fmt.Errorf("parse agent address: %w", err)
	}
	return AgentAddress{
		Schema:  AgentAddressSchema,
		RunID:   parsed.RunID,
		Stage:   parsed.Stage,
		Attempt: parsed.Attempt,
		Agent:   parsed.AgentID,
	}, nil
}

// AddressableAgent is one currently deliverable live agent in the selected
// run. Enumeration excludes entries whose address would collide with another
// live agent.
type AddressableAgent struct {
	Address   AgentAddress     `json:"address"`
	Kind      AgentAddressKind `json:"kind"`
	ParentID  string           `json:"parentId,omitempty"`
	Lifecycle string           `json:"lifecycle,omitempty"`
}

// AgentResolution is the typed outcome of resolving one address against one
// selected run.
type AgentResolution struct {
	Kind    AgentResolutionKind `json:"kind"`
	Address *AgentAddress       `json:"address,omitempty"`
	Agent   *AddressableAgent   `json:"agent,omitempty"`
	Detail  string              `json:"detail,omitempty"`
}

type addressableStageRole struct {
	goober string
}

type encodedAddressAgent struct {
	rawAgent   string
	startedSeq uint64
}

type addressRecord struct {
	identity   string
	agent      AddressableAgent
	startedSeq uint64
	updatedAt  time.Time
}

type runAgentSnapshot struct {
	known         map[string][]addressRecord
	live          map[string]AddressableAgent
	collisions    map[string]int
	latestByStage map[string]StageAttempt
}

// AddressableAgents enumerates the currently addressable agents within the
// selected run only. It never opens sibling runs, so the answer is constrained
// to the chosen run even when the runs root contains other live work.
func (s *Local) AddressableAgents(ctx context.Context, runID string) ([]AddressableAgent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return nil, err
	}
	snapshot, err := addressableRunSnapshot(run)
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(snapshot.live))
	for address := range snapshot.live {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	out := make([]AddressableAgent, 0, len(addresses))
	for _, address := range addresses {
		out = append(out, snapshot.live[address])
	}
	return out, nil
}

// ResolveAgentAddress resolves raw against the selected run and reports a
// typed outcome without attempting delivery.
func (s *Local) ResolveAgentAddress(ctx context.Context, selectedRunID, raw string) (AgentResolution, error) {
	if err := ctx.Err(); err != nil {
		return AgentResolution{}, err
	}
	address, err := ParseAgentAddress(raw)
	if err != nil {
		return AgentResolution{
			Kind:   AgentResolutionMalformed,
			Detail: err.Error(),
		}, nil
	}
	if _, err := parseAddressAgent(address.Agent); err != nil {
		return AgentResolution{
			Kind:    AgentResolutionMalformed,
			Address: &address,
			Detail:  err.Error(),
		}, nil
	}
	if address.RunID != selectedRunID {
		return AgentResolution{
			Kind:    AgentResolutionHistorical,
			Address: &address,
			Detail:  fmt.Sprintf("address names run %q, selected run is %q", address.RunID, selectedRunID),
		}, nil
	}
	run, err := s.openRun(selectedRunID)
	if err != nil {
		return AgentResolution{}, err
	}
	snapshot, err := addressableRunSnapshot(run)
	if err != nil {
		return AgentResolution{}, err
	}
	return snapshot.resolve(address), nil
}

func addressableRunSnapshot(run runRead) (runAgentSnapshot, error) {
	def, err := pinnedWorkflowDefinition(run)
	if err != nil {
		return runAgentSnapshot{}, err
	}
	roles := addressableStageRoles(*def)
	events := recordEvents(run.records)
	attemptsByStage := collectStageAttempts(run.identity.RunID, run.records, artifactIndex{}, "")
	known := make(map[string][]addressRecord)
	latestByStage := make(map[string]StageAttempt, len(attemptsByStage))

	for stage, attempts := range attemptsByStage {
		role, ok := roles[stage]
		if !ok {
			continue
		}
		for _, attempt := range attempts {
			if attempt.StartedSeq > latestByStage[stage].StartedSeq {
				latestByStage[stage] = attempt
			}
			if attempt.StartedSeq == 0 {
				continue
			}
			agent := AddressableAgent{
				Address:   agentAddressFor(run.identity.RunID, stage, attempt.Number, role.goober, attempt.StartedSeq),
				Kind:      AgentAddressTopLevel,
				Lifecycle: attempt.Status,
			}
			record := addressRecord{
				identity:   "top-level\x00" + strconv.FormatUint(attempt.StartedSeq, 10),
				agent:      agent,
				startedSeq: attempt.StartedSeq,
			}
			key := agent.Address.String()
			known[key] = mergeAddressRecord(known[key], record)
		}
	}

	agentEvents, err := filterRunAgentEvents(events, run.identity.RunID)
	if err != nil {
		return runAgentSnapshot{}, err
	}
	for _, event := range agentEvents {
		attempt, ok := stageAttemptForAgentEvent(attemptsByStage[event.Agent.Stage], event)
		if !ok || attempt.StartedSeq == 0 {
			continue
		}
		agent := AddressableAgent{
			Address:   agentAddressFor(run.identity.RunID, event.Agent.Stage, event.Agent.Attempt, event.Agent.ID, attempt.StartedSeq),
			Kind:      AgentAddressNested,
			ParentID:  event.Agent.ParentID,
			Lifecycle: string(event.Agent.Lifecycle),
		}
		record := addressRecord{
			identity:   "nested\x00" + strconv.FormatUint(attempt.StartedSeq, 10) + "\x00" + event.Agent.ID,
			agent:      agent,
			startedSeq: attempt.StartedSeq,
			updatedAt:  event.Agent.UpdatedAt,
		}
		key := agent.Address.String()
		known[key] = mergeAddressRecord(known[key], record)
	}

	candidates := make(map[string][]AddressableAgent)
	for key, records := range known {
		for _, record := range records {
			latest, ok := latestByStage[record.agent.Address.Stage]
			if !ok || latest.StartedSeq != record.startedSeq || latest.Status != "running" {
				continue
			}
			if record.agent.Kind == AgentAddressNested && !agentLifecycleLive(record.agent.Lifecycle) {
				continue
			}
			candidates[key] = append(candidates[key], record.agent)
		}
	}

	live := make(map[string]AddressableAgent, len(candidates))
	collisions := make(map[string]int, len(candidates))
	for key, agents := range candidates {
		collisions[key] = len(agents)
		if len(agents) == 1 {
			live[key] = agents[0]
		}
	}

	return runAgentSnapshot{
		known:         known,
		live:          live,
		collisions:    collisions,
		latestByStage: latestByStage,
	}, nil
}

func (s runAgentSnapshot) resolve(address AgentAddress) AgentResolution {
	token, err := parseAddressAgent(address.Agent)
	if err != nil {
		return AgentResolution{
			Kind:    AgentResolutionMalformed,
			Address: &address,
			Detail:  err.Error(),
		}
	}
	key := address.String()
	if collisions := s.collisions[key]; collisions > 1 {
		return AgentResolution{
			Kind:    AgentResolutionUnaddressable,
			Address: &address,
			Detail:  "multiple live agents share this address",
		}
	}
	if live, ok := s.live[key]; ok {
		return AgentResolution{
			Kind:    AgentResolutionResolved,
			Address: &address,
			Agent:   &live,
		}
	}
	records := s.known[key]
	if len(records) == 0 {
		return AgentResolution{
			Kind:    AgentResolutionHistorical,
			Address: &address,
			Detail:  "address does not belong to the selected run's pinned workflow or nested-agent history",
		}
	}
	latest, ok := s.latestByStage[address.Stage]
	if !ok {
		return AgentResolution{
			Kind:    AgentResolutionHistorical,
			Address: &address,
			Detail:  "address does not belong to the selected run's pinned workflow or nested-agent history",
		}
	}
	if latest.StartedSeq > token.startedSeq {
		return AgentResolution{
			Kind:    AgentResolutionStale,
			Address: &address,
			Detail:  fmt.Sprintf("stage %q advanced to a later visit", address.Stage),
		}
	}
	for _, record := range records {
		if record.startedSeq != token.startedSeq {
			continue
		}
		if latest.Status != "running" {
			if record.agent.Kind == AgentAddressNested && !agentLifecycleLive(record.agent.Lifecycle) {
				return AgentResolution{
					Kind:    AgentResolutionTerminated,
					Address: &address,
					Detail:  fmt.Sprintf("nested agent reached lifecycle %q", record.agent.Lifecycle),
				}
			}
			return AgentResolution{
				Kind:    AgentResolutionTerminated,
				Address: &address,
				Detail:  fmt.Sprintf("stage %q is no longer running", address.Stage),
			}
		}
		if record.agent.Kind == AgentAddressTopLevel {
			return AgentResolution{
				Kind:    AgentResolutionTerminated,
				Address: &address,
				Detail:  "the addressed top-level agent is no longer running",
			}
		}
		if agentLifecycleLive(record.agent.Lifecycle) {
			return AgentResolution{
				Kind:    AgentResolutionUnaddressable,
				Address: &address,
				Detail:  "the addressed nested agent is live but not uniquely addressable",
			}
		}
		return AgentResolution{
			Kind:    AgentResolutionTerminated,
			Address: &address,
			Detail:  fmt.Sprintf("nested agent reached lifecycle %q", record.agent.Lifecycle),
		}
	}
	return AgentResolution{
		Kind:    AgentResolutionHistorical,
		Address: &address,
		Detail:  "address does not belong to the selected run's pinned workflow or nested-agent history",
	}
}

func addressableStageRoles(def workflow.Definition) map[string]addressableStageRole {
	roles := make(map[string]addressableStageRole, len(def.Spec.Tasks)+len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskAgentic || task.Goober == "" {
			continue
		}
		roles[task.Name] = addressableStageRole{goober: task.Goober}
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil || gate.Agentic.Goober == "" {
			continue
		}
		roles[gate.Name] = addressableStageRole{goober: gate.Agentic.Goober}
	}
	return roles
}

func pinnedWorkflowDefinition(run runRead) (*workflow.Definition, error) {
	var ref *journal.InputRef
	for i := range run.identity.Inputs {
		if run.identity.Inputs[i].Name == journal.PinnedWorkflowDefinitionInputName {
			ref = &run.identity.Inputs[i]
			break
		}
	}
	if ref == nil {
		return nil, fmt.Errorf("%w: pinned workflow definition is unavailable", ErrArtifactIntegrity)
	}
	if ref.Integrity != apiv1.IntegrityTrusted {
		return nil, fmt.Errorf("%w: pinned workflow definition has integrity %q", ErrArtifactIntegrity, ref.Integrity)
	}
	data, err := run.reader.ArtifactBytes(ref.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: read pinned workflow definition: %w", ErrArtifactIntegrity, err)
	}
	var def workflow.Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("%w: parse pinned workflow definition: %w", ErrArtifactIntegrity, err)
	}
	if def.Name != run.identity.Workflow || def.Version != run.identity.WorkflowVersion {
		return nil, fmt.Errorf("%w: pinned workflow definition identity does not match run", ErrArtifactIntegrity)
	}
	digest, err := workflow.ComputeDigest(def)
	if err != nil {
		return nil, fmt.Errorf("%w: digest pinned workflow definition: %w", ErrArtifactIntegrity, err)
	}
	if run.identity.WorkflowDigest != "" && digest != run.identity.WorkflowDigest {
		return nil, fmt.Errorf("%w: pinned workflow definition digest does not match run", ErrArtifactIntegrity)
	}
	return &def, nil
}

func encodeAddressAgent(rawAgent string, startedSeq uint64) string {
	return addressAgentTokenPrefix +
		strconv.FormatUint(startedSeq, 10) + "~" +
		base64.RawURLEncoding.EncodeToString([]byte(rawAgent))
}

func parseAddressAgent(token string) (encodedAddressAgent, error) {
	rest, ok := strings.CutPrefix(token, addressAgentTokenPrefix)
	if !ok {
		return encodedAddressAgent{}, fmt.Errorf("agent address token %q is malformed", token)
	}
	startedSeqRaw, rawAgentEncoded, ok := strings.Cut(rest, "~")
	if !ok {
		return encodedAddressAgent{}, fmt.Errorf("agent address token %q is malformed", token)
	}
	startedSeq, err := strconv.ParseUint(startedSeqRaw, 10, 64)
	if err != nil || startedSeq == 0 {
		return encodedAddressAgent{}, fmt.Errorf("agent address token %q has invalid visit identity", token)
	}
	rawAgentBytes, err := base64.RawURLEncoding.DecodeString(rawAgentEncoded)
	if err != nil {
		return encodedAddressAgent{}, fmt.Errorf("agent address token %q has invalid agent identity", token)
	}
	rawAgent := string(rawAgentBytes)
	if rawAgent == "" {
		return encodedAddressAgent{}, fmt.Errorf("agent address token %q has empty agent identity", token)
	}
	return encodedAddressAgent{rawAgent: rawAgent, startedSeq: startedSeq}, nil
}

func agentAddressFor(runID, stage string, attempt int, rawAgent string, startedSeq uint64) AgentAddress {
	return AgentAddress{
		Schema:  AgentAddressSchema,
		RunID:   runID,
		Stage:   stage,
		Attempt: attempt,
		Agent:   encodeAddressAgent(rawAgent, startedSeq),
	}
}

func mergeAddressRecord(records []addressRecord, next addressRecord) []addressRecord {
	for i := range records {
		if records[i].identity != next.identity {
			continue
		}
		if next.updatedAt.After(records[i].updatedAt) || next.updatedAt.Equal(records[i].updatedAt) {
			records[i] = next
		}
		return records
	}
	return append(records, next)
}

func filterRunAgentEvents(events []journal.Event, runID string) ([]journal.Event, error) {
	filtered := make([]journal.Event, 0, len(events))
	for _, event := range events {
		if event.Type != journal.EventAgentLifecycle || event.Agent == nil || event.Agent.RunID != runID {
			continue
		}
		if err := journal.ValidateAgentEvent(event); err != nil {
			return nil, fmt.Errorf("project nested-agent history: %w", err)
		}
		filtered = append(filtered, event)
	}
	return filtered, nil
}

func stageAttemptForAgentEvent(attempts []StageAttempt, event journal.Event) (StageAttempt, bool) {
	if event.Agent == nil {
		return StageAttempt{}, false
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		if attempt.Number != event.Agent.Attempt || attempt.StartedSeq == 0 || attempt.StartedSeq > event.Seq {
			continue
		}
		if attempt.FinishedSeq != 0 && event.Seq > attempt.FinishedSeq {
			continue
		}
		return attempt, true
	}
	return StageAttempt{}, false
}

func agentLifecycleLive(lifecycle string) bool {
	switch journal.AgentLifecycle(lifecycle) {
	case journal.AgentStarted, journal.AgentWaiting, journal.AgentResumed:
		return true
	default:
		return false
	}
}
