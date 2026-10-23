package workflow

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func fanOutReviewTask(name string) apiv1.Task {
	return apiv1.Task{Name: name, Type: apiv1.TaskAgentic, Goober: "reviewer", Goal: "Review one lens.", Workspace: apiv1.WorkspaceRepoReadOnly, Next: TargetJoin}
}

// selectiveParallelDefinition is the design §5.1 example.
func selectiveParallelDefinition() Definition {
	return Definition{Name: "selective", DSLVersion: supportmatrix.V31DSLVersion, Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "triage",
		Tasks: []apiv1.Task{
			{
				Name: "triage", Type: apiv1.TaskDeterministic, Goal: "Decide which review lenses apply.",
				Run:           &apiv1.DeterministicRun{Command: []string{"lens-selector", "--json"}, Workspace: apiv1.WorkspaceRepoReadOnly},
				ArtifactSlots: []apiv1.ArtifactSlot{{Name: "lenses", MediaType: "application/json"}},
				Next:          "review",
			},
			fanOutReviewTask("review-baseline"),
			fanOutReviewTask("review-security"),
			fanOutReviewTask("review-migration"),
			fanOutReviewTask("review-ui"),
			{Name: "collate", Type: apiv1.TaskAgentic, Goober: "lead", Goal: "Collate.", Workspace: apiv1.WorkspaceScratch},
		},
		Parallels: []apiv1.Parallel{{
			Name: "review", FailurePolicy: apiv1.BranchContinueOnError, BranchTimeoutSeconds: 1800, MaxConcurrentBranches: 3, Join: "collate",
			Selection: &apiv1.ParallelSelection{Source: apiv1.FanOutSource{Task: "triage", Slot: "lenses"}},
			Branches: []apiv1.Branch{
				{Name: "baseline", Start: "review-baseline"},
				{Name: "security", Start: "review-security", Optional: true},
				{Name: "migration", Start: "review-migration", Optional: true},
				{Name: "ui", Start: "review-ui", Optional: true},
			},
		}},
	}}
}

// mapDefinition is the design §6.1 example.
func mapDefinition() Definition {
	return Definition{Name: "mapped", DSLVersion: supportmatrix.V31DSLVersion, Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "plan",
		Tasks: []apiv1.Task{
			{
				Name: "plan", Type: apiv1.TaskAgentic, Goober: "planner", Goal: "Select the shard roster.", Workspace: apiv1.WorkspaceRepoReadOnly,
				ArtifactSlots: []apiv1.ArtifactSlot{{Name: "shards", MediaType: "application/json"}},
				Next:          "run-shards",
			},
			{
				Name: "run-shard", Type: apiv1.TaskDeterministic, Goal: "Run one shard.",
				Run:  &apiv1.DeterministicRun{Command: []string{"shard-runner"}, Workspace: apiv1.WorkspaceRepoReadOnly},
				Next: TargetJoin,
			},
			{Name: "collate", Type: apiv1.TaskAgentic, Goober: "lead", Goal: "Summarize.", Workspace: apiv1.WorkspaceScratch},
		},
		Maps: []apiv1.Map{{
			Name: "run-shards", Source: apiv1.FanOutSource{Task: "plan", Slot: "shards"}, Task: "run-shard",
			Bindings:      map[string]apiv1.MapBinding{"shard": apiv1.MapBindingItem, "shardIdentity": apiv1.MapBindingIdentity},
			FailurePolicy: apiv1.BranchContinueOnError, ItemTimeoutSeconds: 1800, MaxConcurrent: 4, Join: "collate",
		}},
	}}
}

func TestFanOutDesignExamplesCompile(t *testing.T) {
	for name, def := range map[string]Definition{"selective parallel": selectiveParallelDefinition(), "map": mapDefinition()} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile(def, WithPreviewFeatures(true)); err != nil {
				t.Fatalf("Compile: %v", err)
			}
			_, err := Compile(def, WithPreviewFeatures(false))
			if err == nil || !strings.Contains(err.Error(), "is preview and requires explicit") {
				t.Fatalf("Compile without preview acknowledgement = %v, want preview refusal", err)
			}
			for _, version := range []string{supportmatrix.V3DSLVersion, supportmatrix.V2DSLVersion} {
				older := def
				older.DSLVersion = version
				_, err := Compile(older, WithPreviewFeatures(true))
				if err == nil || !strings.Contains(err.Error(), `requires dslVersion "3.1"`) {
					t.Fatalf("Compile at DSL %s = %v, want a 3.1 version refusal", version, err)
				}
			}
		})
	}
}

func TestSelectiveParallelCompilerRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Definition)
		want   string
	}{
		{"selection without optional branch", func(d *Definition) {
			for i := range d.Spec.Parallels[0].Branches {
				d.Spec.Parallels[0].Branches[i].Optional = false
			}
		}, `parallel "review" declares selection but no optional branch`},
		{"optional branch without selection", func(d *Definition) { d.Spec.Parallels[0].Selection = nil },
			`parallel "review" declares 3 optional branch(es) but no selection`},
		{"missing source task", func(d *Definition) { d.Spec.Parallels[0].Selection.Source.Task = "nope" },
			`parallel "review" selection source task "nope" is not a defined task`},
		{"source is not a task", func(d *Definition) { d.Spec.Parallels[0].Selection.Source.Task = "review" },
			`source task "review" is a parallel; a fan-out source must be a task`},
		{"undeclared slot", func(d *Definition) { d.Spec.Parallels[0].Selection.Source.Slot = "other" },
			`source slot "other" is not an artifactSlots entry of task "triage"`},
		{"non-JSON slot", func(d *Definition) { d.Spec.Tasks[0].ArtifactSlots[0].MediaType = "text/plain" },
			`slot "lenses" of task "triage" must declare mediaType "application/json"`},
		{"slot maxSize above host bound", func(d *Definition) { d.Spec.Tasks[0].ArtifactSlots[0].MaxSize = MaxFanOutSourceBytes + 1 },
			`declares maxSize 1048577, above the host bound of 1048576 bytes`},
		{"source does not dominate", func(d *Definition) {
			d.Spec.Start = "pre"
			d.Spec.Tasks = append(d.Spec.Tasks, apiv1.Task{Name: "pre", Type: apiv1.TaskDeterministic, Goal: "route",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "gate"})
			d.Spec.Gates = []apiv1.Gate{{Name: "gate", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "ci-green"},
				Branches: map[string]string{"pass": "triage", "fail": "review"}}}
		}, `source task "triage" does not run on every path before "review"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := selectiveParallelDefinition()
			tc.mutate(&def)
			assertCompileProblem(t, def, tc.want)
		})
	}
}

func TestMapCompilerRules(t *testing.T) {
	addTask := func(d *Definition, task apiv1.Task) { d.Spec.Tasks = append(d.Spec.Tasks, task) }
	for _, tc := range []struct {
		name   string
		mutate func(*Definition)
		want   string
	}{
		{"name collides with a task", func(d *Definition) { d.Spec.Maps[0].Name = "collate"; d.Spec.Tasks[0].Next = "collate" },
			`duplicate state "collate"`},
		{"name collides with another map", func(d *Definition) {
			second := d.Spec.Maps[0]
			second.Task = "other-shard"
			d.Spec.Maps = append(d.Spec.Maps, second)
		}, `duplicate state "run-shards"`},
		{"missing source task", func(d *Definition) { d.Spec.Maps[0].Source.Task = "nope" },
			`map "run-shards" source task "nope" is not a defined task`},
		{"undeclared source slot", func(d *Definition) { d.Spec.Maps[0].Source.Slot = "missing" },
			`source slot "missing" is not an artifactSlots entry of task "plan"`},
		{"non-JSON source slot", func(d *Definition) { d.Spec.Tasks[0].ArtifactSlots[0].MediaType = "" },
			`slot "shards" of task "plan" must declare mediaType "application/json"`},
		{"source slot maxSize above host bound", func(d *Definition) { d.Spec.Tasks[0].ArtifactSlots[0].MaxSize = 2 << 20 },
			`above the host bound of 1048576 bytes`},
		{"source is the mapped task", func(d *Definition) {
			d.Spec.Tasks[1].ArtifactSlots = []apiv1.ArtifactSlot{{Name: "shards", MediaType: "application/json"}}
			d.Spec.Maps[0].Source.Task = "run-shard"
		}, `source task "run-shard" does not run on every path before "run-shards"`},
		{"missing mapped task", func(d *Definition) { d.Spec.Maps[0].Task = "nope" }, `map "run-shards" task "nope" is not a defined task`},
		{"mapped task is a parallel", func(d *Definition) { d.Spec.Maps[0].Task = "run-shards" },
			`task "run-shards" is a parallel or map; a map runs one task and nested fan-out is not supported`},
		{"mapped task reachable outside the map", func(d *Definition) {
			d.Spec.Tasks[0].Next = "run-shard"
			d.Spec.Start = "plan"
			addTask(d, apiv1.Task{Name: "entry", Type: apiv1.TaskDeterministic, Goal: "enter",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "run-shards"})
		}, `map "run-shards" task "run-shard" is entered by "plan"; only its map may enter a mapped task`},
		{"mapped task owned by two maps", func(d *Definition) {
			second := d.Spec.Maps[0]
			second.Name, second.Join = "again", "collate-again"
			d.Spec.Tasks[2].Next = "again"
			d.Spec.Maps = append(d.Spec.Maps, second)
			addTask(d, apiv1.Task{Name: "collate-again", Type: apiv1.TaskAgentic, Goober: "lead", Goal: "again", Workspace: apiv1.WorkspaceScratch})
		}, `map "again" task "run-shard" is already owned by map "run-shards"`},
		{"mapped task owned by a parallel branch", func(d *Definition) {
			d.Spec.Tasks[2].Next = "fan"
			addTask(d, apiv1.Task{Name: "lens", Type: apiv1.TaskDeterministic, Goal: "lens",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: TargetJoin})
			addTask(d, apiv1.Task{Name: "end", Type: apiv1.TaskDeterministic, Goal: "end",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}})
			d.Spec.Parallels = []apiv1.Parallel{{Name: "fan", FailurePolicy: apiv1.BranchContinueOnError, Join: "end", MaxConcurrentBranches: 1,
				Branches: []apiv1.Branch{{Name: "a", Start: "run-shard"}, {Name: "b", Start: "lens"}}}}
		}, `map "run-shards" task "run-shard" is also reachable from parallel "fan" branch "a"`},
		{"mapped task exits to a state", func(d *Definition) { d.Spec.Tasks[1].Next = "collate" },
			`map "run-shards" task "run-shard" routes to "collate"; a mapped task must end at "@join", @abort, or @escalate`},
		{"mapped task completes the run", func(d *Definition) { d.Spec.Tasks[1].Next = "" },
			`routes to "<complete>"; a mapped task must end at "@join"`},
		{"mapped task uses writable repo", func(d *Definition) { d.Spec.Tasks[1].Run.Workspace = apiv1.WorkspaceRepo },
			`map "run-shards" task "run-shard" resolves to a writable repo workspace`},
		{"mapped task defaults to writable repo", func(d *Definition) { d.Spec.Tasks[1].Run.Workspace = "" },
			`resolves to a writable repo workspace`},
		{"mapped task enables child workflows", func(d *Definition) {
			d.Spec.Tasks[1] = apiv1.Task{Name: "run-shard", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "shard", Workspace: apiv1.WorkspaceScratch,
				Next: TargetJoin, ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}
		}, `map "run-shards" task "run-shard" declares childWorkflows; a mapped task may not enable child tools`},
		{"missing failure policy", func(d *Definition) { d.Spec.Maps[0].FailurePolicy = "" }, `map "run-shards" has no failurePolicy`},
		{"fail_fast without onFailure", func(d *Definition) { d.Spec.Maps[0].FailurePolicy = apiv1.BranchFailFast },
			`map "run-shards" failurePolicy "fail_fast" requires onFailure`},
		{"continue_on_error with onFailure", func(d *Definition) { d.Spec.Maps[0].OnFailure = TargetAbort },
			`map "run-shards" failurePolicy "continue_on_error" must not declare onFailure`},
		{"onFailure is @join", func(d *Definition) {
			d.Spec.Maps[0].FailurePolicy, d.Spec.Maps[0].OnFailure = apiv1.BranchAllOrNothing, TargetJoin
		}, `map "run-shards" onFailure must not be "@join"`},
		{"undefined onFailure", func(d *Definition) {
			d.Spec.Maps[0].FailurePolicy, d.Spec.Maps[0].OnFailure = apiv1.BranchAllOrNothing, "nowhere"
		}, `map "run-shards" onFailure "nowhere" is not a defined state`},
		{"missing join", func(d *Definition) { d.Spec.Maps[0].Join = "" }, `map "run-shards" has no join state`},
		{"undefined join", func(d *Definition) { d.Spec.Maps[0].Join = "nowhere" }, `map "run-shards" join "nowhere" is not a defined state`},
		{"join entered outside the map", func(d *Definition) { d.Spec.Tasks[0].Next = "collate" },
			`task "plan" routes to "collate", which is the join of map "run-shards"; a join may only be entered through its map`},
		{"negative item timeout", func(d *Definition) { d.Spec.Maps[0].ItemTimeoutSeconds = -1 }, `map "run-shards" itemTimeoutSeconds -1 is negative`},
		{"task timeout exceeds item timeout", func(d *Definition) { d.Spec.Tasks[1].TimeoutSeconds = 3600 },
			`map "run-shards": task "run-shard" timeoutSeconds 3600 exceeds itemTimeoutSeconds 1800`},
		{"negative concurrency", func(d *Definition) { d.Spec.Maps[0].MaxConcurrent = -1 }, `map "run-shards" maxConcurrent -1 is outside 1..32`},
		{"concurrency above host bound", func(d *Definition) { d.Spec.Maps[0].MaxConcurrent = 33 }, `map "run-shards" maxConcurrent 33 is outside 1..32`},
		{"map nested in a parallel branch", func(d *Definition) {
			d.Spec.Tasks[0].Next = "fan"
			addTask(d, apiv1.Task{Name: "lens", Type: apiv1.TaskDeterministic, Goal: "lens",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: TargetJoin})
			addTask(d, apiv1.Task{Name: "end", Type: apiv1.TaskDeterministic, Goal: "end",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}})
			d.Spec.Parallels = []apiv1.Parallel{{Name: "fan", FailurePolicy: apiv1.BranchContinueOnError, Join: "end", MaxConcurrentBranches: 1,
				Branches: []apiv1.Branch{{Name: "a", Start: "run-shards"}, {Name: "b", Start: "lens"}}}}
		}, `parallel "fan" branch "a" contains map "run-shards"; a map may not be nested in a parallel branch`},
		{"binding collides with a static input", func(d *Definition) { d.Spec.Tasks[1].Inputs = map[string]string{"shard": "static"} },
			`map "run-shards" binding "shard" collides with static input "shard" of task "run-shard"`},
		{"binding outside the closed value set", func(d *Definition) { d.Spec.Maps[0].Bindings["shard"] = "item.packages" },
			`map "run-shards" binding "shard" value "item.packages" is not one of item, identity, digest`},
		{"binding name is not an input name", func(d *Definition) { d.Spec.Maps[0].Bindings["bad name"] = apiv1.MapBindingDigest },
			`map "run-shards" binding "bad name" is not a valid input name`},
		{"map name contains a dot", func(d *Definition) { d.Spec.Maps[0].Name = "run.shards"; d.Spec.Tasks[0].Next = "run.shards" },
			`map "run.shards" name must not contain '.'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := mapDefinition()
			tc.mutate(&def)
			assertCompileProblem(t, def, tc.want)
		})
	}
}

