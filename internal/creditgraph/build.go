package creditgraph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// SpanProvenanceAnnotation is the runner.annotation kind that links a recorded
// span to the subagent that emitted it. The link is additive bookkeeping: a
// journal without it yields a graph whose model and tool layer hangs off an
// explicitly unknown subagent rather than a guessed one.
const SpanProvenanceAnnotation = "credit-span-provenance"

// Annotation payload keys for SpanProvenanceAnnotation.
const (
	// SpanProvenanceKeyKind is the runner.* payload key naming the annotation.
	SpanProvenanceKeyKind = "kind"
	// SpanProvenanceKeyAgentID names the emitting subagent.
	SpanProvenanceKeyAgentID = "agentId"
	// SpanProvenanceKeyDigest names the span content digest the link applies to.
	SpanProvenanceKeyDigest = "digest"
)

// Input is everything the read model projects. Events is the run's journal in
// journal order. SpanData supplies recorded span content keyed by content
// digest; spans whose content is absent are reported as gaps rather than
// reconstructed.
type Input struct {
	RunID    string
	Gaggle   string
	Workflow string
	Events   []journal.Event
	SpanData map[string][]byte
}

const outcomeNodeID = "outcome"

type builder struct {
	graph      *Graph
	stageNodes map[string]string
	agentNodes map[string]string
	toolNodes  map[string]string
	spanAgent  map[string]string
	callNodes  map[string]string
	callResult map[string]bool
}

// Build materializes the execution/credit graph for one run.
func Build(input Input) (*Graph, error) {
	b := &builder{
		graph: &Graph{
			Schema:   SchemaVersion,
			RunID:    input.RunID,
			Gaggle:   input.Gaggle,
			Workflow: input.Workflow,
			RootID:   outcomeNodeID,
			index:    map[string]int{},
			children: map[string][]int{},
		},
		stageNodes: map[string]string{},
		agentNodes: map[string]string{},
		toolNodes:  map[string]string{},
		spanAgent:  map[string]string{},
		callNodes:  map[string]string{},
		callResult: map[string]bool{},
	}
	b.collectSpanProvenance(input.Events)
	b.addOutcomeAndRun(input)
	b.addStages(input.Events)
	if err := b.addAgents(input.Events); err != nil {
		return nil, err
	}
	if err := b.addSpans(input); err != nil {
		return nil, err
	}
	b.addEvidence(input.Events)
	b.addEvaluators(input.Events)
	b.reportUnresolvedToolCalls()
	return b.graph, nil
}

func (b *builder) collectSpanProvenance(events []journal.Event) {
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner == nil {
			continue
		}
		if kind, _ := event.Runner[SpanProvenanceKeyKind].(string); kind != SpanProvenanceAnnotation {
			continue
		}
		digest, _ := event.Runner[SpanProvenanceKeyDigest].(string)
		agentID, _ := event.Runner[SpanProvenanceKeyAgentID].(string)
		if digest == "" || agentID == "" {
			continue
		}
		b.spanAgent[digest] = agentID
	}
}

func (b *builder) addOutcomeAndRun(input Input) {
	outcome := Node{ID: outcomeNodeID, Kind: KindOutcome, Provenance: ProvenanceUnknown}
	runNode := Node{
		ID: runNodeID(input.RunID), Kind: KindRun, Label: input.Workflow,
		Provenance: ProvenanceUnknown, Attributes: map[string]string{},
	}
	if input.Gaggle != "" {
		runNode.Attributes["gaggle"] = input.Gaggle
	}
	if input.RunID != "" {
		runNode.Attributes["runId"] = input.RunID
	}
	var finished, started bool
	for _, event := range input.Events {
		switch event.Type {
		case journal.EventRunStarted:
			started = true
		case journal.EventRunFinished:
			finished = true
			outcome.Label = event.Status
			outcome.Provenance = ProvenanceRecorded
			outcome.Attributes = map[string]string{"status": event.Status}
			if event.Verdict != "" {
				outcome.Attributes["verdict"] = event.Verdict
			}
			if event.Target != "" {
				outcome.Attributes["target"] = event.Target
			}
		}
	}
	if started {
		runNode.Provenance = ProvenanceRecorded
	}
	b.addNode(outcome)
	if !finished {
		b.gap(outcome.ID, outcome.Kind, "run recorded no terminal outcome")
	}
	b.addNode(runNode)
	if !started {
		b.gap(runNode.ID, runNode.Kind, "run recorded no run.started identity")
	}
	b.addEdge(outcome.ID, runNode.ID, EdgeAttributedTo, provenanceOf(finished && started))
}

