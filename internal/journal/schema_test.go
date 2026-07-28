package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/schemas"
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
		// Runner-scoped isolation posture record (#1305): payload entirely
		// under runner.*, proving the schema keeps pace with the type. The
		// payload mirrors the harness executor's actual emission shape
		// (posture + mechanism + worktree scope).
		{Type: EventRunnerIsolationPosture, Stage: "impl", Attempt: 1, Runner: map[string]any{
			"posture": "enforced", "mechanism": "seatbelt", "workspace": "/work/run-1/impl",
		}},
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

// eventTypeConstsFromSource parses event.go and returns every string value
// assigned to an EventType const. It reads the source rather than a hand-kept
// list so the schema drift guard below cannot itself fall behind the taxonomy.
func eventTypeConstsFromSource(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "event.go", nil, 0)
	if err != nil {
		t.Fatalf("parse event.go: %v", err)
	}
	consts := make(map[string]string)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "EventType" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s value: %v", name.Name, err)
				}
				consts[name.Name] = value
			}
		}
	}
	if len(consts) == 0 {
		t.Fatal("no EventType consts found in event.go")
	}
	return consts
}

// schemaEventTypeEnum returns the property.type enum from the embedded
// journal-event schema, failing on any duplicate entry.
func schemaEventTypeEnum(t *testing.T) []string {
	t.Helper()
	raw, err := schemas.FS.ReadFile("journal-event.schema.json")
	if err != nil {
		t.Fatalf("read journal-event schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Type struct {
				Enum []string `json:"enum"`
			} `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal journal-event schema: %v", err)
	}
	seen := make(map[string]bool, len(doc.Properties.Type.Enum))
	for _, e := range doc.Properties.Type.Enum {
		if seen[e] {
			t.Errorf("schema type enum lists %q more than once", e)
		}
		seen[e] = true
	}
	return doc.Properties.Type.Enum
}

// TestEventTypeConstsAppearInSchemaEnum is the load-bearing anti-drift guard
// (#1576): every EventType the shipped code can emit must be a member of the
// published schema's closed type enum, or a consumer validating a real journal
// would reject events the runner legitimately wrote. Deriving both sides from
// their sources means a newly added event type fails here until the enum is
// updated, rather than silently invalidating the contract.
func TestEventTypeConstsAppearInSchemaEnum(t *testing.T) {
	consts := eventTypeConstsFromSource(t)
	enum := schemaEventTypeEnum(t)
	inEnum := make(map[string]bool, len(enum))
	for _, e := range enum {
		inEnum[e] = true
	}
	for name, value := range consts {
		if !inEnum[value] {
			t.Errorf("EventType %s = %q is not in journal-event schema type enum", name, value)
		}
	}

	// The reverse direction keeps dead entries from accumulating: every enum
	// value must correspond to an emittable EventType const.
	values := make(map[string]bool, len(consts))
	for _, v := range consts {
		values[v] = true
	}
	for _, e := range enum {
		if !values[e] {
			t.Errorf("journal-event schema type enum lists %q with no matching EventType const", e)
		}
	}
}