func assertCompileProblem(t *testing.T, def Definition, want string) {
	t.Helper()
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil {
		t.Fatalf("Compile succeeded, want error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Compile error = %v\nwant it to contain %q", err, want)
	}
}

func TestFanOutSourceSlotSchemaLowering(t *testing.T) {
	selective, err := Compile(selectiveParallelDefinition(), WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile selective: %v", err)
	}
	if got, ok := selective.SlotEnvelope("triage", "lenses"); !ok || got != schemas.FanOutSelection {
		t.Fatalf("selection source envelope = %q, %v; want %q", got, ok, schemas.FanOutSelection)
	}
	mapped, err := Compile(mapDefinition(), WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile map: %v", err)
	}
	if got, ok := mapped.SlotEnvelope("plan", "shards"); !ok || got != schemas.FanOutRoster {
		t.Fatalf("roster source envelope = %q, %v; want %q", got, ok, schemas.FanOutRoster)
	}
	authored := mapDefinition()
	authored.Spec.Tasks[0].ArtifactSlots[0].SchemaPath = "schemas/shards.schema.json"
	own, err := Compile(authored, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile authored schema: %v", err)
	}
	if got, ok := own.SlotEnvelope("plan", "shards"); ok {
		t.Fatalf("slot with its own schemaPath lowered envelope %q; the author schema must stay effective", got)
	}
}