func (b *builder) addStages(events []journal.Event) {
	for _, event := range events {
		if event.Type != journal.EventStageStarted && event.Type != journal.EventStageFinished {
			continue
		}
		if event.Stage == "" {
			continue
		}
		id := b.ensureStage(event.Stage, event.Attempt, event.Type == journal.EventStageStarted)
		if event.Type != journal.EventStageFinished {
			continue
		}
		if position, ok := b.graph.index[id]; ok && event.Status != "" {
			b.graph.Nodes[position].Attributes["status"] = event.Status
		}
	}
}

func (b *builder) ensureStage(stage string, attempt int, recorded bool) string {
	if attempt < 1 {
		attempt = 1
	}
	key := stage + "#" + strconv.Itoa(attempt)
	if id, ok := b.stageNodes[key]; ok {
		if recorded {
			if position, indexed := b.graph.index[id]; indexed {
				b.graph.Nodes[position].Provenance = ProvenanceRecorded
			}
		}
		return id
	}
	id := "stage:" + key
	b.stageNodes[key] = id
	b.addNode(Node{
		ID: id, Kind: KindStage, Label: stage, Stage: stage, Attempt: attempt,
		Provenance: provenanceOf(recorded), Attributes: map[string]string{},
	})
	if !recorded {
		b.gap(id, KindStage, "stage attempt is referenced but recorded no stage.started")
	}
	b.addEdge(runNodeID(b.graph.RunID), id, EdgeContains, provenanceOf(recorded))
	return id
}

func (b *builder) addAgents(events []journal.Event) error {
	latest, err := journal.AgentTree(events)
	if err != nil {
		return fmt.Errorf("creditgraph: project agent tree: %w", err)
	}
	var order []string
	seen := map[string]bool{}
	for _, event := range events {
		if event.Type != journal.EventAgentLifecycle || event.Agent == nil || event.Agent.ID == "" {
			continue
		}
		if seen[event.Agent.ID] {
			continue
		}
		seen[event.Agent.ID] = true
		order = append(order, event.Agent.ID)
	}
	for _, id := range order {
		agent, ok := latest[id]
		if !ok {
			continue
		}
		b.addAgent(agent, true)
	}
	for _, id := range order {
		agent, ok := latest[id]
		if !ok {
			continue
		}
		b.linkAgent(agent, latest)
	}
	return nil
}

func (b *builder) addAgent(agent journal.AgentProvenance, recorded bool) string {
	if id, ok := b.agentNodes[agent.ID]; ok {
		if recorded {
			if position, indexed := b.graph.index[id]; indexed {
				b.graph.Nodes[position].Provenance = ProvenanceRecorded
			}
		}
		return id
	}
	id := "subagent:" + agent.ID
	b.agentNodes[agent.ID] = id
	attributes := map[string]string{}
	if agent.Plugin != "" {
		attributes["plugin"] = agent.Plugin
	}
	if agent.Lifecycle != "" {
		attributes["lifecycle"] = string(agent.Lifecycle)
	}
	if agent.Fidelity != "" {
		attributes["fidelity"] = agent.Fidelity
	}
	if agent.Coordinator {
		attributes["coordinator"] = "true"
	}
	b.addNode(Node{
		ID: id, Kind: KindSubagent, Label: agent.ID, Stage: agent.Stage,
		Attempt: agent.Attempt, Provenance: provenanceOf(recorded), Attributes: attributes,
	})
	if !recorded {
		b.gap(id, KindSubagent, "subagent is referenced by another agent but recorded no lifecycle of its own")
	}
	return id
}

