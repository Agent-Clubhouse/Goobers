package creditgraph

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func agentEvent(id, parent, stage, model string, lifecycle journal.AgentLifecycle, dependsOn ...string) journal.Event {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	return journal.Event{Type: journal.EventAgentLifecycle, Stage: stage, Agent: &journal.AgentProvenance{
		Schema: "goobers.dev/journal/agent/v1", ID: id, ParentID: parent, RunID: "run-1",
		Stage: stage, Attempt: 1, Lifecycle: lifecycle, ResolvedModel: model,
		StartedAt: now, UpdatedAt: now, DependsOn: dependsOn,
	}}
}

func spanEvent(stage, name, digest string) journal.Event {
	return journal.Event{
		Type: journal.EventSpanRecorded, Stage: stage, Attempt: 1, Name: name,
		DataSchema: telemetry.GenAIEventSchema,
		Ref:        &journal.Ref{Path: "spans/" + name, Digest: digest},
	}
}

func spanProvenance(stage, digest, agentID string) journal.Event {
	return journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Runner: map[string]any{
		SpanProvenanceKeyKind:    SpanProvenanceAnnotation,
		SpanProvenanceKeyAgentID: agentID,
		SpanProvenanceKeyDigest:  digest,
	}}
}

func nodeByID(t *testing.T, graph *Graph, id string) Node {
	t.Helper()
	node, ok := graph.Node(id)
	if !ok {
		t.Fatalf("node %q missing from graph: %+v", id, graph.Nodes)
	}
	return node
}

func hasEdge(graph *Graph, from, to string, kind EdgeKind) bool {
	for _, edge := range graph.OutEdges(from) {
		if edge.To == to && edge.Kind == kind {
			return true
		}
	}
	return false
}

func gapReasons(graph *Graph, nodeID string) []string {
	var reasons []string
	for _, gap := range graph.Gaps {
		if gap.NodeID == nodeID {
			reasons = append(reasons, gap.Reason)
		}
	}
	return reasons
}

