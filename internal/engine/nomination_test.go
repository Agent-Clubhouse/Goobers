package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	localrunner "github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// This file is a fixture-driven dry run of the shipped work-nomination
// workflow. It walks the real definition through the local runner so
// gather-signals records a schema-valid candidate-findings artifact and
// nominate consumes the resulting journal pointer.

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

func loadNominator(t *testing.T) apiv1.GooberSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(nominationConfigRoot, "goobers", "nominator", "goober.yaml"))
	if err != nil {
		t.Fatalf("read nominator goober.yaml: %v", err)
	}
	var g apiv1.Goober
	if err := yaml.Unmarshal(raw, &g); err != nil {
		t.Fatalf("unmarshal nominator: %v", err)
	}
	return g.Spec
}

type candidateFindingsFixture struct {
	Schema   string           `json:"schema"`
	Window   string           `json:"window"`
	Since    string           `json:"since"`
	Findings []rollup.Finding `json:"findings"`
}

func fixtureSignals() []rollup.Finding {
	return []rollup.Finding{
		{
			Kind: rollup.FindingErrorSignature, Subject: "provider.rate_limit in issues-provider claim path",
			Metrics: map[string]float64{"occurrences": 5}, Threshold: 3,
			FlaggedRuns: []rollup.JournalPointer{{RunID: "run-provider-rate-limit"}},
		},
		{
			Kind: rollup.FindingErrorSignature, Subject: "harness.failure in copilot adapter timeout",
			Metrics: map[string]float64{"occurrences": 3}, Threshold: 3,
			FlaggedRuns: []rollup.JournalPointer{{RunID: "run-harness-timeout"}},
		},
		{
			Kind: rollup.FindingStageUnreached, Subject: "internal/foo/bar.go:42 error branch untested",
			Metrics: map[string]float64{"attempts": 0}, Threshold: 1, FlaggedRuns: []rollup.JournalPointer{},
		},
		{
			Kind: rollup.FindingStageUnreached, Subject: "internal/baz/qux.go:10 nil-check untested",
			Metrics: map[string]float64{"attempts": 0}, Threshold: 1, FlaggedRuns: []rollup.JournalPointer{},
		},
	}
}

func fixtureCandidateFindings(t *testing.T, validator *validate.Validator) []byte {
	t.Helper()
	data, err := json.Marshal(candidateFindingsFixture{
		Schema:   "goobers.dev/candidate-findings/v1",
		Window:   "24h0m0s",
		Since:    "2026-07-19T00:00:00Z",
		Findings: fixtureSignals(),
	})
	if err != nil {
		t.Fatalf("marshal candidate findings fixture: %v", err)
	}
	if err := validator.ValidateJSON(schemas.CandidateFindings, data); err != nil {
		t.Fatalf("candidate findings fixture does not match schema: %v", err)
	}
	return data
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
func nominateFixture(signals []rollup.Finding, existing map[string]bool, cap int, capabilities []string) (filed []nominatedIssue, deduped int) {
	autoApprove := false
	for _, granted := range capabilities {
		if granted == string(capability.GitHubIssuesApprove) {
			autoApprove = true
			break
		}
	}
	ordered := append([]rollup.Finding(nil), signals...)
	sort.SliceStable(ordered, func(i, j int) bool {
		iErr := ordered[i].Kind == rollup.FindingErrorSignature
		jErr := ordered[j].Kind == rollup.FindingErrorSignature
		if iErr != jErr {
			return iErr // errors sort before coverage gaps
		}
		return ordered[i].Metrics["occurrences"] > ordered[j].Metrics["occurrences"]
	})
	for _, s := range ordered {
		if existing[s.Subject] {
			deduped++
			continue
		}
		if len(filed) >= cap {
			continue
		}
		labels := []string{"goobers:nominated"}
		if autoApprove {
			labels = append(labels, "goobers:approved")
		}
		filed = append(filed, nominatedIssue{
			title:    "nominated: " + s.Subject,
			evidence: s.Subject,
			labels:   labels,
		})
	}
	return filed, deduped
}

type nominationConnector struct {
	rec       localrunner.ArtifactRecorder
	validator *validate.Validator
	artifact  []byte
}

func (c *nominationConnector) Run(_ context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	wantCommand := []string{
		"goobers", "telemetry-query", "--window", "24h",
		"--aggregate", "stage-failure-rate",
		"--aggregate", "error-signature",
		"--aggregate", "gate-noise",
		"--format", "candidate-findings",
	}
	if !reflect.DeepEqual(run.Command, wantCommand) {
		return apiv1.ResultEnvelope{}, fmt.Errorf("gather-signals command = %v, want %v", run.Command, wantCommand)
	}
	if got := env.Inputs["resultFile"]; got != "candidate-findings.json" {
		return apiv1.ResultEnvelope{}, fmt.Errorf("gather-signals resultFile = %#v, want candidate-findings.json", got)
	}
	if err := c.validator.ValidateJSON(schemas.CandidateFindings, c.artifact); err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("validate connector artifact: %w", err)
	}
	ref, err := c.rec.RecordArtifact(env.TaskID+"/candidate-findings.json", c.artifact)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "materialized candidate findings",
		Artifacts: []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "application/json",
		}},
	}, nil
}