func (b *builder) linkAgent(agent journal.AgentProvenance, latest map[string]journal.AgentProvenance) {
	id := b.agentNodes[agent.ID]
	if agent.ParentID == "" {
		stageID := b.ensureStage(agent.Stage, agent.Attempt, false)
		b.addEdge(stageID, id, EdgeContains, ProvenanceRecorded)
	} else {
		parent, known := latest[agent.ParentID]
		if !known {
			parent = journal.AgentProvenance{ID: agent.ParentID, Stage: agent.Stage, Attempt: agent.Attempt}
			parentID := b.addAgent(parent, false)
			stageID := b.ensureStage(agent.Stage, agent.Attempt, false)
			b.addEdge(stageID, parentID, EdgeContains, ProvenanceUnknown)
		}
		b.addEdge(b.agentNodes[parent.ID], id, EdgeDelegates, provenanceOf(known))
	}
	for _, dependency := range agent.DependsOn {
		target, known := b.agentNodes[dependency]
		if !known {
			b.gap(id, KindSubagent, "declared dependency on subagent "+dependency+" that recorded no lifecycle")
			continue
		}
		b.addEdge(id, target, EdgeDependsOn, ProvenanceRecorded)
	}
	if !b.hasSpan(agent.ID) {
		b.addAgentModelNode(agent, id)
	}
}

// addAgentModelNode records the aggregate model invocation a lifecycle event
// implies when no transcript span breaks it down per call.
func (b *builder) addAgentModelNode(agent journal.AgentProvenance, agentNodeID string) {
	id := "model:" + agent.ID
	attributes := map[string]string{}
	if agent.RequestedModel != "" {
		attributes["requestedModel"] = agent.RequestedModel
	}
	if agent.Usage.InputTokens != nil {
		attributes["inputTokens"] = strconv.FormatInt(*agent.Usage.InputTokens, 10)
	}
	if agent.Usage.OutputTokens != nil {
		attributes["outputTokens"] = strconv.FormatInt(*agent.Usage.OutputTokens, 10)
	}
	recorded := agent.ResolvedModel != ""
	if recorded {
		attributes["model"] = agent.ResolvedModel
	}
	b.addNode(Node{
		ID: id, Kind: KindModelInvocation, Label: agent.ResolvedModel, Stage: agent.Stage,
		Attempt: agent.Attempt, Provenance: provenanceOf(recorded), Attributes: attributes,
	})
	if !recorded {
		b.gap(id, KindModelInvocation, "subagent recorded no resolved model")
	}
	b.addEdge(agentNodeID, id, EdgeContains, provenanceOf(recorded))
}

func (b *builder) hasSpan(agentID string) bool {
	for _, linked := range b.spanAgent {
		if linked == agentID {
			return true
		}
	}
	return false
}

func (b *builder) addSpans(input Input) error {
	for _, event := range input.Events {
		if event.Type != journal.EventSpanRecorded || event.DataSchema != telemetry.GenAIEventSchema {
			continue
		}
		digest := ""
		if event.Ref != nil {
			digest = event.Ref.Digest
		}
		stageID := b.ensureStage(event.Stage, event.Attempt, false)
		ownerID, ownerKnown := b.spanOwner(digest, event, stageID)
		data, ok := input.SpanData[digest]
		if digest == "" || !ok {
			b.gap(ownerID, KindSubagent, "span "+event.Name+" recorded no retrievable content, so its model and tool calls are unknown")
			continue
		}
		if err := b.addTranscript(data, ownerID, ownerKnown, event); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) spanOwner(digest string, event journal.Event, stageID string) (string, bool) {
	if agentID, ok := b.spanAgent[digest]; ok {
		if nodeID, known := b.agentNodes[agentID]; known {
			return nodeID, true
		}
		nodeID := b.addAgent(journal.AgentProvenance{
			ID: agentID, Stage: event.Stage, Attempt: event.Attempt,
		}, false)
		b.addEdge(stageID, nodeID, EdgeContains, ProvenanceUnknown)
		return nodeID, false
	}
	unknown := journal.AgentProvenance{
		ID: "unknown/" + stageID, Stage: event.Stage, Attempt: event.Attempt,
	}
	nodeID, existed := b.agentNodes[unknown.ID]
	if !existed {
		nodeID = b.addAgent(unknown, false)
		b.addEdge(stageID, nodeID, EdgeContains, ProvenanceUnknown)
	}
	b.gap(nodeID, KindSubagent, "span "+event.Name+" names no emitting subagent")
	return nodeID, false
}

type genaiToolRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Success *bool  `json:"success,omitempty"`
}

type genaiUsageRecord struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
}

type genaiRecord struct {
	Role     string            `json:"role"`
	Model    string            `json:"model,omitempty"`
	Usage    *genaiUsageRecord `json:"usage,omitempty"`
	ToolCall *genaiToolRecord  `json:"tool_call,omitempty"`
}