func TestFanOutSourceSlotCannotHoldSelectionAndRoster(t *testing.T) {
	def := selectiveParallelDefinition()
	def.Spec.Parallels[0].Join = "fan-in"
	def.Spec.Tasks = append(def.Spec.Tasks,
		apiv1.Task{Name: "fan-in", Type: apiv1.TaskDeterministic, Goal: "route", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "per-lens"},
		apiv1.Task{Name: "lens-item", Type: apiv1.TaskDeterministic, Goal: "item", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: TargetJoin},
	)
	def.Spec.Maps = []apiv1.Map{{Name: "per-lens", Source: apiv1.FanOutSource{Task: "triage", Slot: "lenses"}, Task: "lens-item", FailurePolicy: apiv1.BranchContinueOnError, Join: "collate"}}
	assertCompileProblem(t, def, `one slot cannot hold both a selection and a roster`)
}

func TestFanOutFeaturesArePreviewDSL31Only(t *testing.T) {
	want := []string{
		"workflow.spec.maps", "workflow.spec.maps.bindings", "workflow.spec.maps.failurePolicy",
		"workflow.spec.maps.itemTimeoutSeconds", "workflow.spec.maps.join", "workflow.spec.maps.maxConcurrent",
		"workflow.spec.maps.onFailure", "workflow.spec.maps.source", "workflow.spec.maps.task",
		"workflow.spec.parallels.branches.optional", "workflow.spec.parallels.selection", "workflow.spec.parallels.selection.source",
	}
	registered := map[string]Feature{}
	for _, feature := range AllFeatures() {
		registered[string(feature.ID)] = feature
	}
	for _, id := range want {
		feature, ok := registered[id]
		if !ok {
			t.Fatalf("feature %s is not registered", id)
		}
		if feature.Level != SupportPreview || len(feature.DSLVersions) != 1 || feature.DSLVersions[0].Version != supportmatrix.V31DSLVersion {
			t.Fatalf("feature %s = %+v, want preview on DSL 3.1 only", id, feature)
		}
	}
	mapped := mapDefinition()
	features, err := FeaturesForWorkflow(mapped)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	got := featureIDStrings(features)
	for _, id := range []string{"workflow.spec.maps", "workflow.spec.maps.bindings", "workflow.spec.maps.maxConcurrent", "workflow.spec.maps.itemTimeoutSeconds"} {
		if !slices.Contains(got, id) {
			t.Fatalf("features = %v, want %s", got, id)
		}
	}
	if slices.Contains(got, "workflow.spec.maps.onFailure") {
		t.Fatalf("features = %v, onFailure is unused", got)
	}
}

