package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func inventoryDefinitions() *instance.ConfigSet {
	return &instance.ConfigSet{
		Manifest: &apiv1.Manifest{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				workflow.PreviewFeaturesAnnotation: "true",
			}},
			Spec: apiv1.ManifestSpec{
				Instance: apiv1.InstanceRef{Name: "clubhouse", Environment: apiv1.EnvironmentDev},
				Gaggles:  []string{"beta", "alpha"},
			},
		},
		Gaggles: []apiv1.Gaggle{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "beta"},
				Spec: apiv1.GaggleSpec{
					Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "beta"},
					Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "example/beta"},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha"},
				Spec: apiv1.GaggleSpec{
					DisplayName: "Alpha Team",
					Project: apiv1.RepoRef{
						Provider:      apiv1.ProviderGitHub,
						Owner:         "example",
						Name:          "alpha",
						ConnectionRef: "alpha-write-token",
					},
					AdditionalRepos: []apiv1.RepoRef{
						{
							Provider:      apiv1.ProviderGitHub,
							Owner:         "example",
							Name:          "docs",
							ConnectionRef: "docs-read-token",
						},
						{
							Provider:      apiv1.ProviderADO,
							Owner:         "example",
							Project:       "platform",
							Name:          "shared",
							ConnectionRef: "shared-read-token",
						},
					},
					Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "example/alpha"},
				},
			},
		},
		Goobers: []apiv1.Goober{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "builder"},
				Spec: apiv1.GooberSpec{
					Gaggle:       "alpha",
					Role:         "implementer",
					Instructions: "builder.md",
					Skills:       []string{"testing", "coding"},
					Capabilities: []string{"repo:push"},
				},
			},
		},
		Workflows: []apiv1.Workflow{
			testInventoryWorkflow("beta", "deploy", "Beta Deploy", "", apiv1.TaskDeterministic),
			testInventoryWorkflow("alpha", "deploy", "Alpha Deploy", "builder", apiv1.TaskAgentic),
		},
	}
}

func testInventoryWorkflow(gaggle, name, displayName, goober string, kind apiv1.TaskType) apiv1.Workflow {
	task := apiv1.Task{
		Name:   "implement",
		Type:   kind,
		Goal:   "Implement the change",
		Goober: goober,
	}
	if kind == apiv1.TaskDeterministic {
		task.Run = &apiv1.DeterministicRun{Command: []string{"true"}}
	}
	return apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{"goobers.dev/purpose": "Ship changes"},
		},
		Spec: apiv1.WorkflowSpec{
			Gaggle:      gaggle,
			DisplayName: displayName,
			Triggers:    []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Readiness:   apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
			Start:       task.Name,
			Tasks:       []apiv1.Task{task},
		},
	}
}

func newInventoryService(t *testing.T, definitions *instance.ConfigSet, report *validate.Report) (*Local, instance.Layout) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Config:      &instance.Config{RunConditions: instance.RunConditions{MaxParallelRuns: 4}},
		Definitions: definitions,
		Validation:  report,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return service, layout
}

func createActiveRun(t *testing.T, layout instance.Layout, runID, gaggle, workflowName string) {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        workflowName,
		WorkflowVersion: currentWorkflowVersion,
		Gaggle:          gaggle,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceInventoryEmptyReadyAndWarningStates(t *testing.T) {
	empty, _ := newInventoryService(t, testDefinitions(), nil)
	got, err := empty.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ready || got.Status != InstanceStatusReady {
		t.Fatalf("empty instance readiness = %t/%q", got.Ready, got.Status)
	}
	if got.Counts != (InventoryCounts{}) {
		t.Fatalf("empty counts = %+v", got.Counts)
	}
	if got.Warnings == nil {
		t.Fatal("empty warnings must encode as an empty array")
	}

	report := &validate.Report{Issues: []validate.Issue{{
		Code:     validate.WarningPreviewFeature,
		Severity: validate.Warning,
		File:     "workflows/deploy.yaml",
		Kind:     "Workflow",
		Name:     "deploy",
		Message:  "preview feature in use",
	}}}
	degraded, _ := newInventoryService(t, inventoryDefinitions(), report)
	got, err = degraded.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != InstanceStatusDegraded || len(got.Warnings) != 1 {
		t.Fatalf("degraded instance = %+v", got)
	}
	warning := got.Warnings[0]
	if warning.Code != validate.WarningPreviewFeature || warning.Severity != validate.Warning ||
		warning.Scope != "workflows/deploy.yaml Workflow/deploy" || warning.Explanation != "preview feature in use" {
		t.Fatalf("warning = %+v", warning)
	}

	starting, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: inventoryDefinitions(),
		Validation:  report,
	}, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	got, err = starting.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Ready || got.Status != InstanceStatusStarting || len(got.Warnings) != 1 {
		t.Fatalf("starting instance with warnings = %+v", got)
	}
}

