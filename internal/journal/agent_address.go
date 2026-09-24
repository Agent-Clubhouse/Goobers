package journal

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// AgentAddressSchema is the stable, portable run-scoped address for an
// addressable agent reconstructed from journal provenance.
const AgentAddressSchema = "goobers.dev/journal/agent-address/v1"

const agentAddressPrefix = AgentAddressSchema + "/"
const journalAgentTokenPrefix = "v1~"

// AgentAddress names one agent within a single run/stage/attempt.
type AgentAddress struct {
	RunID   string
	Stage   string
	Attempt int
	AgentID string
}

// Validate rejects malformed or incomplete addresses before resolution.
func (a AgentAddress) Validate() error {
	if !apiv1.ValidRunID(a.RunID) {
		return fmt.Errorf("journal: invalid agent-address run id %q", a.RunID)
	}
	if strings.TrimSpace(a.Stage) == "" {
		return fmt.Errorf("journal: agent-address stage is required")
	}
	if a.Attempt < 1 {
		return fmt.Errorf("journal: invalid agent-address attempt %d", a.Attempt)
	}
	if a.AgentID == "" {
		return fmt.Errorf("journal: agent-address agent id is required")
	}
	return nil
}

// String returns the canonical portable address.
func (a AgentAddress) String() string {
	return agentAddressPrefix +
		url.PathEscape(a.RunID) + "/" +
		url.PathEscape(a.Stage) + "/" +
		strconv.Itoa(a.Attempt) + "/" +
		url.PathEscape(a.AgentID)
}

// ParseAgentAddress decodes the portable run/stage/attempt/agent identifier.
func ParseAgentAddress(raw string) (AgentAddress, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AgentAddress{}, fmt.Errorf("journal: agent-address is required")
	}
	rest, ok := strings.CutPrefix(raw, agentAddressPrefix)
	if !ok {
		return AgentAddress{}, fmt.Errorf("journal: unsupported agent-address schema %q", raw)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 {
		return AgentAddress{}, fmt.Errorf("journal: invalid agent-address %q", raw)
	}
	runID, err := url.PathUnescape(parts[0])
	if err != nil {
		return AgentAddress{}, fmt.Errorf("journal: decode agent-address run id: %w", err)
	}
	stage, err := url.PathUnescape(parts[1])
	if err != nil {
		return AgentAddress{}, fmt.Errorf("journal: decode agent-address stage: %w", err)
	}
	attempt, err := strconv.Atoi(parts[2])
	if err != nil {
		return AgentAddress{}, fmt.Errorf("journal: decode agent-address attempt: %w", err)
	}
	agentID, err := url.PathUnescape(parts[3])
	if err != nil {
		return AgentAddress{}, fmt.Errorf("journal: decode agent-address agent id: %w", err)
	}
	address := AgentAddress{RunID: runID, Stage: stage, Attempt: attempt, AgentID: agentID}
	if err := address.Validate(); err != nil {
		return AgentAddress{}, err
	}
	return address, nil
}

// AgentAddressStatus is the typed resolution outcome for a run-scoped address.
type AgentAddressStatus string

const (
	// AgentAddressLive means the address resolves to one live agent.
	AgentAddressLive AgentAddressStatus = "live"
	// AgentAddressMalformed means the portable address itself is invalid.
	AgentAddressMalformed AgentAddressStatus = "malformed"
	// AgentAddressHistorical means the address belongs to a different or finished run.
	AgentAddressHistorical AgentAddressStatus = "historical"
	// AgentAddressStale means the run advanced beyond the addressed visit.
	AgentAddressStale AgentAddressStatus = "stale"
	// AgentAddressTerminated means the addressed agent ended within the current visit.
	AgentAddressTerminated AgentAddressStatus = "terminated"
	// AgentAddressUnreachable means the visit exists but the address cannot be delivered.
	AgentAddressUnreachable AgentAddressStatus = "unreachable"
)

// AgentResolution is the typed outcome of resolving one agent address against a
// selected run journal. Only the live and terminated outcomes carry provenance.
type AgentResolution struct {
	Status  AgentAddressStatus
	Address AgentAddress
	Agent   *AgentProvenance
}

// AddressableAgent is one currently reachable agent with its stable address.
type AddressableAgent struct {
	Address AgentAddress
	Agent   AgentProvenance
}

type attemptSpan struct {
	stage       string
	attempt     int
	startedSeq  uint64
	finishedSeq uint64
}

type agentAddressRecord struct {
	identity   string
	address    AgentAddress
	agent      AgentProvenance
	startedSeq uint64
	finished   bool
}

type agentAddressSnapshot struct {
	known              map[string][]agentAddressRecord
	live               map[string]AddressableAgent
	collisions         map[string]int
	latestStartByStage map[string]uint64
}

