package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// This file is a fixture-driven dry run of the shipped work-nomination
// workflow (issue #26, config-examples/gaggles/acme-web/workflows/
// work-nomination.yaml): it loads the real definition and walks it through
// the compiled machine with fakes standing in for the not-yet-wired
// telemetry-materializing stage (#24's CLI, invoked via a deterministic
// stage per #18's dispatch shape) and the Copilot CLI harness (#19). It
// proves the DSL correctly sequences gather-signals -> nominate, that a
// fixture decision function files evidence-backed issues capped at
// maxNominationsPerRun and dedupes against already-nominated issues on a
// second run, and that a nominated item composes with backlog-curation's own
// decision logic (curation_test.go, same package). The actual gap-finding
// *judgment* is the nominator's instructions.md (LLM-driven), not asserted
// here — same boundary curation_test.go and implementation_test.go draw.

const nominationConfigRoot = "../../config-examples/gaggles/acme-web"

func loadNominationWorkflow(t *testing.T) apiv1.WorkflowSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(nominationConfigRoot, "workflows", "work-nomination.yaml"))
	if err != nil {
		t.Fatalf("read work-nomination.yaml: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return w.Spec
}

func nominationRunInput(spec apiv1.WorkflowSpec) RunInput {
	return RunInput{
		RunID:        "run-nomination",
		Gaggle:       "acme-web",
		WorkflowName: "work-nomination",
		Version:      1,
		Spec:         spec,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	}
}

// fixtureSignal is a candidate gap the fixture telemetry-signals artifact
// carries: a recurring error signature or a coverage-gap pointer, with an
// occurrence count driving priority (nominator instructions: strongest
// evidence first — recurring errors by count, then coverage gaps).
type fixtureSignal struct {
	kind        string // "error" | "coverage"
	description string
	occurrences int
}

func fixtureSignals() []fixtureSignal {
	return []fixtureSignal{
		{kind: "error", description: "provider.rate_limit in issues-provider claim path", occurrences: 5},
		{kind: "error", description: "harness.failure in copilot adapter timeout", occurrences: 3},
		{kind: "coverage", description: "internal/foo/bar.go:42 error branch untested", occurrences: 1},
		{kind: "coverage", description: "internal/baz/qux.go:10 nil-check untested", occurrences: 1},
	}
}

// nominatedIssue is what the nominator files for one gap: the fixture's
// stand-in for a real providers.CreateWorkItemRequest.
type nominatedIssue struct {
	title    string
	evidence string
	labels   []string
}

// nominateFixture applies the same evidence-first, capped, dedupe-first
// decision shape described in the nominator's instructions.md,
// deterministically, so the test is reproducible without an LLM in the loop.
// existing holds gap descriptions already covered by an open
// goobers:nominated issue (dedupe query, run first).
func nominateFixture(signals []fixtureSignal, existing map[string]bool, cap int) (filed []nominatedIssue, deduped int) {
	ordered := append([]fixtureSignal(nil), signals...)
	sort.SliceStable(ordered, func(i, j int) bool {
		iErr, jErr := ordered[i].kind == "error", ordered[j].kind == "error"
		if iErr != jErr {
			return iErr // errors sort before coverage gaps
		}
		return ordered[i].occurrences > ordered[j].occurrences
	})
	for _, s := range ordered {
		if existing[s.description] {
			deduped++
			continue
		}
		if len(filed) >= cap {
			continue
		}
		filed = append(filed, nominatedIssue{
			title:    "nominated: " + s.description,
			evidence: s.description,
			labels:   []string{"goobers:nominated"},
		})
	}
	return filed, deduped
}

// TestWorkNominationDryRun exercises the shipped definition end to end over a
// fixture telemetry-signals set: two recurring-error signatures and two
// coverage gaps, all within the default cap of 5 — issue #26's "seeded
// telemetry errors + a coverage hole ... issues filed with evidence" AC.
func TestWorkNominationDryRun(t *testing.T) {
	spec := loadNominationWorkflow(t)
	signals := fixtureSignals()

	det := &fakeRunner{
		run: func(_ context.Context, env apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Outputs: map[string]interface{}{"telemetry-signals": "artifact://telemetry-signals"},
				Summary: "materialized telemetry signals",
			}, nil
		},
	}

	var gotNominateGoal string
	var gotCap string
	inv := &fakeInvoker{
		invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			gotNominateGoal = env.Goal
			gotCap, _ = env.Inputs["maxNominationsPerRun"].(string)
			filed, deduped := nominateFixture(signals, nil, 5)
			issues := make([]interface{}, len(filed))
			for i, f := range filed {
				issues[i] = map[string]interface{}{"title": f.title, "evidence": f.evidence}
			}
			return apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess,
				Outputs: map[string]interface{}{"nomination-summary": map[string]interface{}{
					"filed":   len(filed),
					"deduped": deduped,
					"issues":  issues,
				}},
				Summary: "filed nominations",
			}, nil
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Det: det})

	env.ExecuteWorkflow(Run, nominationRunInput(spec))

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}

	// gather-signals ran first. (Its declared capability — telemetry:read
	// only — is checked statically against the spec in
	// TestWorkNominationNeverGrantsPushCapability; this superseded Temporal
	// engine doesn't flatten capabilities into the runtime envelope, so that
	// assertion belongs at the spec layer, not here.)
	signalsOut, ok := res.Outputs["gather-signals"]
	if !ok || signalsOut.Status != apiv1.ResultSuccess {
		t.Fatalf("gather-signals output missing or not success: %+v", signalsOut)
	}

	// nominate ran second (goober = nominator) and received the configured cap.
	if gotNominateGoal == "" {
		t.Error("nominate stage did not receive a goal")
	}
	if gotCap != "5" {
		t.Errorf("nominate stage maxNominationsPerRun input = %q, want 5", gotCap)
	}
	nominateOut, ok := res.Outputs["nominate"]
	if !ok || nominateOut.Status != apiv1.ResultSuccess {
		t.Fatalf("nominate output missing or not success: %+v", nominateOut)
	}
	summary, ok := nominateOut.Outputs["nomination-summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("nomination-summary missing or wrong shape: %#v", nominateOut.Outputs["nomination-summary"])
	}
	if got, ok := toInt(summary["filed"]); !ok || got != 4 {
		t.Errorf("nomination-summary[filed] = %v, want 4 (all signals within cap)", summary["filed"])
	}
	if got, ok := toInt(summary["deduped"]); !ok || got != 0 {
		t.Errorf("nomination-summary[deduped] = %v, want 0 (first run, nothing existing)", summary["deduped"])
	}
}