func TestInventoryProjectsScopedWorkflowIdentityGraphOwnershipAndActiveCounts(t *testing.T) {
	service, layout := newInventoryService(t, inventoryDefinitions(), nil)
	service.sources.Config.RunConditions.WorkflowBudgets = map[string]int{"deploy": 3}
	service.sources.Config.RunConditions.WorkflowDailyBudgets = map[string]int{"deploy": 4}
	createActiveRun(t, layout, strings.Repeat("1", 32), "alpha", "deploy")
	createActiveRun(t, layout, strings.Repeat("2", 32), "beta", "deploy")
	createActiveRun(t, layout, strings.Repeat("3", 32), "alpha", "removed-workflow")

	instanceView, err := service.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instanceView.Counts.ActiveRuns != 3 || instanceView.Concurrency.ActiveRuns != 3 ||
		instanceView.Concurrency.MaxConcurrentRuns != 4 {
		t.Fatalf("instance concurrency = %+v, counts = %+v", instanceView.Concurrency, instanceView.Counts)
	}

	gaggles, err := service.Gaggles(context.Background(), PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gaggles.Items) != 2 || gaggles.Items[0].Name != "alpha" || gaggles.Items[1].Name != "beta" {
		t.Fatalf("gaggles = %+v", gaggles.Items)
	}
	if gaggles.Items[0].ActiveRunCount != 2 || gaggles.Items[1].ActiveRunCount != 1 {
		t.Fatalf("scoped active counts = %d/%d", gaggles.Items[0].ActiveRunCount, gaggles.Items[1].ActiveRunCount)
	}

	goobers, err := service.Goobers(context.Background(), "alpha", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(goobers.Items) != 1 {
		t.Fatalf("goobers = %+v", goobers.Items)
	}
	goober := goobers.Items[0]
	if goober.Status != "configured" || len(goober.Workflows) != 1 || len(goober.Stages) != 1 {
		t.Fatalf("goober projection = %+v", goober)
	}
	if goober.Workflows[0] != (WorkflowReference{Gaggle: "alpha", Name: "deploy"}) ||
		goober.Stages[0].Stage != "implement" {
		t.Fatalf("goober ownership = %+v / %+v", goober.Workflows, goober.Stages)
	}

	alpha, err := service.Workflow(context.Background(), "alpha", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := service.Workflow(context.Background(), "beta", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Identity.Gaggle != "alpha" || beta.Identity.Gaggle != "beta" {
		t.Fatalf("workflow identities = %+v / %+v", alpha.Identity, beta.Identity)
	}
	if alpha.Definition.Version != currentWorkflowVersion || alpha.Definition.Digest == "" ||
		alpha.Graph.Digest != alpha.Definition.Digest {
		t.Fatalf("alpha definition/graph = %+v / %+v", alpha.Definition, alpha.Graph)
	}
	if alpha.Definition.Digest == beta.Definition.Digest {
		t.Fatal("scoped definitions with different specs must not share a digest")
	}
	if alpha.Concurrency.ActiveRuns != 1 || beta.Concurrency.ActiveRuns != 1 {
		t.Fatalf("workflow active counts = %d/%d", alpha.Concurrency.ActiveRuns, beta.Concurrency.ActiveRuns)
	}
	if alpha.Readiness.MaxRunsPerHour != 3 || alpha.Readiness.MaxRunsPerDay != 4 {
		t.Fatalf("effective workflow budgets = %+v", alpha.Readiness)
	}
	if len(alpha.Owners) != 1 || alpha.Owners[0].Name != "builder" ||
		len(alpha.Graph.Nodes) != 1 || alpha.Graph.Nodes[0].Kind != workflow.GraphNodeAgentic {
		t.Fatalf("alpha graph/owners = %+v / %+v", alpha.Graph, alpha.Owners)
	}

	connections, err := service.Connections(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	wantConnections := []RepositoryConnection{
		{
			Repository: RepositoryIdentity{
				Provider: apiv1.ProviderGitHub,
				Owner:    "example",
				Name:     "alpha",
			},
			AccessMode: RepositoryAccessReadWrite,
		},
		{
			Repository: RepositoryIdentity{
				Provider: apiv1.ProviderGitHub,
				Owner:    "example",
				Name:     "docs",
			},
			AccessMode: RepositoryAccessReadOnly,
		},
		{
			Repository: RepositoryIdentity{
				Provider: apiv1.ProviderADO,
				Owner:    "example",
				Project:  "platform",
				Name:     "shared",
			},
			AccessMode: RepositoryAccessReadOnly,
		},
	}
	if connections.Gaggle != "alpha" || !slices.Equal(connections.Repositories, wantConnections) {
		t.Fatalf("connections = %+v, want %+v", connections, wantConnections)
	}
	encoded, err := json.Marshal(connections)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "connectionRef") ||
		strings.Contains(string(encoded), "alpha-write-token") ||
		strings.Contains(string(encoded), "docs-read-token") ||
		strings.Contains(string(encoded), "shared-read-token") {
		t.Fatalf("connections response exposes credential routing references: %s", encoded)
	}
}

func TestWorkflowInventoryProjectsDesiredAndBlockedOccupancy(t *testing.T) {
	definitions := inventoryDefinitions()
	for i := range definitions.Workflows {
		if definitions.Workflows[i].Spec.Gaggle == "alpha" && definitions.Workflows[i].Name == "deploy" {
			definitions.Workflows[i].Spec.Readiness.DesiredConcurrentRuns = 2
		}
	}
	service, layout := newInventoryService(t, definitions, nil)
	createActiveRun(t, layout, strings.Repeat("4", 32), "alpha", "deploy")
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{
		Type:     journal.EventTickSkipped,
		Gaggle:   "alpha",
		Workflow: "deploy",
		Reason:   "refill blocked: " + localscheduler.ReasonBudget,
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	workflow, err := service.Workflow(context.Background(), "alpha", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Concurrency.ActiveRuns != 1 ||
		workflow.Concurrency.DesiredRuns != 2 ||
		workflow.Concurrency.MaxConcurrentRuns != 2 ||
		!workflow.Concurrency.AdmissionBlocked ||
		workflow.Concurrency.BlockingCondition != localscheduler.ReasonBudget {
		t.Fatalf("workflow concurrency = %+v", workflow.Concurrency)
	}
}

func TestInventoryRejectsCrossGaggleWorkflowOwner(t *testing.T) {
	definitions := inventoryDefinitions()
	definitions.Goobers[0].Spec.Gaggle = "beta"
	_, err := NewLocal(LocalSources{
		Layout:      instance.NewLayout(t.TempDir()),
		Definitions: definitions,
	}, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), `goober "builder" in gaggle "beta"`) {
		t.Fatalf("cross-gaggle owner error = %v", err)
	}
}

// TestWorkflowStagesIncludeParallels is the regression test for #2193: the
// portal's workflow-detail page threw "inconsistent workflow stages" for any
// workflow using the parallels DSL because workflowStages() only walked
// Spec.Tasks/Spec.Gates while the compiled graph also has a node per
// Spec.Parallels entry. Exercised through the real service.Workflow() path
// (not workflowStages() in isolation) so it catches drift between the two
// independently-updated code paths the portal's consistency check spans.
func TestWorkflowStagesIncludeParallels(t *testing.T) {
	definitions := inventoryDefinitions()
	definitions.Workflows = append(definitions.Workflows, apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "quality-sprint",
			Annotations: map[string]string{"goobers.dev/purpose": "Fan out review lenses"},
		},
		DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:      "alpha",
			DisplayName: "Quality Sprint",
			Triggers:    []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Readiness:   apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Start:       "churn",
			Tasks: []apiv1.Task{
				{Name: "churn", Type: apiv1.TaskDeterministic, Goal: "churn report",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: "fan"},
				{Name: "review-security", Type: apiv1.TaskDeterministic, Goal: "security lens",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: workflow.TargetJoin},
				{Name: "review-perf", Type: apiv1.TaskDeterministic, Goal: "perf lens",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: workflow.TargetJoin},
				{Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate findings",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: workflow.TerminalComplete},
			},
			Parallels: []apiv1.Parallel{{
				Name:          "fan",
				FailurePolicy: apiv1.BranchContinueOnError,
				Join:          "collate",
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "perf", Start: "review-perf"},
				},
			}},
		},
	})

	service, _ := newInventoryService(t, definitions, nil)
	detail, err := service.Workflow(context.Background(), "alpha", "quality-sprint")
	if err != nil {
		t.Fatal(err)
	}

	if len(detail.Stages) != len(detail.Graph.Nodes) || detail.StageCount != len(detail.Graph.Nodes) {
		t.Fatalf("stages/graph node count mismatch: stages=%d stageCount=%d graph.Nodes=%d",
			len(detail.Stages), detail.StageCount, len(detail.Graph.Nodes))
	}

	byName := make(map[string]StageDefinition, len(detail.Stages))
	for _, stage := range detail.Stages {
		byName[stage.Name] = stage
	}
	for _, node := range detail.Graph.Nodes {
		stage, ok := byName[node.ID]
		if !ok || stage.Kind != node.Kind {
			t.Fatalf("graph node %q kind %q has no matching stage (stage=%+v)", node.ID, node.Kind, stage)
		}
	}
	if fan, ok := byName["fan"]; !ok || fan.Kind != workflow.GraphNodeParallel {
		t.Fatalf("expected a %q stage for the parallel block, got %+v", workflow.GraphNodeParallel, byName["fan"])
	}
}