// AddressableAgents reconstructs the currently live, run-local agents that can
// be targeted safely from journal provenance alone.
func AddressableAgents(events []Event, runID string) ([]AddressableAgent, error) {
	if !apiv1.ValidRunID(runID) {
		return nil, fmt.Errorf("journal: run id is required for agent enumeration")
	}
	if PhaseFromEvents(events) != PhaseRunning {
		return nil, nil
	}
	snapshot, err := buildAgentAddressSnapshot(events, runID)
	if err != nil {
		return nil, err
	}
	list := make([]AddressableAgent, 0, len(snapshot.live))
	for _, agent := range snapshot.live {
		list = append(list, agent)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Address.Stage != list[j].Address.Stage {
			return list[i].Address.Stage < list[j].Address.Stage
		}
		if list[i].Address.Attempt != list[j].Address.Attempt {
			return list[i].Address.Attempt < list[j].Address.Attempt
		}
		return list[i].Address.AgentID < list[j].Address.AgentID
	})
	return list, nil
}

// ResolveAgentAddress resolves one portable agent address against a selected
// run's journal and reports why non-live targets are not deliverable.
func ResolveAgentAddress(events []Event, runID, raw string) (AgentResolution, error) {
	address, err := ParseAgentAddress(raw)
	if err != nil {
		return AgentResolution{Status: AgentAddressMalformed}, nil
	}
	if _, err := parseJournalAgentToken(address.AgentID); err != nil {
		return AgentResolution{Status: AgentAddressMalformed, Address: address}, nil
	}
	result := AgentResolution{Status: AgentAddressUnreachable, Address: address}
	if address.RunID != runID {
		result.Status = AgentAddressHistorical
		return result, nil
	}
	snapshot, err := buildAgentAddressSnapshot(events, runID)
	if err != nil {
		return AgentResolution{}, err
	}
	phase := PhaseFromEvents(events)
	token, _ := parseJournalAgentToken(address.AgentID)
	key := address.String()
	if phase == PhaseRunning {
		if snapshot.collisions[key] > 1 {
			return result, nil
		}
		if live, ok := snapshot.live[key]; ok {
			result.Status = AgentAddressLive
			result.Agent = &live.Agent
			return result, nil
		}
	}
	records := snapshot.known[key]
	if len(records) == 0 {
		result.Status = AgentAddressHistorical
		return result, nil
	}
	if snapshot.latestStartByStage[address.Stage] > token.startedSeq {
		result.Status = AgentAddressStale
		return result, nil
	}
	record, matches, ok := selectAgentAddressRecord(records, token.startedSeq)
	if !ok {
		result.Status = AgentAddressHistorical
		return result, nil
	}
	if matches > 1 {
		return result, nil
	}
	result.Agent = &record.agent
	if phase != PhaseRunning || record.finished {
		result.Status = AgentAddressTerminated
		return result, nil
	}
	if agentTerminal(record.agent.Lifecycle) {
		result.Status = AgentAddressTerminated
		return result, nil
	}
	return result, nil
}

// AddressableAgents enumerates the selected run's current live agents.
func (r *Reader) AddressableAgents() ([]AddressableAgent, error) {
	identity, err := r.Identity()
	if err != nil {
		return nil, err
	}
	events, err := r.Events()
	if err != nil {
		return nil, err
	}
	return AddressableAgents(events, identity.RunID)
}

// ResolveAgentAddress resolves a portable agent address against the selected
// run journal.
func (r *Reader) ResolveAgentAddress(raw string) (AgentResolution, error) {
	identity, err := r.Identity()
	if err != nil {
		return AgentResolution{}, err
	}
	events, err := r.Events()
	if err != nil {
		return AgentResolution{}, err
	}
	return ResolveAgentAddress(events, identity.RunID, raw)
}

func buildAgentAddressSnapshot(events []Event, runID string) (agentAddressSnapshot, error) {
	filtered := latestPodAgentEvents(events)
	spansByStage, latestStartByStage := collectAttemptSpans(filtered)
	known := make(map[string][]agentAddressRecord)
	for _, event := range filtered {
		if event.Type != EventAgentLifecycle || event.Agent == nil || event.Agent.RunID != runID {
			continue
		}
		if err := validateAgent(*event.Agent); err != nil {
			return agentAddressSnapshot{}, err
		}
		span, ok := attemptSpanForEvent(spansByStage[event.Agent.Stage], event)
		if !ok || span.startedSeq == 0 {
			continue
		}
		address := AgentAddress{
			RunID:   runID,
			Stage:   event.Agent.Stage,
			Attempt: event.Agent.Attempt,
			AgentID: encodeJournalAgentToken(event.Agent.ID, span.startedSeq),
		}
		key := address.String()
		record := agentAddressRecord{
			identity:   agentAddressIdentity(*event.Agent, span.startedSeq),
			address:    address,
			agent:      *event.Agent,
			startedSeq: span.startedSeq,
			finished:   span.finishedSeq != 0,
		}
		known[key] = mergeAgentAddressRecord(known[key], record)
	}
	live := make(map[string]AddressableAgent, len(known))
	collisions := make(map[string]int, len(known))
	for key, records := range known {
		candidates := make([]agentAddressRecord, 0, len(records))
		for _, record := range records {
			if latestStartByStage[record.address.Stage] != record.startedSeq {
				continue
			}
			collisions[key]++
			if record.finished || agentTerminal(record.agent.Lifecycle) {
				continue
			}
			candidates = append(candidates, record)
		}
		if collisions[key] == 1 && len(candidates) == 1 {
			live[key] = AddressableAgent{Address: candidates[0].address, Agent: candidates[0].agent}
		}
	}
	return agentAddressSnapshot{
		known:              known,
		live:               live,
		collisions:         collisions,
		latestStartByStage: latestStartByStage,
	}, nil
}

