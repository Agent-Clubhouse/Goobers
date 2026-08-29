package journal

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/validate"
)

// TestEmittedBytesMatchSchema validates the journal's actual on-disk output
// against the checked-in JSON schemas, so the Go event/identity types and the
// api/schemas contract cannot drift apart.
func TestEmittedBytesMatchSchema(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}

	_, scrub := DefaultScrubber()
	root := t.TempDir()
	run, err := Create(root, testIdentity(), map[string][]byte{
		"issue.md": []byte("issue body"),
	}, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	art, err := run.RecordArtifact("plan.txt", []byte("a plan"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	// A real span.recorded event with DataSchema populated (#2042): the
	// harness executor defaults every span to telemetry.GenAIEventSchema
	// ("goobers.dev/telemetry/genai-event/v1" — not imported here to avoid
	// journal<->telemetry, which already imports journal) whenever the
	// adapter leaves TranscriptSchema empty, so this is the common case,
	// not an edge case — the schema must accept it.
	if _, err := run.RecordSpanWithSchema("impl", "transcript", "goobers.dev/telemetry/genai-event/v1", []byte(`{"role":"assistant"}`)); err != nil {
		t.Fatalf("RecordSpanWithSchema: %v", err)
	}
	// Exercise a representative spread of event shapes.
	for _, ev := range []Event{
		{Type: EventStageStarted, Stage: "impl", Attempt: 1},
		{
			Type: EventStageRerunRequested, Stage: "impl", Attempt: 2,
			Actor: "maintainer@example.com", InstructionAddendum: "Reuse the existing parser.",
		},
		{Type: EventStageHeartbeat, Stage: "impl", Attempt: 1},
		{Type: EventStageFinished, Stage: "impl", Attempt: 2, AttemptClass: AttemptPolicy, Status: "success"},
		// Outputs/Artifacts populated (#107/#108's resume reconstruction) —
		// proves the schema's declared "outputs"/"artifacts" properties stay
		// in sync with the Go Event type, not just the zero-value (omitted)
		// case above.
		{
			Type: EventStageFinished, Stage: "impl", Attempt: 3, Status: "success",
			Outputs:   map[string]any{"ciStatus": "success", "coverage": 81.2},
			Artifacts: []Ref{{Path: art.Path, Digest: art.Digest, Size: art.Size}},
		},
		{Type: EventGateStarted, Gate: "review", Runner: map[string]any{"repassAttempt": 1}},
		{Type: EventGatePaused, Gate: "approval"},
		{Type: EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Target: "park-escalated", Escalated: true},
		{
			Type: EventGateOverridden, Gate: "review", Verdict: "pass",
			Actor: "operator@example.test", Rationale: "Reviewed the nondeterministic finding.", Status: string(PhaseEscalated),
			Target: "@complete", WorkflowVersion: testIdentity().WorkflowVersion, WorkflowDigest: testIdentity().WorkflowDigest,
		},
		{
			Type: EventRunResumed, Status: string(PhaseEscalated), Complete: true,
			Actor: "operator@example.test", Action: "override", Gate: "review",
			Decision: "pass", Rationale: "accepted risk",
			WorkflowVersion: testIdentity().WorkflowVersion,
			WorkflowDigest:  testIdentity().WorkflowDigest,
		},
		{Type: EventRefTouched, ExternalRef: &ExternalRef{Provider: "github", Kind: "pr", ID: "9"}},
		{Type: EventError, Error: &ErrorDetail{Code: "boom", Message: "detail"}},
		// Runner-scoped isolation posture record (#1305): payload entirely
		// under runner.*, proving the schema keeps pace with the type. The
		// payload mirrors the harness executor's actual emission shape
		// (posture + mechanism + worktree scope).
		{Type: EventRunnerIsolationPosture, Stage: "impl", Attempt: 1, Runner: map[string]any{
			"posture": "enforced", "mechanism": "seatbelt", "workspace": "/work/run-1/impl",
		}},
		{Type: EventRunFinished, Status: string(PhaseCompleted)},
		{Type: EventAgentLifecycle, Agent: &AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker-1", RunID: testIdentity().RunID,
			Stage: "impl", Attempt: 1, Lifecycle: AgentCompleted,
			StartedAt: fixedClock()(), UpdatedAt: fixedClock()(),
		}},
		{Type: EventAgentMessage, PeerMessage: &PeerMessageMetadata{
			ID: "message-1", SenderID: "worker-1", RecipientID: "coordinator",
			OccurredAt: fixedClock()(), Purpose: "completion",
		}},
	} {
		if err := run.Append(ev); err != nil {
			t.Fatalf("Append %s: %v", ev.Type, err)
		}
	}
	_ = run.Close()

	dir := filepath.Join(root, testIdentity().RunID)

	// Every emitted event line validates against the event schema.
	f, err := os.Open(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventBytes)
	n := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := v.ValidateJSON("journal-event.schema.json", line); err != nil {
			t.Errorf("event line %d fails schema: %v\n%s", n, err, line)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no events emitted")
	}

	// run.yaml validates against the run schema.
	yb, err := os.ReadFile(filepath.Join(dir, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	jb, err := yaml.YAMLToJSON(yb)
	if err != nil {
		t.Fatalf("run.yaml -> json: %v", err)
	}
	if err := v.ValidateJSON("journal-run.schema.json", jb); err != nil {
		t.Errorf("run.yaml fails schema: %v\n%s", err, jb)
	}

	// schema.json validates against the directory metadata schema.
	sb, err := os.ReadFile(filepath.Join(dir, fileSchema))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateJSON("journal-schema.schema.json", sb); err != nil {
		t.Errorf("schema.json fails schema: %v\n%s", err, sb)
	}
}

// TestSchemaRejectsMalformedEvent guards that the schema actually constrains —
// an unknown event type and a missing required field are both rejected.
func TestSchemaRejectsMalformedEvent(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}

	bad := [][]byte{
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"not.a.real.type"}`),
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z"}`), // missing type
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"artifact.recorded","ref":{"path":"x","digest":"notasha","size":1}}`),
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"gate.overridden","gate":"review","verdict":"pass","target":"@complete","actor":"operator","status":"escalated","workflowVersion":1,"workflowDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"gate.overridden","gate":"review","verdict":"pass","actor":"operator","rationale":"manual inspection","status":"escalated","workflowVersion":1,"workflowDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"gate.overridden","gate":"review","verdict":"pass","target":"","actor":"operator","rationale":"manual inspection","status":"escalated","workflowVersion":1,"workflowDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"notification.requested"}`),
		[]byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"notification.delivery.receipt"}`),
	}
	for i, b := range bad {
		if err := v.ValidateJSON("journal-event.schema.json", b); err == nil {
			t.Errorf("case %d: schema accepted malformed event: %s", i, b)
		}
	}
}

func TestMarshalEventRejectsGateOverrideWithoutTarget(t *testing.T) {
	if _, err := marshalEvent(Event{Type: EventGateOverridden}); err == nil {
		t.Fatal("marshalEvent accepted gate override without target")
	}
}

func TestMarshalEventRejectsNotificationWithoutTypedPayload(t *testing.T) {
	for _, eventType := range []EventType{EventNotificationRequested, EventNotificationReceipt} {
		if _, err := marshalEvent(Event{Type: eventType}); err == nil {
			t.Fatalf("marshalEvent accepted %s without its typed payload", eventType)
		}
	}
}

// TestIdentityRefusesUnknownSchema pins #2054: run.yaml is written once at
// Create and never migrated in place, so a reader has no way to safely
// reinterpret a shape it does not own. Before this fix, Identity() bare-
// unmarshaled any schema version — a future run/v2 reshape would silently
// zero-value fields this build doesn't recognize rather than refusing, the
// same class of bug the event stream and CLI envelope already guard against.
func TestIdentityRefusesUnknownSchema(t *testing.T) {
	run, root := newRun(t)
	dir := filepath.Join(root, testIdentity().RunID)
	_ = run.Close()

	runYAMLPath := filepath.Join(dir, fileRunYAML)
	b, err := os.ReadFile(runYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(b, []byte("schema: "+RunSchema), []byte("schema: goobers.dev/journal/run/v2"), 1)
	if bytes.Equal(tampered, b) {
		t.Fatal("test setup: schema line not found in run.yaml")
	}
	if err := os.WriteFile(runYAMLPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	if _, err := reader.Identity(); err == nil {
		t.Fatal("Identity accepted an unknown run schema version instead of refusing it")
	} else if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error = %v; want a clear unsupported-schema refusal", err)
	}
}

// TestStateRefusesUnknownSchema mirrors TestIdentityRefusesUnknownSchema for
// state.json (#2054).
func TestStateRefusesUnknownSchema(t *testing.T) {
	run, root := newRun(t)
	dir := filepath.Join(root, testIdentity().RunID)
	if err := run.Append(Event{Type: EventStageStarted, Stage: "impl", Attempt: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = run.Close()

	statePath := filepath.Join(dir, fileState)
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(b, []byte(`"schema": "`+StateSchema+`"`), []byte(`"schema": "goobers.dev/journal/state/v2"`), 1)
	if bytes.Equal(tampered, b) {
		t.Fatal("test setup: schema field not found in state.json")
	}
	if err := os.WriteFile(statePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	if _, err := reader.State(); err == nil {
		t.Fatal("State accepted an unknown state schema version instead of refusing it")
	} else if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error = %v; want a clear unsupported-schema refusal", err)
	}
}

func TestSchemaRejectsPartialPinnedRunControls(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	run := []byte(`{
		"schema":"goobers.dev/journal/run/v1",
		"runId":"0123456789abcdef0123456789abcdef",
		"workflow":"build",
		"workflowVersion":1,
		"gaggle":"web",
		"runControls":{"maxRepasses":3},
		"trigger":{"kind":"manual"},
		"startedAt":"2026-07-20T20:00:00Z"
	}`)
	if err := v.ValidateJSON("journal-run.schema.json", run); err == nil {
		t.Fatal("journal schema accepted partially pinned runControls")
	}
}