func transcript(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// TestBuildTraversesOutcomeDownToNestedExecution pins the graph's headline
// contract: the final outcome is the root and every nested execution element
// — stage, subagent, model invocation, tool call, tool result, evidence,
// evaluator — is reachable from it.
func TestBuildTraversesOutcomeDownToNestedExecution(t *testing.T) {
	digest := "sha256:span"
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		agentEvent("root", "", "implement", "gpt-5", journal.AgentStarted),
		spanEvent("implement", "copilot-cli.transcript", digest),
		spanProvenance("implement", digest, "root"),
		{Seq: 3, Type: journal.EventArtifactRecorded, Stage: "implement", Attempt: 1,
			Name: "diff", Ref: &journal.Ref{Digest: "sha256:diff"}, Integrity: apiv1.IntegrityUnapproved},
		{Seq: 4, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "success"},
		{Seq: 5, Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass", Target: "done", Stage: "implement", Attempt: 1},
		{Seq: 6, Type: journal.EventRunFinished, Status: "completed"},
	}
	graph, err := Build(Input{
		RunID: "run-1", Gaggle: "goobers-repo", Workflow: "implementation", Events: events,
		SpanData: map[string][]byte{digest: transcript(
			`{"role":"assistant","model":"gpt-5","usage":{"input_tokens":10,"output_tokens":3}}`,
			`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
			`{"role":"tool","tool_call":{"id":"call-1","success":true}}`,
		)},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	root, ok := graph.Root()
	if !ok || root.Kind != KindOutcome || root.Attributes["status"] != "completed" {
		t.Fatalf("root = %+v, want the recorded final outcome", root)
	}
	if graph.Schema != SchemaVersion {
		t.Fatalf("schema = %q, want %q", graph.Schema, SchemaVersion)
	}
	seen := map[NodeKind]int{}
	graph.Walk(graph.RootID, func(node Node, _ int) bool {
		seen[node.Kind]++
		return true
	})
	for _, kind := range []NodeKind{
		KindOutcome, KindRun, KindStage, KindSubagent, KindModelInvocation,
		KindToolCall, KindToolResult, KindTool, KindEvidence, KindEvaluator,
	} {
		if seen[kind] == 0 {
			t.Fatalf("kind %q is not reachable from the outcome: %+v", kind, graph.Nodes)
		}
	}
	if !hasEdge(graph, graph.RootID, "run:run-1", EdgeAttributedTo) {
		t.Fatalf("outcome must be attributed to the run: %+v", graph.Edges)
	}
	if !hasEdge(graph, "run:run-1", "stage:implement#1", EdgeContains) ||
		!hasEdge(graph, "stage:implement#1", "subagent:root", EdgeContains) {
		t.Fatalf("run must contain its stage and subagent: %+v", graph.Edges)
	}
	call := graph.NodesOfKind(KindToolCall)
	if len(call) != 1 || call[0].Label != "bash" {
		t.Fatalf("tool calls = %+v", call)
	}
	if !hasEdge(graph, call[0].ID, "tool:bash", EdgeUses) {
		t.Fatalf("tool call must name the shared tool: %+v", graph.Edges)
	}
	if len(graph.Descendants(call[0].ID)) == 0 {
		t.Fatalf("tool call must reach its result and tool: %+v", graph.Edges)
	}
	for _, node := range graph.Nodes {
		if node.Provenance != ProvenanceRecorded {
			t.Fatalf("fully instrumented run produced an unknown node: %+v", node)
		}
	}
	if len(graph.Gaps) != 0 {
		t.Fatalf("fully instrumented run reported gaps: %+v", graph.Gaps)
	}
}

// TestBuildProjectsNestedSubagents covers delegation chains, declared
// dependencies, and a parent that never journaled a lifecycle of its own.
func TestBuildProjectsNestedSubagents(t *testing.T) {
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		agentEvent("coordinator", "", "implement", "gpt-5", journal.AgentCompleted),
		agentEvent("worker-a", "coordinator", "implement", "gpt-5-mini", journal.AgentCompleted),
		agentEvent("worker-b", "coordinator", "implement", "gpt-5-mini", journal.AgentCompleted, "worker-a"),
		agentEvent("orphan", "ghost", "implement", "gpt-5-mini", journal.AgentCompleted),
		{Seq: 3, Type: journal.EventRunFinished, Status: "completed"},
	}
	graph, err := Build(Input{RunID: "run-1", Workflow: "implementation", Events: events})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !hasEdge(graph, "subagent:coordinator", "subagent:worker-a", EdgeDelegates) ||
		!hasEdge(graph, "subagent:coordinator", "subagent:worker-b", EdgeDelegates) {
		t.Fatalf("nested subagents must hang off their parent: %+v", graph.Edges)
	}
	if !hasEdge(graph, "subagent:worker-b", "subagent:worker-a", EdgeDependsOn) {
		t.Fatalf("declared dependency must be an edge: %+v", graph.Edges)
	}
	ghost := nodeByID(t, graph, "subagent:ghost")
	if ghost.Provenance != ProvenanceUnknown {
		t.Fatalf("unrecorded parent = %+v, want unknown provenance", ghost)
	}
	if len(gapReasons(graph, ghost.ID)) == 0 {
		t.Fatalf("unrecorded parent must be reported as a gap: %+v", graph.Gaps)
	}
	if !hasEdge(graph, "subagent:ghost", "subagent:orphan", EdgeDelegates) ||
		!hasEdge(graph, "stage:implement#1", "subagent:ghost", EdgeContains) {
		t.Fatalf("orphan must stay reachable through its unknown parent: %+v", graph.Edges)
	}
	for _, edge := range graph.OutEdges("subagent:ghost") {
		if edge.To == "subagent:orphan" && edge.Provenance != ProvenanceUnknown {
			t.Fatalf("edge to an unrecorded parent must be unknown: %+v", edge)
		}
	}
	var reached bool
	graph.Walk(graph.RootID, func(node Node, _ int) bool {
		if node.ID == "subagent:orphan" {
			reached = true
		}
		return true
	})
	if !reached {
		t.Fatalf("every subagent must be reachable from the outcome: %+v", graph.Edges)
	}
}

// TestBuildSharesToolIdentityAcrossAgents pins that one tool used by several
// subagents is a single shared node with an edge from each call, so credit can
// be aggregated per tool without conflating the calls.
func TestBuildSharesToolIdentityAcrossAgents(t *testing.T) {
	first, second := "sha256:first", "sha256:second"
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		agentEvent("worker-a", "", "implement", "gpt-5", journal.AgentCompleted),
		agentEvent("worker-b", "", "implement", "gpt-5", journal.AgentCompleted),
		spanEvent("implement", "a.transcript", first),
		spanProvenance("implement", first, "worker-a"),
		spanEvent("implement", "b.transcript", second),
		spanProvenance("implement", second, "worker-b"),
		{Seq: 3, Type: journal.EventRunFinished, Status: "completed"},
	}
	graph, err := Build(Input{
		RunID: "run-1", Workflow: "implementation", Events: events,
		SpanData: map[string][]byte{
			first: transcript(
				`{"role":"assistant","model":"gpt-5"}`,
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"a-1","name":"bash"}}`,
				`{"role":"tool","tool_call":{"id":"a-1","success":true}}`,
			),
			second: transcript(
				`{"role":"assistant","model":"gpt-5"}`,
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"b-1","name":"bash"}}`,
				`{"role":"tool","tool_call":{"id":"b-1","success":false}}`,
			),
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tools := graph.NodesOfKind(KindTool); len(tools) != 1 || tools[0].ID != "tool:bash" {
		t.Fatalf("shared tool = %+v, want exactly one node", tools)
	}
	calls := graph.NodesOfKind(KindToolCall)
	if len(calls) != 2 {
		t.Fatalf("tool calls = %+v, want one per agent", calls)
	}
	for _, call := range calls {
		if !hasEdge(graph, call.ID, "tool:bash", EdgeUses) {
			t.Fatalf("call %q must use the shared tool: %+v", call.ID, graph.Edges)
		}
	}
	results := graph.NodesOfKind(KindToolResult)
	if len(results) != 2 || results[0].Attributes["success"] != "true" || results[1].Attributes["success"] != "false" {
		t.Fatalf("tool results = %+v, want one distinct result per call", results)
	}
	var visits int
	graph.Walk(graph.RootID, func(node Node, _ int) bool {
		if node.ID == "tool:bash" {
			visits++
		}
		return true
	})
	if visits != 1 {
		t.Fatalf("shared node visited %d times, want once per traversal", visits)
	}
}

// TestBuildKeepsSpansOfOneSubagentDistinct pins that a subagent owning more
// than one span — one per stage attempt, say — keeps every span's records as
// their own nodes, so no call is silently dropped and no result is joined to
// another span's call.
func TestBuildKeepsSpansOfOneSubagentDistinct(t *testing.T) {
	first, second := "sha256:attempt-1", "sha256:attempt-2"
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		agentEvent("worker", "", "implement", "gpt-5", journal.AgentCompleted),
		spanEvent("implement", "attempt-1.transcript", first),
		spanProvenance("implement", first, "worker"),
		spanEvent("implement", "attempt-2.transcript", second),
		spanProvenance("implement", second, "worker"),
		{Seq: 3, Type: journal.EventRunFinished, Status: "completed"},
	}
	graph, err := Build(Input{
		RunID: "run-1", Workflow: "implementation", Events: events,
		SpanData: map[string][]byte{
			first: transcript(
				`{"role":"assistant","model":"gpt-5"}`,
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"a-1","name":"bash"}}`,
				`{"role":"tool","tool_call":{"id":"a-1","success":true}}`,
			),
			second: transcript(
				`{"role":"assistant","model":"gpt-5"}`,
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"b-1","name":"grep"}}`,
				`{"role":"tool","tool_call":{"id":"b-1","success":false}}`,
			),
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	calls := graph.NodesOfKind(KindToolCall)
	if len(calls) != 2 {
		t.Fatalf("tool calls = %+v, want one per span", calls)
	}
	byCallID := map[string]Node{}
	for _, call := range calls {
		byCallID[call.Attributes["callId"]] = call
	}
	for callID, tool := range map[string]string{"a-1": "bash", "b-1": "grep"} {
		call, ok := byCallID[callID]
		if !ok || call.Label != tool {
			t.Fatalf("call %q = %+v, want a node labeled %q", callID, call, tool)
		}
	}
	results := graph.NodesOfKind(KindToolResult)
	if len(results) != 2 {
		t.Fatalf("tool results = %+v, want one per span", results)
	}
	for _, result := range results {
		call, ok := byCallID[result.Attributes["callId"]]
		if !ok {
			t.Fatalf("result %+v joins no call of its own span", result)
		}
		if !hasEdge(graph, call.ID, result.ID, EdgeProduces) {
			t.Fatalf("result %q must be produced by call %q: %+v", result.ID, call.ID, graph.Edges)
		}
	}
	if models := graph.NodesOfKind(KindModelInvocation); len(models) != 2 {
		t.Fatalf("model invocations = %+v, want both spans' invocations", models)
	}
	if len(graph.Gaps) != 0 {
		t.Fatalf("fully instrumented spans reported gaps: %+v", graph.Gaps)
	}
}

// TestBuildMarksMissingProvenanceUnknown pins the read model's refusal to
// invent links on a partially instrumented run.
func TestBuildMarksMissingProvenanceUnknown(t *testing.T) {
	linked, unavailable := "sha256:linked", "sha256:unavailable"
	events := []journal.Event{
		spanEvent("implement", "orphan.transcript", linked),
		spanEvent("review", "missing.transcript", unavailable),
		{Seq: 1, Type: journal.EventArtifactRecorded, Stage: "review", Attempt: 1, Name: "verdict"},
	}
	graph, err := Build(Input{
		RunID: "run-1", Workflow: "implementation", Events: events,
		SpanData: map[string][]byte{linked: transcript(
			`{"role":"assistant","tool_call":{"id":"call-1","name":"bash"}}`,
			`{"role":"tool","tool_call":{"id":"call-2","success":true}}`,
		)},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	outcome := nodeByID(t, graph, graph.RootID)
	if outcome.Provenance != ProvenanceUnknown || len(gapReasons(graph, outcome.ID)) == 0 {
		t.Fatalf("outcome = %+v with gaps %+v, want an explicit unknown", outcome, graph.Gaps)
	}
	run := nodeByID(t, graph, "run:run-1")
	if run.Provenance != ProvenanceUnknown {
		t.Fatalf("run without run.started = %+v, want unknown", run)
	}
	stage := nodeByID(t, graph, "stage:implement#1")
	if stage.Provenance != ProvenanceUnknown || len(gapReasons(graph, stage.ID)) == 0 {
		t.Fatalf("stage never started = %+v with gaps %+v, want an explicit unknown", stage, graph.Gaps)
	}
	owner := nodeByID(t, graph, "subagent:unknown/stage:implement#1")
	if owner.Provenance != ProvenanceUnknown {
		t.Fatalf("unattributed span owner = %+v, want unknown", owner)
	}
	calls := graph.NodesOfKind(KindToolCall)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
	callGaps := strings.Join(gapReasons(graph, calls[0].ID), "|")
	if !strings.Contains(callGaps, "no recorded model invocation") ||
		!strings.Contains(callGaps, "no result") {
		t.Fatalf("tool call gaps = %q, want the missing model invocation and result named", callGaps)
	}
	results := graph.NodesOfKind(KindToolResult)
	if len(results) != 1 || results[0].Provenance != ProvenanceUnknown {
		t.Fatalf("unjoined tool result = %+v, want unknown rather than a fabricated call", results)
	}
	if hasEdge(graph, calls[0].ID, results[0].ID, EdgeProduces) {
		t.Fatalf("a result for a different call id must not be joined: %+v", graph.Edges)
	}
	unavailableGaps := strings.Join(gapReasons(graph, "subagent:unknown/stage:review#1"), "|")
	if !strings.Contains(unavailableGaps, "no retrievable content") {
		t.Fatalf("span without content gaps = %q, want the missing content named", unavailableGaps)
	}
	evidence := graph.NodesOfKind(KindEvidence)
	if len(evidence) != 1 || evidence[0].Provenance != ProvenanceUnknown {
		t.Fatalf("digestless artifact = %+v, want unknown provenance", evidence)
	}
	for _, node := range graph.Nodes {
		if node.Kind == KindModelInvocation {
			t.Fatalf("no model invocation may be invented for an uninstrumented run: %+v", node)
		}
	}
}

// TestBuildIsDeterministic pins that the same journal projects the same graph,
// so a stored graph can be compared across rebuilds.
func TestBuildIsDeterministic(t *testing.T) {
	digest := "sha256:span"
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		agentEvent("root", "", "implement", "gpt-5", journal.AgentCompleted),
		agentEvent("child", "root", "implement", "gpt-5-mini", journal.AgentCompleted),
		spanEvent("implement", "copilot-cli.transcript", digest),
		spanProvenance("implement", digest, "root"),
		{Seq: 3, Type: journal.EventRunFinished, Status: "completed"},
	}
	input := Input{RunID: "run-1", Workflow: "implementation", Events: events,
		SpanData: map[string][]byte{digest: transcript(
			`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
		)}}
	first, err := Build(input)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := Build(input)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(next.Nodes) != len(first.Nodes) || len(next.Edges) != len(first.Edges) ||
			len(next.Gaps) != len(first.Gaps) {
			t.Fatalf("graph size drifted: %+v vs %+v", next, first)
		}
		for j := range next.Nodes {
			if next.Nodes[j].ID != first.Nodes[j].ID {
				t.Fatalf("node order drifted at %d: %q vs %q", j, next.Nodes[j].ID, first.Nodes[j].ID)
			}
		}
		for j := range next.Edges {
			if next.Edges[j] != first.Edges[j] {
				t.Fatalf("edge order drifted at %d: %+v vs %+v", j, next.Edges[j], first.Edges[j])
			}
		}
		for j := range next.Gaps {
			if next.Gaps[j] != first.Gaps[j] {
				t.Fatalf("gap order drifted at %d: %+v vs %+v", j, next.Gaps[j], first.Gaps[j])
			}
		}
	}
}