func (b *builder) addTranscript(data []byte, ownerID string, ownerKnown bool, event journal.Event) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	ownerProvenance := provenanceOf(ownerKnown)
	lastModelID := ""
	index := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record genaiRecord
		if err := json.Unmarshal(line, &record); err != nil {
			b.gap(ownerID, KindSubagent, "span "+event.Name+" carries an undecodable record, so part of its execution is unknown")
			continue
		}
		index++
		switch {
		case record.ToolCall != nil && record.Role == "tool":
			b.addToolResult(record, ownerID, ownerProvenance, event, index)
		case record.ToolCall != nil:
			b.addToolCall(record, ownerID, lastModelID, ownerProvenance, event, index)
		case record.Role == "assistant":
			lastModelID = b.addModelInvocation(record, ownerID, ownerProvenance, event, index)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("creditgraph: read span %q: %w", event.Name, err)
	}
	return nil
}

func (b *builder) addModelInvocation(record genaiRecord, ownerID string, ownerProvenance Provenance, event journal.Event, index int) string {
	id := "model:" + ownerID + "#" + strconv.Itoa(index)
	attributes := map[string]string{}
	if record.Model != "" {
		attributes["model"] = record.Model
	}
	if record.Usage != nil {
		if record.Usage.InputTokens != nil {
			attributes["inputTokens"] = strconv.FormatInt(*record.Usage.InputTokens, 10)
		}
		if record.Usage.OutputTokens != nil {
			attributes["outputTokens"] = strconv.FormatInt(*record.Usage.OutputTokens, 10)
		}
	}
	recorded := record.Model != ""
	b.addNode(Node{
		ID: id, Kind: KindModelInvocation, Label: record.Model, Stage: event.Stage,
		Attempt: event.Attempt, Provenance: provenanceOf(recorded), Attributes: attributes,
	})
	if !recorded {
		b.gap(id, KindModelInvocation, "model invocation names no model")
	}
	b.addEdge(ownerID, id, EdgeContains, ownerProvenance)
	return id
}

func (b *builder) addToolCall(record genaiRecord, ownerID, lastModelID string, ownerProvenance Provenance, event journal.Event, index int) {
	callID := record.ToolCall.ID
	id := "toolcall:" + ownerID + "#" + strconv.Itoa(index)
	attributes := map[string]string{}
	if callID != "" {
		attributes["callId"] = callID
	}
	if record.ToolCall.Name != "" {
		attributes["tool"] = record.ToolCall.Name
	}
	b.addNode(Node{
		ID: id, Kind: KindToolCall, Label: record.ToolCall.Name, Stage: event.Stage,
		Attempt: event.Attempt, Provenance: provenanceOf(record.ToolCall.Name != ""),
		Attributes: attributes,
	})
	if record.ToolCall.Name == "" {
		b.gap(id, KindToolCall, "tool call names no tool")
	}
	if lastModelID != "" {
		b.addEdge(lastModelID, id, EdgeInvokes, ownerProvenance)
	} else {
		b.addEdge(ownerID, id, EdgeContains, ownerProvenance)
		b.gap(id, KindToolCall, "tool call follows no recorded model invocation")
	}
	if record.ToolCall.Name != "" {
		b.addEdge(id, b.ensureTool(record.ToolCall.Name), EdgeUses, ProvenanceRecorded)
	}
	if callID == "" {
		b.gap(id, KindToolCall, "tool call carries no call id, so its result cannot be joined")
		return
	}
	b.callNodes[callID] = id
}

func (b *builder) addToolResult(record genaiRecord, ownerID string, ownerProvenance Provenance, event journal.Event, index int) {
	callID := record.ToolCall.ID
	id := "toolresult:" + ownerID + "#" + strconv.Itoa(index)
	attributes := map[string]string{}
	if callID != "" {
		attributes["callId"] = callID
	}
	if record.ToolCall.Success != nil {
		attributes["success"] = strconv.FormatBool(*record.ToolCall.Success)
	}
	callNodeID, known := b.callNodes[callID]
	b.addNode(Node{
		ID: id, Kind: KindToolResult, Stage: event.Stage, Attempt: event.Attempt,
		Provenance: provenanceOf(known), Attributes: attributes,
	})
	if !known {
		b.addEdge(ownerID, id, EdgeContains, ownerProvenance)
		b.gap(id, KindToolResult, "tool result joins no recorded tool call")
		return
	}
	b.callResult[callID] = true
	b.addEdge(callNodeID, id, EdgeProduces, ProvenanceRecorded)
}