type fixtureNominator struct {
	validator *validate.Validator
	runsDir   string
	existing  map[string]bool

	gotGoal    string
	gotCap     string
	gotFiled   int
	gotDeduped int
	summary    string
}

func (n *fixtureNominator) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	n.gotGoal = env.Goal
	n.gotCap, _ = env.Inputs["maxNominationsPerRun"].(string)
	cap, err := strconv.Atoi(n.gotCap)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("parse maxNominationsPerRun: %w", err)
	}

	var artifact *apiv1.ArtifactPointer
	for _, pointer := range env.ContextPointers {
		if pointer.Name == "gather-signals.artifact[0]" {
			artifact = pointer.Artifact
			break
		}
	}
	if artifact == nil {
		return apiv1.ResultEnvelope{}, errors.New("candidate-findings context pointer not received")
	}
	data, err := artifact.Resolve(filepath.Join(n.runsDir, env.RunID))
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("resolve candidate-findings artifact: %w", err)
	}
	if err := n.validator.ValidateJSON(schemas.CandidateFindings, data); err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("validate candidate-findings handoff: %w", err)
	}
	var candidates candidateFindingsFixture
	if err := json.Unmarshal(data, &candidates); err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("decode candidate-findings handoff: %w", err)
	}

	filed, deduped := nominateFixture(candidates.Findings, n.existing, cap, env.Capabilities)
	n.gotFiled, n.gotDeduped = len(filed), deduped
	n.summary = fmt.Sprintf(
		"found %d candidates; %d deduped; filed %d; 0 skipped at per-run cap",
		len(candidates.Findings), deduped, len(filed),
	)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: n.summary}, nil
}

func (n *fixtureNominator) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("fixture nominator does not review")
}

func nominationMachine(t *testing.T, spec apiv1.WorkflowSpec) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(
		workflow.Definition{Name: "work-nomination", Version: 1, Spec: spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile work-nomination: %v", err)
	}
	gather, ok := machine.Task("gather-signals")
	if !ok {
		t.Fatal("compiled workflow is missing gather-signals")
	}
	if len(gather.ExpectedOutputs) != 1 || gather.ExpectedOutputs[0] != "candidate-findings" {
		t.Fatalf("gather-signals expectedOutputs = %v, want [candidate-findings]", gather.ExpectedOutputs)
	}
	return machine
}

