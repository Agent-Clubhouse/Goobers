package journal

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
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
			Type: EventRunResumed, Status: string(PhaseEscalated), Target: "impl",
			Actor: "operator@example.test", WorkflowVersion: testIdentity().WorkflowVersion,
			WorkflowDigest: testIdentity().WorkflowDigest,
		},
		{Type: EventRefTouched, ExternalRef: &ExternalRef{Provider: "github", Kind: "pr", ID: "9"}},
		{Type: EventError, Error: &ErrorDetail{Code: "boom", Message: "detail"}},
		{Type: EventRunFinished, Status: string(PhaseCompleted)},
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
	}
	for i, b := range bad {
		if err := v.ValidateJSON("journal-event.schema.json", b); err == nil {
			t.Errorf("case %d: schema accepted malformed event: %s", i, b)
		}
	}
}