func (b *builder) ensureTool(name string) string {
	if id, ok := b.toolNodes[name]; ok {
		return id
	}
	id := "tool:" + name
	b.toolNodes[name] = id
	b.addNode(Node{ID: id, Kind: KindTool, Label: name, Provenance: ProvenanceRecorded})
	return id
}

func (b *builder) reportUnresolvedToolCalls() {
	for _, node := range b.graph.Nodes {
		if node.Kind != KindToolCall {
			continue
		}
		callID := node.Attributes["callId"]
		if callID == "" || b.callResult[callID] {
			continue
		}
		b.gap(node.ID, KindToolCall, "tool call recorded no result")
	}
}

func (b *builder) addEvidence(events []journal.Event) {
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded {
			continue
		}
		digest := ""
		if event.Ref != nil {
			digest = event.Ref.Digest
		}
		id := "evidence:" + digest
		if digest == "" {
			id = "evidence:" + event.Stage + "/" + event.Name
		}
		if _, exists := b.graph.index[id]; exists {
			continue
		}
		attributes := map[string]string{}
		if digest != "" {
			attributes["digest"] = digest
		}
		if event.Integrity != "" {
			attributes["integrity"] = string(event.Integrity)
		}
		b.addNode(Node{
			ID: id, Kind: KindEvidence, Label: event.Name, Stage: event.Stage,
			Attempt: event.Attempt, Provenance: provenanceOf(digest != ""),
			Attributes: attributes,
		})
		if digest == "" {
			b.gap(id, KindEvidence, "artifact recorded no content digest")
		}
		parent := runNodeID(b.graph.RunID)
		if event.Stage != "" {
			parent = b.ensureStage(event.Stage, event.Attempt, false)
		}
		b.addEdge(parent, id, EdgeProduces, ProvenanceRecorded)
	}
}

func (b *builder) addEvaluators(events []journal.Event) {
	for _, event := range events {
		if event.Type != journal.EventGateEvaluated && event.Type != journal.EventGateOverridden {
			continue
		}
		id := "evaluator:" + event.Gate + "#" + strconv.FormatUint(event.Seq, 10)
		attributes := map[string]string{}
		if event.Verdict != "" {
			attributes["verdict"] = event.Verdict
		}
		if event.Target != "" {
			attributes["target"] = event.Target
		}
		if event.Type == journal.EventGateOverridden {
			attributes["overridden"] = "true"
		}
		recorded := event.Gate != "" && event.Verdict != ""
		b.addNode(Node{
			ID: id, Kind: KindEvaluator, Label: event.Gate,
			Provenance: provenanceOf(recorded), Attributes: attributes,
		})
		if !recorded {
			b.gap(id, KindEvaluator, "gate evaluation recorded no gate name or verdict")
		}
		b.addEdge(runNodeID(b.graph.RunID), id, EdgeContains, ProvenanceRecorded)
		if event.Stage != "" {
			b.addEdge(id, b.ensureStage(event.Stage, event.Attempt, false), EdgeEvaluates, ProvenanceRecorded)
		}
	}
}

func (b *builder) addNode(node Node) {
	if _, exists := b.graph.index[node.ID]; exists {
		return
	}
	b.graph.index[node.ID] = len(b.graph.Nodes)
	b.graph.Nodes = append(b.graph.Nodes, node)
}

func (b *builder) addEdge(from, to string, kind EdgeKind, provenance Provenance) {
	for _, position := range b.graph.children[from] {
		existing := b.graph.Edges[position]
		if existing.To == to && existing.Kind == kind {
			return
		}
	}
	b.graph.children[from] = append(b.graph.children[from], len(b.graph.Edges))
	b.graph.Edges = append(b.graph.Edges, Edge{From: from, To: to, Kind: kind, Provenance: provenance})
}

func (b *builder) gap(nodeID string, kind NodeKind, reason string) {
	for _, existing := range b.graph.Gaps {
		if existing.NodeID == nodeID && existing.Reason == reason {
			return
		}
	}
	b.graph.Gaps = append(b.graph.Gaps, Gap{NodeID: nodeID, Kind: kind, Reason: reason})
}

func runNodeID(runID string) string { return "run:" + runID }

func provenanceOf(recorded bool) Provenance {
	if recorded {
		return ProvenanceRecorded
	}
	return ProvenanceUnknown
}