func collectAttemptSpans(events []Event) (map[string][]attemptSpan, map[string]uint64) {
	spans := make(map[string][]attemptSpan)
	latest := make(map[string]uint64)
	for _, event := range events {
		if event.Stage == "" || event.Attempt < 1 {
			continue
		}
		switch event.Type {
		case EventStageStarted:
			span := attemptSpan{stage: event.Stage, attempt: event.Attempt, startedSeq: event.Seq}
			spans[event.Stage] = append(spans[event.Stage], span)
			if event.Seq > latest[event.Stage] {
				latest[event.Stage] = event.Seq
			}
		case EventStageFinished:
			stageSpans := spans[event.Stage]
			for i := len(stageSpans) - 1; i >= 0; i-- {
				if stageSpans[i].attempt != event.Attempt || stageSpans[i].finishedSeq != 0 {
					continue
				}
				stageSpans[i].finishedSeq = event.Seq
				spans[event.Stage] = stageSpans
				break
			}
		}
	}
	return spans, latest
}

func attemptSpanForEvent(spans []attemptSpan, event Event) (attemptSpan, bool) {
	if event.Agent == nil {
		return attemptSpan{}, false
	}
	for i := len(spans) - 1; i >= 0; i-- {
		span := spans[i]
		if span.attempt != event.Agent.Attempt || span.startedSeq > event.Seq {
			continue
		}
		if span.finishedSeq != 0 && event.Seq > span.finishedSeq {
			continue
		}
		return span, true
	}
	return attemptSpan{}, false
}

func encodeJournalAgentToken(rawAgent string, startedSeq uint64) string {
	return journalAgentTokenPrefix +
		strconv.FormatUint(startedSeq, 10) + "~" +
		base64.RawURLEncoding.EncodeToString([]byte(rawAgent))
}

type journalAgentToken struct {
	startedSeq uint64
	rawAgent   string
}

func parseJournalAgentToken(token string) (journalAgentToken, error) {
	rest, ok := strings.CutPrefix(token, journalAgentTokenPrefix)
	if !ok {
		return journalAgentToken{}, fmt.Errorf("journal: malformed agent-address token %q", token)
	}
	startedSeqRaw, rawEncoded, ok := strings.Cut(rest, "~")
	if !ok {
		return journalAgentToken{}, fmt.Errorf("journal: malformed agent-address token %q", token)
	}
	startedSeq, err := strconv.ParseUint(startedSeqRaw, 10, 64)
	if err != nil || startedSeq == 0 {
		return journalAgentToken{}, fmt.Errorf("journal: invalid agent-address visit identity %q", token)
	}
	rawBytes, err := base64.RawURLEncoding.DecodeString(rawEncoded)
	if err != nil {
		return journalAgentToken{}, fmt.Errorf("journal: invalid agent-address agent identity %q", token)
	}
	if len(rawBytes) == 0 {
		return journalAgentToken{}, fmt.Errorf("journal: invalid empty agent-address agent identity")
	}
	return journalAgentToken{startedSeq: startedSeq, rawAgent: string(rawBytes)}, nil
}

func mergeAgentAddressRecord(records []agentAddressRecord, next agentAddressRecord) []agentAddressRecord {
	for i := range records {
		if records[i].identity != next.identity {
			continue
		}
		if newerAgentEvent(&next.agent, &records[i].agent) {
			records[i] = next
		}
		return records
	}
	return append(records, next)
}

func selectAgentAddressRecord(records []agentAddressRecord, startedSeq uint64) (agentAddressRecord, int, bool) {
	var (
		selected agentAddressRecord
		matches  int
	)
	for _, record := range records {
		if record.startedSeq != startedSeq {
			continue
		}
		if matches == 0 || newerAgentEvent(&record.agent, &selected.agent) {
			selected = record
		}
		matches++
	}
	return selected, matches, matches > 0
}

func agentAddressIdentity(agent AgentProvenance, startedSeq uint64) string {
	parent := agent.ParentID
	if parent == "" {
		parent = "\x00"
	}
	return strconv.FormatUint(startedSeq, 10) +
		"\x00" + agent.ID +
		"\x00" + parent +
		"\x00" + strconv.FormatBool(agent.Worker)
}

func agentTerminal(lifecycle AgentLifecycle) bool {
	switch lifecycle {
	case AgentCompleted, AgentFailed, AgentCancelled:
		return true
	default:
		return false
	}
}