// TestWorkNominationDedupesOnSecondRun proves issue #26's "deduped on second
// run" AC: when every signal from the first run is already covered by an
// open goobers:nominated issue, a second run files nothing new.
func TestWorkNominationDedupesOnSecondRun(t *testing.T) {
	spec := loadNominationWorkflow(t)
	signals := fixtureSignals()
	existing := map[string]bool{}
	for _, s := range signals {
		existing[s.description] = true
	}

	det := &fakeRunner{
		run: func(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"telemetry-signals": "artifact://telemetry-signals"}}, nil
		},
	}
	inv := &fakeInvoker{
		invoke: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			filed, deduped := nominateFixture(signals, existing, 5)
			return apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess,
				Outputs: map[string]interface{}{"nomination-summary": map[string]interface{}{
					"filed":   len(filed),
					"deduped": deduped,
				}},
			}, nil
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Det: det})
	env.ExecuteWorkflow(Run, nominationRunInput(spec))

	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	summary := res.Outputs["nominate"].Outputs["nomination-summary"].(map[string]interface{})
	if got, ok := toInt(summary["filed"]); !ok || got != 0 {
		t.Errorf("second-run nomination-summary[filed] = %v, want 0 (all deduped)", summary["filed"])
	}
	if got, ok := toInt(summary["deduped"]); !ok || got != len(signals) {
		t.Errorf("second-run nomination-summary[deduped] = %v, want %d", summary["deduped"], len(signals))
	}
}