func runNominationFixture(t *testing.T, runID string, existing map[string]bool) *fixtureNominator {
	t.Helper()
	validator, err := validate.New()
	if err != nil {
		t.Fatalf("create schema validator: %v", err)
	}
	artifact := fixtureCandidateFindings(t, validator)
	instanceRoot := t.TempDir()
	runsDir := filepath.Join(instanceRoot, "runs")
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("create worktree manager: %v", err)
	}
	repo := nominationFixtureRepo(t)
	nominator := &fixtureNominator{validator: validator, runsDir: runsDir, existing: existing}
	r, err := localrunner.New(localrunner.Config{
		NewDeterministic: func(rec localrunner.ArtifactRecorder, _ localrunner.SecretRegistrar) (invoke.Deterministic, error) {
			return &nominationConnector{rec: rec, validator: validator, artifact: artifact}, nil
		},
		NewAgentic: func(name string, _ localrunner.ArtifactRecorder, _ localrunner.SecretRegistrar) (invoke.Goober, error) {
			if name != "nominator" {
				return nil, fmt.Errorf("unexpected goober %q", name)
			}
			return nominator, nil
		},
		Worktrees: manager,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return repo, nil
		},
	})
	if err != nil {
		t.Fatalf("create local runner: %v", err)
	}
	result, err := r.Start(context.Background(), localrunner.StartInput{
		RunID:   runID,
		Machine: nominationMachine(t, loadNominationWorkflow(t)),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("run work-nomination: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	return nominator
}

func nominationFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runNominationGit(t, "", "init", "--initial-branch=main", work)
	runNominationGit(t, work, "config", "user.email", "test@example.com")
	runNominationGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("write fixture repository: %v", err)
	}
	runNominationGit(t, work, "add", "README.md")
	runNominationGit(t, work, "commit", "-m", "initial")
	runNominationGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runNominationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

// TestWorkNominationDryRun exercises the shipped definition end to end over a
// schema-valid connector artifact: two recurring-error signatures and two
// coverage gaps, all within the default cap of 5.
func TestWorkNominationDryRun(t *testing.T) {
	nominator := runNominationFixture(t, "run-nomination", nil)
	if nominator.gotGoal == "" {
		t.Error("nominate stage did not receive a goal")
	}
	if nominator.gotCap != "5" {
		t.Errorf("nominate stage maxNominationsPerRun input = %q, want 5", nominator.gotCap)
	}
	if nominator.gotFiled != 4 {
		t.Errorf("filed = %d, want 4 (all signals within cap)", nominator.gotFiled)
	}
	if nominator.gotDeduped != 0 {
		t.Errorf("deduped = %d, want 0 (first run, nothing existing)", nominator.gotDeduped)
	}
	if nominator.summary != "found 4 candidates; 0 deduped; filed 4; 0 skipped at per-run cap" {
		t.Errorf("nominate summary = %q, want run counts", nominator.summary)
	}
}

// TestWorkNominationDedupesOnSecondRun proves issue #26's "deduped on second
// run" AC: when every signal from the first run is already covered by an
// open goobers:nominated issue, a second run files nothing new.
func TestWorkNominationDedupesOnSecondRun(t *testing.T) {
	signals := fixtureSignals()
	existing := map[string]bool{}
	for _, s := range signals {
		existing[s.Subject] = true
	}

	nominator := runNominationFixture(t, "run-nomination-second", existing)
	if nominator.gotFiled != 0 {
		t.Errorf("second-run filed = %d, want 0 (all deduped)", nominator.gotFiled)
	}
	if nominator.gotDeduped != len(signals) {
		t.Errorf("second-run deduped = %d, want %d", nominator.gotDeduped, len(signals))
	}
	if nominator.summary != "found 4 candidates; 4 deduped; filed 0; 0 skipped at per-run cap" {
		t.Errorf("second-run nominate summary = %q, want run counts", nominator.summary)
	}
}

// TestWorkNominationCapsAtMaxPerRun proves the noise-control cap actually
// bounds filing even when more genuine candidates exist than the configured
// per-run maximum.
func TestWorkNominationCapsAtMaxPerRun(t *testing.T) {
	filed, deduped := nominateFixture(fixtureSignals(), nil, 2, nil)
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

func TestWorkNominationApprovalCapabilityIsStageOptIn(t *testing.T) {
	spec := loadNominationWorkflow(t)
	nominator := loadNominator(t)

	nominatorAllowsApproval := false
	nominateTask := -1
	for _, granted := range nominator.Capabilities {
		if granted == string(capability.GitHubIssuesApprove) {
			nominatorAllowsApproval = true
		}
	}
	for i, task := range spec.Tasks {
		if task.Name != "nominate" {
			continue
		}
		nominateTask = i
		for _, declared := range task.Capabilities {
			if declared == string(capability.GitHubIssuesApprove) {
				t.Fatal("shipped nominate stage must leave self-approval disabled")
			}
		}
	}
	if !nominatorAllowsApproval {
		t.Fatal("nominator must allow an operator to opt the stage into github:issues:approve")
	}
	if nominateTask == -1 {
		t.Fatal("nominate task not found")
	}

	spec.Tasks[nominateTask].Capabilities = append(
		spec.Tasks[nominateTask].Capabilities,
		string(capability.GitHubIssuesApprove),
	)
	if _, err := workflow.Compile(
		workflow.Definition{Name: "work-nomination", Version: 1, Spec: spec},
		workflow.WithGoobers(map[string]apiv1.GooberSpec{"nominator": nominator}),
		workflow.WithPreviewFeatures(true),
	); err != nil {
		t.Fatalf("compile opted-in work-nomination: %v", err)
	}
}

// TestNominatedIssueComposesWithCuration is the fixture-layer form of issue
// #26's composition AC: an issue this workflow files carries only the
// nomination marker, leaving the maintainer trust decision and readiness
// marker to backlog curation.
func TestNominatedIssueComposesWithCuration(t *testing.T) {
	filed, _ := nominateFixture(fixtureSignals()[:1], nil, 5, nil)
	if len(filed) != 1 {
		t.Fatalf("len(filed) = %d, want 1", len(filed))
	}

	if got := filed[0].labels; len(got) != 1 || got[0] != "goobers:nominated" {
		t.Errorf("nominated issue labels = %v, want only goobers:nominated", got)
	}
}

func TestNominatedIssueAutoApprovesOnlyWithCapability(t *testing.T) {
	filed, _ := nominateFixture(
		fixtureSignals()[:1],
		nil,
		5,
		[]string{string(capability.GitHubIssuesApprove)},
	)
	if len(filed) != 1 {
		t.Fatalf("len(filed) = %d, want 1", len(filed))
	}
	want := []string{"goobers:nominated", "goobers:approved"}
	if !reflect.DeepEqual(filed[0].labels, want) {
		t.Errorf("nominated issue labels = %v, want %v", filed[0].labels, want)
	}
}
