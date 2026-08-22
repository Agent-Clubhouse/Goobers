package v30

// repofrom_parallel_test.go pins WF022's parallel fan-out/fan-in dataflow:
// Machine.Outgoing returns a branch-terminal stage's RAW target ("@join"), and
// the reaching-definitions fixed-point must resolve it to the owning
// parallel's join state exactly as buildGraph does — otherwise a producer
// inside a branch (update-behind-pr on a scratch workspace advances the ref
// provider-side; legal per parallel rule 9, a producer per the §4 commit
// reading) never reaches the post-join consumer, the author's CORRECT
// declaration is refused as a WF022 dead entry, and the under-declared
// spelling compiles clean — the exact silent-loss shape WF022 exists to make
// inexpressible. The failure lane shares the gap: a branch failure hands the
// run to onFailure at any point, so an already-ran branch producer can be the
// last producer the failure-lane consumer sees.

import (
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
)

// branchProducerDef is the probe shape from the finding:
//
//	implement (repo producer) -> fan{advance: update-pr (scratch,
//	provider-side ref advance) | perf: review-perf (scratch)} -> collate
//	(repo consumer at the join)
//
// collate's reaching-last-producer set is {implement, update-pr}: update-pr
// through the advance branch's fan-in edge, implement through the perf branch
// (no producer kills it there).
func branchProducerDef() Definition {
	return Definition{
		Name:       "branch-producer",
		Version:    1,
		DSLVersion: DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    "implement",
			Tasks: []apiv1.Task{
				{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "fan"},
				{Name: "update-pr", Type: apiv1.TaskDeterministic, Goal: "advance the ref provider-side",
					Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "update-behind-pr"}, Workspace: apiv1.WorkspaceScratch},
					Capabilities:  []string{string(capability.GitHubPRWrite), string(capability.GitHubIssuesWrite)},
					PolicyActions: []string{"update-pr-branch", "clear-remediation"},
					Next:          TargetJoin},
				{Name: "review-perf", Type: apiv1.TaskDeterministic, Goal: "perf lens",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: TargetJoin},
				{Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate on the run branch",
					Run:      &apiv1.DeterministicRun{Command: []string{"make", "report"}},
					RepoFrom: apiv1.RepoFrom{"implement", "update-pr"},
					Next:     TerminalComplete},
			},
			Parallels: []apiv1.Parallel{{
				Name:          "fan",
				FailurePolicy: apiv1.BranchContinueOnError,
				Join:          "collate",
				Branches: []apiv1.Branch{
					{Name: "advance", Start: "update-pr"},
					{Name: "perf", Start: "review-perf"},
				},
			}},
		},
	}
}

// branchProducerOnFailureDef is the failure-lane analog: fail_fast with an
// onFailure route that lands on a repo consumer. When a branch fails, the
// advance branch's producer may already have advanced the ref, so cleanup
// must cover it too.
func branchProducerOnFailureDef() Definition {
	def := branchProducerDef()
	def.Name = "branch-producer-onfailure"
	def.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
	def.Spec.Parallels[0].OnFailure = "cleanup"
	def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
		Name: "cleanup", Type: apiv1.TaskDeterministic, Goal: "clean up on the run branch",
		Run:      &apiv1.DeterministicRun{Command: []string{"make", "clean"}},
		RepoFrom: apiv1.RepoFrom{"implement", "update-pr"},
		Next:     TargetEscalate,
	})
	return def
}

// TestParallelBranchProducerReachesPostJoinConsumer: the author's correct
// two-element declaration at the join consumer compiles — refusing it as a
// dead entry is the fan-in dataflow gap.
func TestParallelBranchProducerReachesPostJoinConsumer(t *testing.T) {
	if _, err := compileAcknowledged(branchProducerDef()); err != nil {
		t.Fatalf("correct declaration [implement, update-pr] at the join consumer must compile: %v", err)
	}
}

// TestParallelBranchProducerUnderDeclarationFailsWF022: the under-declared
// [implement] spelling silently discards the branch producer's ref advance on
// the fan-in path and must be refused, naming the uncovered producer.
func TestParallelBranchProducerUnderDeclarationFailsWF022(t *testing.T) {
	def := branchProducerDef()
	for i := range def.Spec.Tasks {
		if def.Spec.Tasks[i].Name == "collate" {
			def.Spec.Tasks[i].RepoFrom = apiv1.RepoFrom{"implement"}
		}
	}
	_, err := compileAcknowledged(def)
	if err == nil ||
		!strings.Contains(err.Error(), `task "collate" repoFrom "implement" does not cover producer "update-pr"`) {
		t.Fatalf("Compile error = %v, want WF022 uncovered-producer rejection naming update-pr", err)
	}
}

// TestParallelBranchProducerRepoFromCoverage: the #3516 migrator seam must
// compute the same full set — it shares reachingProducers with the checker.
func TestParallelBranchProducerRepoFromCoverage(t *testing.T) {
	coverage := RepoFromCoverage(branchProducerDef())
	if coverage == nil {
		t.Fatal("RepoFromCoverage returned nil for a structurally valid definition")
	}
	if got, want := coverage["collate"], []string{"implement", "update-pr"}; !slices.Equal(got, want) {
		t.Fatalf("collate coverage = %v, want %v (the branch producer must cross the fan-in edge)", got, want)
	}
}

// TestParallelOnFailureLaneCoversBranchProducer: the failure-lane analog in
// both directions — the correct declaration compiles, the under-declared one
// is refused — plus the migrator-seam set.
func TestParallelOnFailureLaneCoversBranchProducer(t *testing.T) {
	if _, err := compileAcknowledged(branchProducerOnFailureDef()); err != nil {
		t.Fatalf("correct declaration [implement, update-pr] at the failure-lane consumer must compile: %v", err)
	}

	coverage := RepoFromCoverage(branchProducerOnFailureDef())
	if coverage == nil {
		t.Fatal("RepoFromCoverage returned nil for a structurally valid definition")
	}
	if got, want := coverage["cleanup"], []string{"implement", "update-pr"}; !slices.Equal(got, want) {
		t.Fatalf("cleanup coverage = %v, want %v (a branch failure can hand off after the sibling producer ran)", got, want)
	}

	def := branchProducerOnFailureDef()
	for i := range def.Spec.Tasks {
		if def.Spec.Tasks[i].Name == "cleanup" {
			def.Spec.Tasks[i].RepoFrom = apiv1.RepoFrom{"implement"}
		}
	}
	_, err := compileAcknowledged(def)
	if err == nil ||
		!strings.Contains(err.Error(), `task "cleanup" repoFrom "implement" does not cover producer "update-pr"`) {
		t.Fatalf("Compile error = %v, want WF022 uncovered-producer rejection naming update-pr at the failure-lane consumer", err)
	}
}
