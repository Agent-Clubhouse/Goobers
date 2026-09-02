package creditgraph

import (
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func routedAgentEvent(id, stage, requested, resolved string, outputTokens int64) journal.Event {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tokens := outputTokens
	return journal.Event{Type: journal.EventAgentLifecycle, Stage: stage, Agent: &journal.AgentProvenance{
		Schema: "goobers.dev/journal/agent/v1", ID: id, RunID: "run-1", Stage: stage, Attempt: 1,
		Lifecycle: journal.AgentCompleted, RequestedModel: requested, ResolvedModel: resolved,
		StartedAt: now, UpdatedAt: now,
		Usage: journal.AgentUsage{OutputTokens: &tokens},
	}}
}

func attributeGraph(t *testing.T, input Input) Attribution {
	t.Helper()
	graph, err := Build(input)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return Attribute(graph)
}

func causeOfClass(t *testing.T, attribution Attribution, class FailureClass) CauseFinding {
	t.Helper()
	causes := attribution.CausesOfClass(class)
	if len(causes) == 0 {
		t.Fatalf("no %q cause in %+v", class, attribution.Causes)
	}
	return causes[0]
}

func classesOf(attribution Attribution) []FailureClass {
	classes := make([]FailureClass, 0, len(attribution.Causes))
	for _, cause := range attribution.Causes {
		classes = append(classes, cause.Class)
	}
	return classes
}

func contributionOf(t *testing.T, attribution Attribution, nodeID string) Contribution {
	t.Helper()
	contribution, ok := attribution.Contribution(nodeID)
	if !ok {
		t.Fatalf("node %q has no contribution: %+v", nodeID, attribution.Contributions)
	}
	return contribution
}

// failingStageEvents is a one-stage failing run whose transcript the caller
// supplies, the shape most classification rules are read off.
func failingStageEvents(digest string) []journal.Event {
	return []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		agentEvent("root", "", "implement", "gpt-5", journal.AgentFailed),
		spanEvent("implement", "copilot-cli.transcript", digest),
		spanProvenance("implement", digest, "root"),
		{Seq: 3, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
		{Seq: 4, Type: journal.EventRunFinished, Status: "failed"},
	}
}

// TestAttributeSignsContributionsFromTheOutcome pins the headline contract:
// responsibility flows from the outcome down the graph, and a node's sign is
// its own recorded signal when it has one, so a tool that succeeded inside a
// failing run reads as a positive contribution rather than as blame.
func TestAttributeSignsContributionsFromTheOutcome(t *testing.T) {
	digest := "sha256:mixed"
	attribution := attributeGraph(t, Input{
		RunID: "run-1", Workflow: "implementation", Events: failingStageEvents(digest),
		SpanData: map[string][]byte{digest: transcript(
			`{"role":"assistant","model":"gpt-5","usage":{"input_tokens":10,"output_tokens":4}}`,
			`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
			`{"role":"tool","tool_call":{"id":"call-1","success":true}}`,
			`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-2","name":"edit"}}`,
			`{"role":"tool","tool_call":{"id":"call-2","success":false}}`,
		)},
	})
	if attribution.Schema != AttributionSchemaVersion {
		t.Fatalf("schema = %q, want %q", attribution.Schema, AttributionSchemaVersion)
	}
	if attribution.OutcomeSign != -1 || attribution.Outcome != "failed" {
		t.Fatalf("outcome = %q/%v, want the recorded failure", attribution.Outcome, attribution.OutcomeSign)
	}
	root := contributionOf(t, attribution, attribution.RootID)
	if root.Share != 1 || root.Score != -1 {
		t.Fatalf("root contribution = %+v, want the whole responsibility mass, signed negative", root)
	}
	var positive, negative int
	for _, contribution := range attribution.Contributions {
		if contribution.Kind != KindToolResult {
			continue
		}
		switch {
		case contribution.Score > 0:
			positive++
		case contribution.Score < 0:
			negative++
		}
	}
	if positive != 1 || negative != 1 {
		t.Fatalf("tool results = %d positive / %d negative, want the succeeding one credited and the failing one blamed: %+v",
			positive, negative, attribution.Contributions)
	}
	stage := contributionOf(t, attribution, "stage:implement#1")
	if stage.Score >= 0 || stage.Share <= 0 {
		t.Fatalf("failing stage contribution = %+v, want a negative share of the outcome", stage)
	}
	if len(attribution.Assumptions) == 0 {
		t.Fatalf("attribution must record the assumptions behind its estimates")
	}
	for _, assumption := range attribution.Assumptions {
		if strings.Contains(assumption, "correlational") {
			return
		}
	}
	t.Fatalf("assumptions must state that a share is correlational: %+v", attribution.Assumptions)
}

// TestAttributeSharesFlowTowardTheFailingStage checks that responsibility
// prefers the branch whose recorded signal agrees with the outcome, so the
// failing stage of a failing run outweighs its succeeding sibling.
func TestAttributeSharesFlowTowardTheFailingStage(t *testing.T) {
	attribution := attributeGraph(t, Input{
		RunID: "run-1", Workflow: "implementation", Events: []journal.Event{
			{Seq: 1, Type: journal.EventRunStarted},
			{Seq: 2, Type: journal.EventStageStarted, Stage: "plan", Attempt: 1},
			{Seq: 3, Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: "success"},
			{Seq: 4, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			{Seq: 5, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
			{Seq: 6, Type: journal.EventRunFinished, Status: "failed"},
		},
	})
	failing := contributionOf(t, attribution, "stage:implement#1")
	succeeding := contributionOf(t, attribution, "stage:plan#1")
	if failing.Share <= succeeding.Share {
		t.Fatalf("failing stage share %v must exceed the succeeding stage's %v", failing.Share, succeeding.Share)
	}
	if failing.Score >= 0 || succeeding.Score <= 0 {
		t.Fatalf("signs = %v/%v, want the failing stage negative and the succeeding one positive",
			failing.Score, succeeding.Score)
	}
}

// TestAttributeIsReproducibleForAFixedGraph pins the acceptance criterion that
// scores and uncertainty are reproducible: two builds of the same journal
// attribute identically, and attributing the same graph twice is stable.
func TestAttributeIsReproducibleForAFixedGraph(t *testing.T) {
	digest := "sha256:repeat"
	input := Input{
		RunID: "run-1", Workflow: "implementation", Events: failingStageEvents(digest),
		SpanData: map[string][]byte{digest: transcript(
			`{"role":"assistant","model":"gpt-5","usage":{"input_tokens":10,"output_tokens":4}}`,
			`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
			`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
		)},
	}
	first := attributeGraph(t, input)
	second := attributeGraph(t, input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("attribution is not reproducible:\nfirst  = %+v\nsecond = %+v", first, second)
	}
	graph, err := Build(input)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !reflect.DeepEqual(Attribute(graph), Attribute(graph)) {
		t.Fatalf("attributing one graph twice differs")
	}
}

// TestAttributeClassifiesEachFailureClass exercises the taxonomy one class at
// a time, because the acceptance criterion is that classification is explicit
// and testable per class.
func TestAttributeClassifiesEachFailureClass(t *testing.T) {
	digest := "sha256:class"
	tests := []struct {
		name  string
		class FailureClass
		input Input
	}{
		{
			name: "bad tool result", class: ClassBadToolResult,
			input: Input{RunID: "run-1", Events: failingStageEvents(digest), SpanData: map[string][]byte{digest: transcript(
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
				`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
			)}},
		},
		{
			name: "bad tool choice", class: ClassBadToolChoice,
			input: Input{RunID: "run-1", Events: failingStageEvents(digest), SpanData: map[string][]byte{digest: transcript(
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"grep"}}`,
				`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-2","name":"bash"}}`,
				`{"role":"tool","tool_call":{"id":"call-2","success":true}}`,
			)}},
		},
		{
			name: "bad interpretation", class: ClassBadInterpretation,
			input: Input{RunID: "run-1", Events: failingStageEvents(digest), SpanData: map[string][]byte{digest: transcript(
				`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
				`{"role":"tool","tool_call":{"id":"call-1","success":true}}`,
			)}},
		},
		{
			name: "weak instructions", class: ClassWeakInstructions,
			input: Input{RunID: "run-1", Events: []journal.Event{
				{Seq: 1, Type: journal.EventRunStarted},
				{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
				agentEvent("root", "", "implement", "gpt-5", journal.AgentFailed),
				{Seq: 3, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
				{Seq: 4, Type: journal.EventRunFinished, Status: "failed"},
			}},
		},
		{
			name: "routing", class: ClassRouting,
			input: Input{RunID: "run-1", Events: []journal.Event{
				{Seq: 1, Type: journal.EventRunStarted},
				{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
				routedAgentEvent("root", "implement", "gpt-5", "gpt-4.1", 12),
				{Seq: 3, Type: journal.EventArtifactRecorded, Stage: "implement", Attempt: 1,
					Name: "diff", Ref: &journal.Ref{Digest: "sha256:diff"}, Integrity: apiv1.IntegrityUnapproved},
				{Seq: 4, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
				{Seq: 5, Type: journal.EventRunFinished, Status: "failed"},
			}},
		},
		{
			name: "model", class: ClassModel,
			input: Input{RunID: "run-1", Events: []journal.Event{
				{Seq: 1, Type: journal.EventRunStarted},
				{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
				routedAgentEvent("root", "implement", "gpt-5", "gpt-5", 0),
				{Seq: 3, Type: journal.EventArtifactRecorded, Stage: "implement", Attempt: 1,
					Name: "diff", Ref: &journal.Ref{Digest: "sha256:diff"}, Integrity: apiv1.IntegrityUnapproved},
				{Seq: 4, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
				{Seq: 5, Type: journal.EventRunFinished, Status: "failed"},
			}},
		},
		{
			name: "topology", class: ClassTopology,
			input: Input{RunID: "run-1", Events: []journal.Event{
				{Seq: 1, Type: journal.EventRunStarted},
				{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
				agentEvent("root", "", "implement", "gpt-5", journal.AgentFailed, "never-ran"),
				{Seq: 3, Type: journal.EventArtifactRecorded, Stage: "implement", Attempt: 1,
					Name: "diff", Ref: &journal.Ref{Digest: "sha256:diff"}, Integrity: apiv1.IntegrityUnapproved},
				{Seq: 4, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
				{Seq: 5, Type: journal.EventRunFinished, Status: "failed"},
			}},
		},
		{
			name: "environment", class: ClassEnvironment,
			input: Input{RunID: "run-1", Events: []journal.Event{
				{Seq: 1, Type: journal.EventRunStarted},
				{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
				agentEvent("root", "", "implement", "gpt-5", journal.AgentFailed),
				{Seq: 3, Type: journal.EventArtifactRecorded, Stage: "implement", Attempt: 1,
					Name: "diff", Ref: &journal.Ref{Digest: "sha256:diff"}, Integrity: apiv1.IntegrityUnapproved},
				{Seq: 4, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "error"},
				{Seq: 5, Type: journal.EventRunFinished, Status: "failed"},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attribution := attributeGraph(t, test.input)
			cause := causeOfClass(t, attribution, test.class)
			if cause.Stage != "implement" || cause.NodeID == "" || cause.Summary == "" {
				t.Fatalf("cause = %+v, want it to name the stage, a node, and a summary", cause)
			}
			if cause.Confidence <= 0 || cause.Confidence > 1 {
				t.Fatalf("confidence = %v, want a value in (0,1]", cause.Confidence)
			}
			for _, class := range classesOf(attribution) {
				if class == ClassUnknown {
					t.Fatalf("a classified failure must not also report an unknown cause: %+v", attribution.Causes)
				}
			}
		})
	}
}

// TestAttributeReportsUnknownCauseWhenNoSignalDistinguishesOne covers the
// unknown-cause criterion: a failing stage the journal describes but does not
// explain is left unattributed rather than blamed on its nearest component.
func TestAttributeReportsUnknownCauseWhenNoSignalDistinguishesOne(t *testing.T) {
	attribution := attributeGraph(t, Input{
		RunID: "run-1", Events: []journal.Event{
			{Seq: 1, Type: journal.EventRunStarted},
			{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			{Seq: 3, Type: journal.EventArtifactRecorded, Stage: "implement", Attempt: 1,
				Name: "diff", Ref: &journal.Ref{Digest: "sha256:diff"}, Integrity: apiv1.IntegrityUnapproved},
			{Seq: 4, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
			{Seq: 5, Type: journal.EventRunFinished, Status: "failed"},
		},
	})
	cause := causeOfClass(t, attribution, ClassUnknown)
	if len(attribution.Causes) != 1 {
		t.Fatalf("causes = %+v, want only the unknown one", attribution.Causes)
	}
	if cause.Confidence > unknownCauseCeiling {
		t.Fatalf("unknown cause confidence = %v, want at most %v", cause.Confidence, unknownCauseCeiling)
	}
	if len(cause.Assumptions) == 0 {
		t.Fatalf("an unknown cause must record why it declines to attribute: %+v", cause)
	}
}

// TestAttributeReportsUnknownCauseWhenTheRunFailsWithoutAFailingStage keeps a
// failing outcome from being pinned on whichever stage happens to be present.
func TestAttributeReportsUnknownCauseWhenTheRunFailsWithoutAFailingStage(t *testing.T) {
	attribution := attributeGraph(t, Input{
		RunID: "run-1", Events: []journal.Event{
			{Seq: 1, Type: journal.EventRunStarted},
			{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			{Seq: 3, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "success"},
			{Seq: 4, Type: journal.EventRunFinished, Status: "failed"},
		},
	})
	cause := causeOfClass(t, attribution, ClassUnknown)
	if cause.NodeID != attribution.RootID {
		t.Fatalf("cause = %+v, want it attached to the outcome", cause)
	}
}

// TestAttributeLowersConfidenceWhenProvenanceIsMissing pins the rule that a
// hole in the journal reduces confidence instead of producing blame.
func TestAttributeLowersConfidenceWhenProvenanceIsMissing(t *testing.T) {
	digest := "sha256:absent"
	instrumented := attributeGraph(t, Input{
		RunID: "run-1", Events: failingStageEvents(digest),
		SpanData: map[string][]byte{digest: transcript(
			`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
			`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
		)},
	})
	blind := attributeGraph(t, Input{RunID: "run-1", Events: failingStageEvents(digest)})

	instrumentedStage := contributionOf(t, instrumented, "stage:implement#1")
	blindStage := contributionOf(t, blind, "stage:implement#1")
	if blindStage.Confidence >= instrumentedStage.Confidence {
		t.Fatalf("missing provenance must lower confidence: blind %v vs instrumented %v",
			blindStage.Confidence, instrumentedStage.Confidence)
	}
	if blindStage.Uncertainty <= instrumentedStage.Uncertainty {
		t.Fatalf("missing provenance must raise uncertainty: blind %v vs instrumented %v",
			blindStage.Uncertainty, instrumentedStage.Uncertainty)
	}
	cause := causeOfClass(t, blind, ClassUnknown)
	if len(blind.Causes) != 1 || !strings.Contains(cause.Summary, "provenance is missing") {
		t.Fatalf("causes = %+v, want a single unattributed finding naming the missing provenance", blind.Causes)
	}
	if instrumented.CausesOfClass(ClassBadToolResult) == nil {
		t.Fatalf("the instrumented run must still classify its recorded failure: %+v", instrumented.Causes)
	}
}

// TestAttributeHandlesCorrelatedFailures covers two stages failing on the same
// tool: each stage keeps its own finding, and the shared tool accumulates
// responsibility from both without the finding claiming it caused either.
func TestAttributeHandlesCorrelatedFailures(t *testing.T) {
	first, second := "sha256:one", "sha256:two"
	failing := transcript(
		`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
		`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
	)
	attribution := attributeGraph(t, Input{
		RunID: "run-1", Events: []journal.Event{
			{Seq: 1, Type: journal.EventRunStarted},
			{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			agentEvent("implementer", "", "implement", "gpt-5", journal.AgentFailed),
			spanEvent("implement", "implement.transcript", first),
			spanProvenance("implement", first, "implementer"),
			{Seq: 3, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
			{Seq: 4, Type: journal.EventStageStarted, Stage: "verify", Attempt: 1},
			agentEvent("verifier", "", "verify", "gpt-5", journal.AgentFailed),
			spanEvent("verify", "verify.transcript", second),
			spanProvenance("verify", second, "verifier"),
			{Seq: 5, Type: journal.EventStageFinished, Stage: "verify", Attempt: 1, Status: "failure"},
			{Seq: 6, Type: journal.EventRunFinished, Status: "failed"},
		},
		SpanData: map[string][]byte{first: failing, second: failing},
	})
	causes := attribution.CausesOfClass(ClassBadToolResult)
	if len(causes) != 2 || causes[0].Stage != "implement" || causes[1].Stage != "verify" {
		t.Fatalf("causes = %+v, want one per failing stage, in graph order", causes)
	}
	tool := contributionOf(t, attribution, "tool:bash")
	implement := contributionOf(t, attribution, "stage:implement#1")
	if tool.Score >= 0 {
		t.Fatalf("the shared tool = %+v, want a negative share of a failing run", tool)
	}
	if tool.Share <= implement.Share/2 {
		t.Fatalf("the shared tool must accumulate responsibility from both stages: %v vs %v",
			tool.Share, implement.Share)
	}
	for _, cause := range causes {
		if cause.Class == ClassBadToolChoice {
			t.Fatalf("a tool failing in both stages is correlation, not a wrong choice: %+v", cause)
		}
	}
}

// TestAttributeLowersConfidenceOnContradictoryEvidence covers a stage whose
// signals disagree: a failing tool result beside successful ones and an
// evaluator that passed the stage the run recorded as failing.
func TestAttributeLowersConfidenceOnContradictoryEvidence(t *testing.T) {
	digest := "sha256:contradiction"
	spans := map[string][]byte{digest: transcript(
		`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
		`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
		`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-2","name":"bash"}}`,
		`{"role":"tool","tool_call":{"id":"call-2","success":true}}`,
	)}
	consistent := attributeGraph(t, Input{RunID: "run-1", Events: failingStageEvents(digest), SpanData: spans})

	contradicted := attributeGraph(t, Input{
		RunID: "run-1", SpanData: spans,
		Events: append(failingStageEvents(digest),
			journal.Event{Seq: 5, Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass",
				Stage: "implement", Attempt: 1}),
	})
	before := causeOfClass(t, consistent, ClassBadToolResult)
	after := causeOfClass(t, contradicted, ClassBadToolResult)
	if after.Confidence >= before.Confidence {
		t.Fatalf("a passing verdict on a failing stage must lower confidence: %v vs %v",
			after.Confidence, before.Confidence)
	}
	joined := strings.Join(after.Assumptions, "\n")
	if !strings.Contains(joined, "tool result(s) in the same stage succeeded") ||
		!strings.Contains(joined, "passing verdict") {
		t.Fatalf("assumptions must record both contradictions: %+v", after.Assumptions)
	}
}

// TestAttributeRecordsInterventionEvidence checks that a repeated stage attempt
// whose outcome differed is recorded as the natural experiment it is, and
// raises the finding's confidence above the purely correlational case.
func TestAttributeRecordsInterventionEvidence(t *testing.T) {
	digest := "sha256:retry"
	spans := map[string][]byte{digest: transcript(
		`{"role":"assistant","model":"gpt-5","tool_call":{"id":"call-1","name":"bash"}}`,
		`{"role":"tool","tool_call":{"id":"call-1","success":false}}`,
	)}
	once := attributeGraph(t, Input{RunID: "run-1", Events: failingStageEvents(digest), SpanData: spans})
	retried := attributeGraph(t, Input{
		RunID: "run-1", SpanData: spans,
		Events: append(failingStageEvents(digest),
			journal.Event{Seq: 5, Type: journal.EventStageStarted, Stage: "implement", Attempt: 2},
			journal.Event{Seq: 6, Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Status: "success"}),
	})
	before := causeOfClass(t, once, ClassBadToolResult)
	after := causeOfClass(t, retried, ClassBadToolResult)
	if after.Confidence <= before.Confidence {
		t.Fatalf("intervention evidence must raise confidence: %v vs %v", after.Confidence, before.Confidence)
	}
	found := false
	for _, evidence := range after.Evidence {
		if strings.HasPrefix(evidence, "intervention:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence = %+v, want the retried attempt recorded as an intervention", after.Evidence)
	}
}

// TestAttributeClassifiesNothingForASucceedingRun keeps the classifier from
// manufacturing a cause where there is no failure.
func TestAttributeClassifiesNothingForASucceedingRun(t *testing.T) {
	attribution := attributeGraph(t, Input{
		RunID: "run-1", Events: []journal.Event{
			{Seq: 1, Type: journal.EventRunStarted},
			{Seq: 2, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			agentEvent("root", "", "implement", "gpt-5", journal.AgentCompleted),
			{Seq: 3, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "success"},
			{Seq: 4, Type: journal.EventRunFinished, Status: "completed"},
		},
	})
	if len(attribution.Causes) != 0 {
		t.Fatalf("causes = %+v, want none for a succeeding run", attribution.Causes)
	}
	stage := contributionOf(t, attribution, "stage:implement#1")
	if stage.Score <= 0 {
		t.Fatalf("stage contribution = %+v, want positive credit in a succeeding run", stage)
	}
}

// TestAttributeNilGraph keeps the read model safe for a caller that could not
// build a graph at all.
func TestAttributeNilGraph(t *testing.T) {
	attribution := Attribute(nil)
	if attribution.Schema != AttributionSchemaVersion || len(attribution.Contributions) != 0 {
		t.Fatalf("attribution = %+v, want an empty, schema-stamped result", attribution)
	}
}