func TestMapGraphProjection(t *testing.T) {
	machine, err := Compile(mapDefinition(), WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	graph := machine.Graph()
	var mapNode bool
	for _, node := range graph.Nodes {
		if node.ID == "run-shards" {
			mapNode = node.Kind == GraphNodeMap
		}
	}
	if !mapNode {
		t.Fatalf("graph nodes = %+v, want run-shards as a map node", graph.Nodes)
	}
	for _, want := range []GraphEdge{
		{Source: "plan", Target: "run-shards"},
		{Source: "run-shards", Target: "run-shard"},
		{Source: "run-shard", Target: "collate"},
	} {
		if !slices.Contains(graph.Edges, want) {
			t.Fatalf("graph edges = %+v, want %+v", graph.Edges, want)
		}
	}
}

func TestRefuseUnsupportedFanOutIsTyped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spec     apiv1.WorkflowSpec
		sentinel error
		feature  string
		state    string
	}{
		{"map", mapDefinition().Spec, ErrMapUnsupported, "spec.maps", "run-shards"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RefuseUnsupportedFanOut(tc.spec)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("RefuseUnsupportedFanOut = %v, want %v", err, tc.sentinel)
			}
			var typed *UnsupportedFanOutError
			if !errors.As(err, &typed) || typed.Feature != tc.feature || typed.State != tc.state {
				t.Fatalf("RefuseUnsupportedFanOut = %#v, want feature %q state %q", err, tc.feature, tc.state)
			}
		})
	}
	if err := RefuseUnsupportedFanOut(selectiveParallelDefinition().Spec); err != nil {
		t.Fatalf("selective parallel refused after engine support: %v", err)
	}
	plain := selectiveParallelDefinition().Spec
	plain.Parallels[0].Selection = nil
	for i := range plain.Parallels[0].Branches {
		plain.Parallels[0].Branches[i].Optional = false
	}
	if err := RefuseUnsupportedFanOut(plain); err != nil {
		t.Fatalf("plain parallel refused: %v", err)
	}
}
