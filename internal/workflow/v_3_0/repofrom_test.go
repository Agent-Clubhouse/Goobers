package v30

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func loadGoldenDefinition(t *testing.T, file string) Definition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", file))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var parsed apiv1.Workflow
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return Definition{Name: parsed.Name, Version: 1, DSLVersion: parsed.DSLVersion, Spec: parsed.Spec}
}

// TestDiscriminatorFixtureCoverage is the §9 item 10 compile-half
// discriminator (delivery decisions 001/002): on the goobers/implementation
// shape, reaching definitions over the stage graph — gate-fail edges
// included, fixed-point over the cycle — must yield [implement, remediate-ci]
// at local-ci. A [implement]-only result is the back-edge-pruning failure and
// an automatic fail.
func TestDiscriminatorFixtureCoverage(t *testing.T) {
	def := loadGoldenDefinition(t, "implementation.yaml")

	if _, err := compileAcknowledged(def); err != nil {
		t.Fatalf("the discriminator fixture must compile: %v", err)
	}

	coverage := RepoFromCoverage(def)
	if coverage == nil {
		t.Fatal("RepoFromCoverage returned nil for a structurally valid definition")
	}
	if got, want := coverage["local-ci"], []string{"implement", "remediate-ci"}; !slices.Equal(got, want) {
		t.Fatalf("local-ci coverage = %v, want %v (a [implement]-only result is the back-edge-pruning failure)", got, want)
	}
	if got, want := coverage["push-branch"], []string{"implement", "remediate-ci"}; !slices.Equal(got, want) {
		t.Fatalf("push-branch coverage = %v, want %v", got, want)
	}
	// remediate-ci's own attempts are excluded: only implement remains.
	if got, want := coverage["remediate-ci"], []string{"implement"}; !slices.Equal(got, want) {
		t.Fatalf("remediate-ci coverage = %v, want %v (own attempts excluded)", got, want)
	}
	// The first repo stage of the run has no incoming edge.
	if got := coverage["implement"]; len(got) != 0 {
		t.Fatalf("implement coverage = %v, want empty (creates the branch)", got)
	}
}

// TestDiscriminatorImplementOnlyDeclarationFailsWF022 pins the other half of
// the discriminator: the same shape declaring only [implement] at local-ci —
// the answer back-edge pruning would compute — must refuse to compile,
// naming the uncovered producer.
func TestDiscriminatorImplementOnlyDeclarationFailsWF022(t *testing.T) {
	def := loadGoldenDefinition(t, "implementation.yaml")
	for i := range def.Spec.Tasks {
		if def.Spec.Tasks[i].Name == "local-ci" {
			def.Spec.Tasks[i].RepoFrom = apiv1.RepoFrom{"implement"}
		}
	}
	_, err := compileAcknowledged(def)
	if err == nil ||
		!strings.Contains(err.Error(), `task "local-ci" repoFrom "implement" does not cover producer "remediate-ci"`) {
		t.Fatalf("Compile error = %v, want WF022 uncovered-producer rejection naming remediate-ci", err)
	}
}

// TestUndeclaredChainFailsWF022 is §9 item 7's compile half: the 2.0-style
// implicit repo chain written in a 3.0 document is refused.
func TestUndeclaredChainFailsWF022(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "local-ci"},
			{Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "ci",
				Run: &apiv1.DeterministicRun{Command: []string{"make", "ci"}}},
		},
	}
	_, err := compileAcknowledged(Definition{Name: "implicit-chain", Version: 1, Spec: spec})
	if err == nil ||
		!strings.Contains(err.Error(), `task "local-ci" runs on the repo workspace after producer(s) "implement" but declares no repoFrom`) ||
		!strings.Contains(err.Error(), "declare repoFrom: implement") {
		t.Fatalf("Compile error = %v, want WF022 undeclared-chain rejection with a declaration hint", err)
	}

	// Declaring the edge fixes it.
	spec.Tasks[1].RepoFrom = apiv1.RepoFrom{"implement"}
	if _, err := compileAcknowledged(Definition{Name: "declared-chain", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("declared chain must compile: %v", err)
	}
}