func TestObjectWarningsRequireExactScope(t *testing.T) {
	report := &validate.Report{Issues: []validate.Issue{
		{
			Code:     validate.WarningModelFallback,
			Severity: validate.Warning,
			Kind:     "Goober",
			Name:     "builder",
			Message:  "configured model is unavailable",
		},
		{
			Code:     validate.WarningPreviewFeature,
			Severity: validate.Warning,
			Kind:     "Goober",
			Name:     "builder-preview",
			Message:  "preview feature in use",
		},
	}}
	service, _ := newInventoryService(t, inventoryDefinitions(), report)

	goobers, err := service.Goobers(context.Background(), "alpha", PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(goobers.Items) != 1 || len(goobers.Items[0].Warnings) != 1 ||
		goobers.Items[0].Warnings[0].Code != validate.WarningModelFallback {
		t.Fatalf("goober warnings = %+v", goobers.Items)
	}
}

func TestInventoryPaginationIsDeterministicAndCursorsAreScoped(t *testing.T) {
	definitions := testDefinitions()
	definitions.Manifest.Spec.Gaggles = []string{"charlie", "alpha", "bravo"}
	definitions.Gaggles = []apiv1.Gaggle{
		{ObjectMeta: metav1.ObjectMeta{Name: "charlie"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "bravo"}},
	}
	service, _ := newInventoryService(t, definitions, nil)

	first, err := service.Gaggles(context.Background(), PageRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Items[0].Name != "alpha" || first.Items[1].Name != "bravo" ||
		!first.Page.HasMore || first.Page.NextCursor == "" || first.Page.Total != 3 {
		t.Fatalf("first page = %+v", first)
	}
	second, err := service.Gaggles(context.Background(), PageRequest{Limit: 2, Cursor: first.Page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Name != "charlie" || second.Page.HasMore {
		t.Fatalf("second page = %+v", second)
	}
	if _, err := service.Gaggles(context.Background(), PageRequest{Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("malformed cursor error = %v", err)
	}
	if _, err := service.Goobers(context.Background(), "alpha", PageRequest{Cursor: first.Page.NextCursor}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-list cursor error = %v", err)
	}
}

func TestInventoryMissingIdentities(t *testing.T) {
	service, _ := newInventoryService(t, inventoryDefinitions(), nil)
	if _, err := service.Goobers(context.Background(), "missing", PageRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing gaggle error = %v", err)
	}
	if _, err := service.Workflow(context.Background(), "alpha", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing workflow error = %v", err)
	}
	if _, err := service.Connections(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing connections error = %v", err)
	}
}