// TestWorkNominationCapsAtMaxPerRun proves the noise-control cap actually
// bounds filing even when more genuine candidates exist than the configured
// per-run maximum.
func TestWorkNominationCapsAtMaxPerRun(t *testing.T) {
	filed, deduped := nominateFixture(fixtureSignals(), nil, 2)
	if len(filed) != 2 {
		t.Fatalf("len(filed) = %d, want 2 (capped)", len(filed))
	}
	if deduped != 0 {
		t.Fatalf("deduped = %d, want 0 (cap, not dedupe, is why the rest were skipped)", deduped)
	}
	// Strongest evidence first: the two highest-occurrence error signatures.
	if filed[0].evidence != "provider.rate_limit in issues-provider claim path" ||
		filed[1].evidence != "harness.failure in copilot adapter timeout" {
		t.Errorf("cap did not prioritize strongest evidence first: %+v", filed)
	}
}

// TestWorkNominationNeverGrantsPushCapability is the fixture-layer form of
// issue #26's "no push credentials materialized" AC: the compiled machine's
// declared capabilities across every stage exclude repo:push and
// github:pr:write. Push credentials for a capability never declared are
// never materialized (SEC-042/SEC-045) — this asserts the declaration itself
// stays clean, which is what makes that non-injection guarantee apply here.
func TestWorkNominationNeverGrantsPushCapability(t *testing.T) {
	spec := loadNominationWorkflow(t)
	for _, task := range spec.Tasks {
		for _, cap := range task.Capabilities {
			if cap == "repo:push" || cap == "github:pr:write" {
				t.Errorf("stage %q declares %q — work-nomination must never push or open a PR (issue #26 scope)", task.Name, cap)
			}
		}
	}

	byName := map[string][]string{}
	for _, task := range spec.Tasks {
		byName[task.Name] = task.Capabilities
	}
	if got := byName["gather-signals"]; len(got) != 1 || got[0] != "telemetry:read" {
		t.Errorf("gather-signals capabilities = %v, want exactly [telemetry:read]", got)
	}
	wantNominate := map[string]bool{"repo:read": true, "telemetry:read": true, "github:issues:write": true}
	if got := byName["nominate"]; len(got) != len(wantNominate) {
		t.Errorf("nominate capabilities = %v, want exactly %v", got, wantNominate)
	} else {
		for _, c := range got {
			if !wantNominate[c] {
				t.Errorf("nominate declares unexpected capability %q", c)
			}
		}
	}
}

// TestNominatedIssueComposesWithCuration is the fixture-layer form of issue
// #26's composition AC: an issue this workflow files is exactly the shape
// backlog-curation's own decision logic (curateFixture, curation_test.go,
// same package) marks goobers:ready — fresh, not a duplicate, not
// oversized — once a maintainer applies goobers:approved and curation's
// query-backlog stage claims it (#25).
func TestNominatedIssueComposesWithCuration(t *testing.T) {
	filed, _ := nominateFixture(fixtureSignals()[:1], nil, 5)
	if len(filed) != 1 {
		t.Fatalf("len(filed) = %d, want 1", len(filed))
	}

	nominated := fixtureBacklogItem{id: "nom-1", title: filed[0].title, ageDays: 0}
	summary := curateFixture([]fixtureBacklogItem{nominated})
	if got, ok := toInt(summary["markedReady"]); !ok || got != 1 {
		t.Errorf("curation summary for nominated item = %+v, want markedReady=1", summary)
	}
	if got, ok := toInt(summary["deduped"]); !ok || got != 0 {
		t.Errorf("nominated item wrongly curated as a duplicate: %+v", summary)
	}
	if got, ok := toInt(summary["staleFlagged"]); !ok || got != 0 {
		t.Errorf("nominated item wrongly curated as stale: %+v", summary)
	}
	if got, ok := toInt(summary["split"]); !ok || got != 0 {
		t.Errorf("nominated item wrongly curated as oversized: %+v", summary)
	}
}