func TestRepoFromDeadEntriesFailWF022(t *testing.T) {
	base := func() apiv1.WorkflowSpec {
		return apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    "implement",
			Tasks: []apiv1.Task{
				{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "local-ci"},
				{Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "ci",
					Run:      &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
					RepoFrom: apiv1.RepoFrom{"implement"}, Next: "report"},
				{Name: "report", Type: apiv1.TaskDeterministic, Goal: "report",
					Run: &apiv1.DeterministicRun{Command: []string{"make", "report"}, Workspace: apiv1.WorkspaceScratch}},
			},
		}
	}

	// An undefined stage name.
	spec := base()
	spec.Tasks[1].RepoFrom = apiv1.RepoFrom{"implement", "ghost"}
	_, err := compileAcknowledged(Definition{Name: "ghost", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `repoFrom names "ghost", which is not a defined task`) {
		t.Fatalf("Compile error = %v, want undefined-task dead entry", err)
	}

	// A defined stage that never produces.
	spec = base()
	spec.Tasks[1].RepoFrom = apiv1.RepoFrom{"implement", "report"}
	_, err = compileAcknowledged(Definition{Name: "non-producer", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `repoFrom names "report", which never advances the run branch`) {
		t.Fatalf("Compile error = %v, want non-producer dead entry", err)
	}

	// The stage itself.
	spec = base()
	spec.Tasks[1].RepoFrom = apiv1.RepoFrom{"implement", "local-ci"}
	_, err = compileAcknowledged(Definition{Name: "self", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `repoFrom names the stage itself`) {
		t.Fatalf("Compile error = %v, want self-reference rejection", err)
	}

	// A producer that can never immediately precede the consumer: remediate
	// sits AFTER local-ci with no path back.
	spec = base()
	spec.Tasks[2] = apiv1.Task{
		Name: "remediate", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "fix",
		RepoFrom: apiv1.RepoFrom{"implement"},
	}
	spec.Tasks[1].Next = "remediate"
	spec.Tasks[1].RepoFrom = apiv1.RepoFrom{"implement", "remediate"}
	_, err = compileAcknowledged(Definition{Name: "unreachable-producer", Version: 1, Spec: spec})
	if err == nil ||
		!strings.Contains(err.Error(), `repoFrom names "remediate", but no forward path reaches "local-ci" with "remediate" as its last producer`) {
		t.Fatalf("Compile error = %v, want never-precedes dead entry", err)
	}

	// repoFrom on a non-repo stage is dead config.
	spec = base()
	spec.Tasks[2].RepoFrom = apiv1.RepoFrom{"implement"}
	_, err = compileAcknowledged(Definition{Name: "scratch-repofrom", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `task "report" declares repoFrom but does not run on the writable repo workspace`) {
		t.Fatalf("Compile error = %v, want non-consumer repoFrom rejection", err)
	}
}

// TestProducerClassification pins the dsl-3.0.md §4 commit reading: agentic
// non-readonly stages and the ref-advancing builtins produce; publish-only
// builtins and plain deterministic stages do not; commitsRepo opts a
// committing script in.
func TestProducerClassification(t *testing.T) {
	agentic := apiv1.Task{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"}
	if !isRepoProducer(agentic) {
		t.Error("agentic repo stage must be a producer")
	}
	readonly := agentic
	readonly.Workspace = apiv1.WorkspaceRepoReadOnly
	if isRepoProducer(readonly) {
		t.Error("agentic repo-readonly stage must not be a producer")
	}
	scratch := agentic
	scratch.Workspace = apiv1.WorkspaceScratch
	if isRepoProducer(scratch) {
		t.Error("agentic scratch stage must not be a producer")
	}

	builtin := func(sub string) apiv1.Task {
		return apiv1.Task{Name: sub, Type: apiv1.TaskDeterministic, Goal: sub,
			Run: &apiv1.DeterministicRun{Command: []string{"goobers", sub}}}
	}
	for _, producer := range []string{"rebase-pr", "update-behind-pr"} {
		if !isRepoProducer(builtin(producer)) {
			t.Errorf("%s advances the run-branch ref and must be a producer", producer)
		}
	}
	for _, consumer := range []string{"push-branch", "push-remediated", "open-pr", "backlog-query"} {
		if isRepoProducer(builtin(consumer)) {
			t.Errorf("%s publishes or reads and must not be a producer", consumer)
		}
	}

	script := apiv1.Task{Name: "commit", Type: apiv1.TaskDeterministic, Goal: "commit",
		Run: &apiv1.DeterministicRun{Script: "git commit -am wip"}}
	if isRepoProducer(script) {
		t.Error("a deterministic script is a non-producer by default")
	}
	script.CommitsRepo = true
	if !isRepoProducer(script) {
		t.Error("commitsRepo must opt a committing script into producer-ness")
	}
}

// TestCommittingScriptOptInFeedsCoverage: a commitsRepo stage becomes a
// definition that downstream consumers must cover — and it kills the
// upstream definition on its path.
func TestCommittingScriptOptInFeedsCoverage(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "format"},
			{Name: "format", Type: apiv1.TaskDeterministic, Goal: "format and commit",
				Run:         &apiv1.DeterministicRun{Script: "gofmt -w . && git commit -am fmt"},
				CommitsRepo: true,
				RepoFrom:    apiv1.RepoFrom{"implement"}, Next: "local-ci"},
			{Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "ci",
				Run:      &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
				RepoFrom: apiv1.RepoFrom{"format"}},
		},
	}
	def := Definition{Name: "committing-script", Version: 1, Spec: spec}
	if _, err := compileAcknowledged(def); err != nil {
		t.Fatalf("committing-script chain must compile: %v", err)
	}
	coverage := RepoFromCoverage(def)
	// format is the last producer before local-ci on the only path; implement
	// is killed by format's definition.
	if got, want := coverage["local-ci"], []string{"format"}; !slices.Equal(got, want) {
		t.Fatalf("local-ci coverage = %v, want %v (the opt-in producer kills the upstream definition)", got, want)
	}
}
