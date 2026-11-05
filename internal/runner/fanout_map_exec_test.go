package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workflow"
)

// Map execution corpus (FAN-4, #7385; design plan-driven-dynamic-fan-out.md
// §14 cases 9–11, 15–20 and 22). Every fixture runs the §6.1 shape: a
// planner publishes a roster into its `shards` slot, the run-shards map runs
// run-shard once per item with typed bindings, and collate joins.

const mapExampleRoster = `{"schemaVersion":1,"items":[` +
	`{"identity":"unit-c","item":{"packages":["./internal/c/..."]}},` +
	`{"identity":"unit-a","item":{"packages":["./internal/a/..."]}},` +
	`{"identity":"unit-b","item":{"packages":["./internal/b/..."]}}]}`

var mapExampleIdentities = []string{"unit-a", "unit-b", "unit-c"}

type mapOptions struct {
	width       int32
	policy      apiv1.BranchFailurePolicy
	onFailure   string
	itemTimeout int32
	retry       *apiv1.RetryPolicy
	minimum     apiv1.Integrity
	contextFrom []string
	// agentic makes the planner and the join agentic, as in §6.1.
	agentic bool
}

// mapExampleMachine compiles the §6.1 example: plan → run-shards{run-shard
// per item} → collate, with shard/shardIdentity/shardDigest bindings.
func mapExampleMachine(t *testing.T, opts mapOptions) *workflow.Machine {
	t.Helper()
	if opts.policy == "" {
		opts.policy = apiv1.BranchContinueOnError
	}
	det := func(name, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run: &apiv1.DeterministicRun{Command: []string{name}, Workspace: apiv1.WorkspaceScratch}, Next: next}
	}
	slots := []apiv1.ArtifactSlot{{Name: "shards", MediaType: "application/json"}}
	plan := det("plan", "run-shards")
	plan.Goal, plan.ArtifactSlots = "Select the immutable test-shard roster.", slots
	collate := det("collate", workflow.TerminalComplete)
	collate.Goal = "Summarize every shard result in identity order."
	if opts.agentic {
		plan = apiv1.Task{Name: "plan", Type: apiv1.TaskAgentic, Goober: "shard-planner", Goal: plan.Goal,
			Workspace: apiv1.WorkspaceScratch, ArtifactSlots: slots, Next: "run-shards"}
		collate = apiv1.Task{Name: "collate", Type: apiv1.TaskAgentic, Goober: "test-lead", Goal: collate.Goal,
			Workspace: apiv1.WorkspaceScratch, Next: workflow.TerminalComplete}
	}
	shard := det("run-shard", workflow.TargetJoin)
	shard.Goal, shard.Retry, shard.MinimumIntegrity, shard.ContextFrom = "Run one shard.", opts.retry, opts.minimum, opts.contextFrom
	tasks := []apiv1.Task{plan, shard, collate}
	if opts.onFailure == "shards-failed" {
		tasks = append(tasks, det("shards-failed", workflow.TerminalComplete))
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "shard-map", Version: 1, DSLVersion: "3.1",
		Annotations: map[string]string{"goobers.dev/allow-preview-features": "true"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "demo", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "plan", Tasks: tasks,
			Maps: []apiv1.Map{{
				Name: "run-shards", Source: apiv1.FanOutSource{Task: "plan", Slot: "shards"}, Task: "run-shard",
				Bindings: map[string]apiv1.MapBinding{
					"shard": apiv1.MapBindingItem, "shardIdentity": apiv1.MapBindingIdentity, "shardDigest": apiv1.MapBindingDigest,
				},
				FailurePolicy: opts.policy, OnFailure: opts.onFailure, ItemTimeoutSeconds: opts.itemTimeout,
				MaxConcurrent: opts.width, Join: "collate",
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile map fixture: %v", err)
	}
	return machine
}

// mapHarness scripts the planner, the mapped task and the join, and records
// what each received.
type mapHarness struct {
	runner          *Runner
	runsDir         string
	roster          []byte
	rosterIntegrity apiv1.Integrity

	mu        sync.Mutex
	calls     map[string]int
	attempts  map[string]int
	items     map[string]map[string]any
	joins     []apiv1.InvocationEnvelope
	active    int
	maxActive int
	// script, when set, may replace an item attempt's result.
	script func(identity string, attempt int) (apiv1.ResultEnvelope, error, bool)
	// hold, when set, runs inside an item attempt before it returns.
	hold func(identity string)
}

func newMapHarness(t *testing.T, roster string) *mapHarness {
	t.Helper()
	h := &mapHarness{roster: []byte(roster), calls: map[string]int{}, attempts: map[string]int{}, items: map[string]map[string]any{}}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	r, err := New(Config{
		ResolveRepositoryIdentity: lookupRunnerRepositoryFixture,
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &mapStubDeterministic{h: h, rec: rec}, nil
		},
		NewAgentic: func(_ string, rec ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			return &mapStubGoober{h: h, rec: rec}, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		ScratchDir:   t.TempDir(),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new map runner: %v", err)
	}
	h.runner, h.runsDir = r, runsDir
	return h
}

type mapStubDeterministic struct {
	h   *mapHarness
	rec ArtifactRecorder
}

func (d *mapStubDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return d.h.dispatch(ctx, d.rec, env)
}

type mapStubGoober struct {
	h   *mapHarness
	rec ArtifactRecorder
}

func (g *mapStubGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return g.h.dispatch(ctx, g.rec, env)
}

func (g *mapStubGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("no reviewer in the map fixture")
}

func (h *mapHarness) dispatch(ctx context.Context, rec ArtifactRecorder, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	stage := env.TaskID[strings.LastIndex(env.TaskID, ":")+1:]
	switch stage {
	case "plan":
		h.count(stage)
		return h.publishRoster(ctx, rec, env)
	case "run-shard":
		return h.runItem(rec, env)
	case "collate":
		h.mu.Lock()
		h.calls[stage]++
		h.joins = append(h.joins, env)
		h.mu.Unlock()
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"collated": true}}, nil
	default:
		h.count(stage)
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"ran": stage}}, nil
	}
}

func (h *mapHarness) count(stage string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls[stage]++
}

func (h *mapHarness) callCount(stage string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[stage]
}

// publishRoster publishes the roster through the real named artifact-set
// boundary (Prepare → Bind → Publish), as an executor does. It never supplies
// a digest the document does not already carry.
func (h *mapHarness) publishRoster(ctx context.Context, rec ArtifactRecorder, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if env.ArtifactPublication == nil {
		return apiv1.ResultEnvelope{}, errors.New("plan dispatched without its slot publication contract")
	}
	dir, err := os.MkdirTemp("", "roster-*")
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "shards.json"), h.roster, 0o600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	manifest, _ := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion,
		Entries: []artifactset.ManifestEntry{{Name: "shards", Path: "shards.json", MediaType: "application/json"}}})
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	_, scrubber := journal.DefaultScrubber()
	prepared, err := artifactset.Prepare(ctx, dir, "manifest.json", artifactset.NewSanitizer(scrubber))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := prepared.Bind(env.ArtifactPublication, env.Attempt); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	pointers, err := prepared.Publish(ctx, func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := rec.RecordArtifact(env.TaskID+"/"+name, data)
		integrity := ref.Integrity
		if h.rosterIntegrity != "" {
			integrity = h.rosterIntegrity
		}
		return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: media, Integrity: integrity}, err
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: pointers}, nil
}

// runItem is one mapped-task attempt: it records the bound inputs, tracks
// worker concurrency, and returns one output and one artifact per identity.
func (h *mapHarness) runItem(rec ArtifactRecorder, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	identity, _ := env.Inputs["shardIdentity"].(string)
	h.mu.Lock()
	h.calls["run-shard"]++
	h.attempts[identity]++
	attempt := h.attempts[identity]
	inputs := make(map[string]any, len(env.Inputs))
	for k, v := range env.Inputs {
		inputs[k] = v
	}
	h.items[identity] = inputs
	h.active++
	h.maxActive = max(h.maxActive, h.active)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.active--
		h.mu.Unlock()
	}()
	if h.hold != nil {
		h.hold(identity)
	}
	if h.script != nil {
		if result, err, ok := h.script(identity, attempt); ok {
			return result, err
		}
	}
	ref, err := rec.RecordArtifact(env.TaskID+"/"+identity+".txt", []byte("result of "+identity))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{
		Status:    apiv1.ResultSuccess,
		Outputs:   map[string]any{"shard": identity, "attempt": attempt},
		Artifacts: []apiv1.ArtifactPointer{{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "text/plain", Integrity: ref.Integrity}},
	}, nil
}

func (h *mapHarness) start(t *testing.T, runID string, machine *workflow.Machine) Result {
	t.Helper()
	result, err := h.runner.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo", Machine: machine, Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return result
}

func (h *mapHarness) events(t *testing.T, runID string) []journal.Event {
	t.Helper()
	return readRunEvents(t, h.runsDir, runID)
}

func (h *mapHarness) join(t *testing.T) ([]journal.BranchOutcome, []MapItemResult, apiv1.InvocationEnvelope) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.joins) != 1 {
		t.Fatalf("join dispatched %d times, want exactly once", len(h.joins))
	}
	env := h.joins[0]
	completeness, ok := env.Inputs[runsemantics.MapCompletenessInput].([]journal.BranchOutcome)
	if !ok {
		t.Fatalf("join %s = %T, want []journal.BranchOutcome", runsemantics.MapCompletenessInput, env.Inputs[runsemantics.MapCompletenessInput])
	}
	results, ok := env.Inputs[runsemantics.MapResultsInput].([]MapItemResult)
	if !ok {
		t.Fatalf("join %s = %T, want []MapItemResult", runsemantics.MapResultsInput, env.Inputs[runsemantics.MapResultsInput])
	}
	if _, static := env.Inputs[runsemantics.BranchCompletenessInput]; static {
		t.Fatalf("map join also received %s", runsemantics.BranchCompletenessInput)
	}
	return completeness, results, env
}

// assertMapJournalVerifies replays a live journal through the strict resume
// verifier: every record the executor wrote must be one resume accepts.
func assertMapJournalVerifies(t *testing.T, events []journal.Event, machine *workflow.Machine) {
	t.Helper()
	if pending, err := findPendingFanOut(events, machine); err != nil || pending.start != -1 {
		t.Fatalf("live map journal does not verify: pending %d, %v", pending.start, err)
	}
}

func singleEvent(t *testing.T, events []journal.Event, kind journal.EventType) journal.Event {
	t.Helper()
	matched := eventsOfType(events, kind)
	if len(matched) != 1 {
		t.Fatalf("%s recorded %d times, want exactly once", kind, len(matched))
	}
	return matched[0]
}

func mapItemsSummary(items []journal.MapItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.Identity)
	}
	return strings.Join(parts, ",")
}

func mapCompletenessSummary(record []journal.BranchOutcome) string {
	parts := make([]string, 0, len(record))
	for _, entry := range record {
		parts = append(parts, fmt.Sprintf("%d:%s:%s", entry.Branch, entry.MapItemIdentity, entry.Status))
	}
	return strings.Join(parts, ",")
}

func stageAttempts(events []journal.Event, stage string, branch int) []int {
	var attempts []int
	for _, event := range eventsOfType(events, journal.EventStageStarted) {
		if event.Stage == stage && event.Branch == branch {
			attempts = append(attempts, event.Attempt)
		}
	}
	return attempts
}

// §14 case 9, case 19 and the §6.1 acceptance: an agentic-style planner that
// omits digests produces exactly three branches with stable identity and
// digest, every item receives its typed bindings, and the join runs once with
// identity-ordered mapCompleteness and mapResults that keep each item's
// outputs and artifacts distinct.
func TestConformanceMapExampleRunsEndToEnd(t *testing.T) {
	for _, width := range []int32{1, 4} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			h := newMapHarness(t, mapExampleRoster)
			runID := fmt.Sprintf("map-example-%d", width)
			machine := mapExampleMachine(t, mapOptions{width: width, itemTimeout: 1800, agentic: true})
			if result := h.start(t, runID, machine); result.Phase != journal.PhaseCompleted {
				t.Fatalf("phase = %q, want completed", result.Phase)
			}
			assertMapJournalVerifies(t, h.events(t, runID), machine)
			for stage, want := range map[string]int{"plan": 1, "run-shard": 3, "collate": 1} {
				if got := h.callCount(stage); got != want {
					t.Errorf("%s ran %d times, want %d", stage, got, want)
				}
			}
			events := h.events(t, runID)
			started := singleEvent(t, events, journal.EventMapStarted)
			roster := started.MapRoster
			if roster == nil || mapItemsSummary(roster.Items) != strings.Join(mapExampleIdentities, ",") {
				t.Fatalf("map.started roster = %+v, want the identity-sorted three-item roster", roster)
			}
			if roster.Source.Task != "plan" || roster.Source.Slot != "shards" || roster.Source.Integrity != apiv1.IntegrityDerived ||
				roster.MaxConcurrent != width || roster.ItemTimeoutSeconds != 1800 {
				t.Fatalf("map.started source/bounds = %+v", roster)
			}
			for _, item := range roster.Items {
				digest, err := fanout.ItemDigest(item.Identity, item.Item)
				if err != nil || digest != item.Digest {
					t.Fatalf("item %s digest %s, want the computed canonical %s (%v)", item.Identity, item.Digest, digest, err)
				}
			}
			branches := eventsOfType(events, journal.EventBranchStarted)
			if len(branches) != 3 {
				t.Fatalf("branch.started = %d, want 3", len(branches))
			}
			for _, branch := range branches {
				item := roster.Items[branch.Branch-1]
				if branch.Parallel != "run-shards" || branch.BranchName != item.Identity || branch.MapItemIdentity != item.Identity ||
					branch.MapItemDigest != item.Digest || branch.Stage != "run-shard" {
					t.Fatalf("branch.started %+v does not carry roster branch %d", branch, branch.Branch)
				}
				inputs := h.items[item.Identity]
				bound, _ := json.Marshal(inputs["shard"])
				if string(bound) != string(item.Item) || inputs["shardIdentity"] != item.Identity || inputs["shardDigest"] != item.Digest {
					t.Fatalf("item %s bindings = %v, want item %s, identity and digest", item.Identity, inputs, item.Item)
				}
			}
			finished := singleEvent(t, events, journal.EventMapFinished)
			if got := mapCompletenessSummary(finished.Completeness); got != "1:unit-a:succeeded,2:unit-b:succeeded,3:unit-c:succeeded" || finished.Target != "collate" {
				t.Fatalf("map.finished = %s → %q", got, finished.Target)
			}
			completeness, results, env := h.join(t)
			if !reflect.DeepEqual(completeness, finished.Completeness) {
				t.Fatalf("join mapCompleteness = %+v, want the map.finished record", completeness)
			}
			if len(results) != 3 {
				t.Fatalf("mapResults = %+v, want 3 entries", results)
			}
			for i, result := range results {
				item := roster.Items[i]
				if result.Identity != item.Identity || result.Digest != item.Digest || result.Branch != i+1 || result.Status != journal.BranchSucceeded ||
					result.Outputs["shard"] != item.Identity || result.Integrity != apiv1.IntegrityDerived {
					t.Fatalf("mapResults[%d] = %+v, want item %s's own result", i, result, item.Identity)
				}
				if len(result.Pointers) != 1 || result.Pointers[0].BranchName != item.Identity || result.Pointers[0].Branch != i+1 ||
					result.Pointers[0].Artifact == nil {
					t.Fatalf("mapResults[%d] pointers = %+v, want one artifact tagged with its identity", i, result.Pointers)
				}
			}
			tagged := map[string]bool{}
			for _, pointer := range env.ContextPointers {
				if pointer.Branch > 0 {
					tagged[pointer.BranchName] = true
				}
			}
			if len(tagged) != 3 {
				t.Fatalf("join context pointers carry identities %v, want all three items", tagged)
			}
		})
	}
}

// §14 case 10: a different input array order produces an identical
// normalized roster and an identical result order.
func TestConformanceMapInputOrderDoesNotChangeRoster(t *testing.T) {
	permuted := `{"schemaVersion":1,"items":[` +
		`{"identity":"unit-b","item":{"packages":["./internal/b/..."]}},` +
		`{"identity":"unit-c","item":{"packages":["./internal/c/..."]}},` +
		`{"identity":"unit-a","item":{"packages":["./internal/a/..."]}}]}`
	run := func(roster string) ([]journal.MapItem, []journal.BranchOutcome, []string) {
		h := newMapHarness(t, roster)
		h.start(t, "map-order", mapExampleMachine(t, mapOptions{width: 3}))
		events := h.events(t, "map-order")
		_, results, _ := h.join(t)
		order := make([]string, 0, len(results))
		for _, result := range results {
			order = append(order, fmt.Sprintf("%s=%v", result.Identity, result.Outputs["shard"]))
		}
		return singleEvent(t, events, journal.EventMapStarted).MapRoster.Items, singleEvent(t, events, journal.EventMapFinished).Completeness, order
	}
	itemsA, finishedA, orderA := run(mapExampleRoster)
	itemsB, finishedB, orderB := run(permuted)
	if !reflect.DeepEqual(itemsA, itemsB) {
		t.Fatalf("normalized rosters differ:\n%+v\n%+v", itemsA, itemsB)
	}
	if !reflect.DeepEqual(finishedA, finishedB) || !reflect.DeepEqual(orderA, orderB) {
		t.Fatalf("result order differs: %v / %v", orderA, orderB)
	}
}

// §14 case 11: different completion interleavings produce an identical
// map.finished, and the join reads results in identity order regardless.
func TestConformanceMapCompletionInterleavingsProduceIdenticalFinish(t *testing.T) {
	run := func(release []string) (journal.Event, []string, []string) {
		h := newMapHarness(t, mapExampleRoster)
		gates := map[string]chan struct{}{}
		for _, identity := range mapExampleIdentities {
			gates[identity] = make(chan struct{})
		}
		arrived := make(chan string, len(mapExampleIdentities))
		h.hold = func(identity string) {
			arrived <- identity
			<-gates[identity]
		}
		const runID = "map-interleave"
		machine := mapExampleMachine(t, mapOptions{width: 3})
		type outcome struct {
			result Result
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := h.runner.Start(context.Background(), StartInput{
				RunID: runID, Gaggle: "demo", Machine: machine, Trigger: journal.Trigger{Kind: journal.TriggerManual},
			})
			done <- outcome{result, err}
		}()
		for range mapExampleIdentities {
			select {
			case <-arrived:
			case <-time.After(runnerTestWaitTimeout):
				t.Fatal("items did not all start concurrently")
			}
		}
		for _, identity := range release {
			close(gates[identity])
			waitForItemSettled(t, h, runID, identity)
		}
		select {
		case got := <-done:
			if got.err != nil || got.result.Phase != journal.PhaseCompleted {
				t.Fatalf("Start = %+v, %v", got.result, got.err)
			}
		case <-time.After(runnerTestWaitTimeout):
			t.Fatal("run did not finish")
		}
		events := h.events(t, runID)
		var settled []string
		for _, event := range eventsOfType(events, journal.EventBranchFinished) {
			settled = append(settled, event.MapItemIdentity)
		}
		_, results, _ := h.join(t)
		order := make([]string, 0, len(results))
		for _, result := range results {
			order = append(order, result.Identity)
		}
		finished := singleEvent(t, events, journal.EventMapFinished)
		finished.Seq, finished.Time = 0, time.Time{}
		return finished, settled, order
	}
	forward, settledForward, orderForward := run([]string{"unit-a", "unit-b", "unit-c"})
	reverse, settledReverse, orderReverse := run([]string{"unit-c", "unit-b", "unit-a"})
	if reflect.DeepEqual(settledForward, settledReverse) {
		t.Fatalf("the two runs settled in the same order %v; the interleavings did not differ", settledForward)
	}
	if !reflect.DeepEqual(forward.Completeness, reverse.Completeness) || forward.Target != reverse.Target {
		t.Fatalf("map.finished differs across interleavings:\n%+v\n%+v", forward, reverse)
	}
	if !reflect.DeepEqual(orderForward, orderReverse) || strings.Join(orderForward, ",") != strings.Join(mapExampleIdentities, ",") {
		t.Fatalf("join result order = %v / %v, want identity order", orderForward, orderReverse)
	}
}

// waitForItemSettled polls the journal until the item's branch.finished is
// durable, so the next release cannot overtake it.
func waitForItemSettled(t *testing.T, h *mapHarness, runID, identity string) {
	t.Helper()
	deadline := time.Now().Add(runnerTestWaitTimeout)
	for time.Now().Before(deadline) {
		rd, err := journal.OpenRead(filepath.Join(h.runsDir, runID))
		if err == nil {
			events, err := rd.Events()
			if err == nil {
				for _, event := range eventsOfType(events, journal.EventBranchFinished) {
					if event.MapItemIdentity == identity {
						return
					}
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("item %s never settled", identity)
}

// §14 case 15: maxConcurrent bounds the active workers while every item
// settles. The first `width` items wait for each other, so the bound is
// demonstrably reached, never exceeded.
func TestConformanceMapMaxConcurrentBoundsActiveWorkers(t *testing.T) {
	roster := `{"schemaVersion":1,"items":[` +
		`{"identity":"i1","item":{}},{"identity":"i2","item":{}},{"identity":"i3","item":{}},` +
		`{"identity":"i4","item":{}},{"identity":"i5","item":{}}]}`
	for _, width := range []int32{1, 2} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			h := newMapHarness(t, roster)
			var startedMu sync.Mutex
			started := 0
			h.hold = func(string) {
				startedMu.Lock()
				started++
				first := started <= int(width)
				startedMu.Unlock()
				deadline := time.Now().Add(runnerTestWaitTimeout)
				for first && time.Now().Before(deadline) {
					h.mu.Lock()
					reached := h.active >= int(width)
					h.mu.Unlock()
					if reached {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
			runID := fmt.Sprintf("map-bound-%d", width)
			if result := h.start(t, runID, mapExampleMachine(t, mapOptions{width: width})); result.Phase != journal.PhaseCompleted {
				t.Fatalf("phase = %q", result.Phase)
			}
			if h.maxActive != int(width) {
				t.Fatalf("max active workers = %d, want exactly the bound %d", h.maxActive, width)
			}
			finished := singleEvent(t, h.events(t, runID), journal.EventMapFinished)
			if got := mapCompletenessSummary(finished.Completeness); got != "1:i1:succeeded,2:i2:succeeded,3:i3:succeeded,4:i4:succeeded,5:i5:succeeded" {
				t.Fatalf("completeness = %s, want every item settled", got)
			}
		})
	}
}

// §14 case 16 (retry): a retry repeats only the mapped task for the same
// frozen item. It never reselects, re-hashes or reorders the roster.
func TestConformanceMapRetryStaysScopedToOneItem(t *testing.T) {
	h := newMapHarness(t, mapExampleRoster)
	h.script = func(identity string, attempt int) (apiv1.ResultEnvelope, error, bool) {
		if identity == "unit-b" && attempt == 1 {
			return apiv1.ResultEnvelope{}, errors.New("transient shard failure"), true
		}
		return apiv1.ResultEnvelope{}, nil, false
	}
	const runID = "map-retry"
	machine := mapExampleMachine(t, mapOptions{width: 3, retry: &apiv1.RetryPolicy{MaxAttempts: 2}})
	if result := h.start(t, runID, machine); result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q", result.Phase)
	}
	events := h.events(t, runID)
	assertMapJournalVerifies(t, events, machine)
	for branch, want := range map[int]string{1: "[1]", 2: "[1 2]", 3: "[1]"} {
		if got := fmt.Sprint(stageAttempts(events, "run-shard", branch)); got != want {
			t.Errorf("branch %d attempts = %s, want %s", branch, got, want)
		}
	}
	singleEvent(t, events, journal.EventMapStarted)
	if h.callCount("plan") != 1 {
		t.Fatal("a retry re-ran the planner")
	}
	if got := mapCompletenessSummary(singleEvent(t, events, journal.EventMapFinished).Completeness); got != "1:unit-a:succeeded,2:unit-b:succeeded,3:unit-c:succeeded" {
		t.Fatalf("completeness = %s", got)
	}
	_, results, _ := h.join(t)
	if fmt.Sprint(results[1].Outputs["attempt"]) != "2" || fmt.Sprint(results[0].Outputs["attempt"]) != "1" {
		t.Fatalf("mapResults = %+v, want unit-b's retried attempt and unit-a's first", results)
	}
}

// §14 case 16 (timeout): itemTimeoutSeconds keeps the static boundary rule
// and bounds one item. A resumed item whose budget already expired settles
// timed-out at its next boundary; its siblings run and succeed.
func TestConformanceMapTimeoutStaysScopedToOneItem(t *testing.T) {
	h := newMapHarness(t, "never read")
	machine := mapExampleMachine(t, mapOptions{width: 1, itemTimeout: 60})
	const runID = "map-timeout"
	mapFixtureJournal(t, h.runsDir, runID, machine, apiv1.IntegrityDerived, func(items []journal.MapItem, clock *time.Time) []mapFixtureStep {
		return append(mapItemSettled(items, 1, journal.BranchSucceeded),
			mapFixtureStep{at: time.Now().Add(-2 * time.Hour), event: mapBranchEvent(items, journal.EventBranchStarted, 2, "")})
	})
	result, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	if h.callCount("run-shard") != 1 || h.attempts["unit-c"] != 1 || h.attempts["unit-b"] != 0 {
		t.Fatalf("item attempts = %v, want only unit-c to run", h.attempts)
	}
	events := h.events(t, runID)
	if got := mapCompletenessSummary(singleEvent(t, events, journal.EventMapFinished).Completeness); got != "1:unit-a:succeeded,2:unit-b:timed-out,3:unit-c:succeeded" {
		t.Fatalf("completeness = %s", got)
	}
	if h.callCount("plan") != 0 || h.callCount("collate") != 1 {
		t.Fatalf("plan ran %d, collate ran %d", h.callCount("plan"), h.callCount("collate"))
	}
}

// §14 case 17: fail_fast records every unstarted item as cancelled and routes
// to onFailure without running the join.
func TestConformanceMapFailFastCancelsUnstartedItems(t *testing.T) {
	h := newMapHarness(t, mapExampleRoster)
	h.script = func(identity string, _ int) (apiv1.ResultEnvelope, error, bool) {
		if identity == "unit-a" {
			return apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "shard_failed", Message: "unit-a failed"}}, nil, true
		}
		return apiv1.ResultEnvelope{}, nil, false
	}
	const runID = "map-fail-fast"
	machine := mapExampleMachine(t, mapOptions{width: 1, policy: apiv1.BranchFailFast, onFailure: "shards-failed"})
	if result := h.start(t, runID, machine); result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q", result.Phase)
	}
	events := h.events(t, runID)
	assertMapJournalVerifies(t, events, machine)
	if got := strings.Join(startedBranches(events), ","); got != "1:unit-a" {
		t.Fatalf("started branches = %s, want only unit-a", got)
	}
	finished := singleEvent(t, events, journal.EventMapFinished)
	if got := mapCompletenessSummary(finished.Completeness); got != "1:unit-a:failed,2:unit-b:cancelled,3:unit-c:cancelled" || finished.Target != "shards-failed" {
		t.Fatalf("map.finished = %s → %q", got, finished.Target)
	}
	for _, event := range eventsOfType(events, journal.EventBranchFinished) {
		if event.Branch > 1 && (event.MapItemIdentity == "" || event.MapItemDigest == "") {
			t.Fatalf("cancelled item record lost its roster identity: %+v", event)
		}
	}
	if h.callCount("collate") != 0 || h.callCount("shards-failed") != 1 || h.callCount("run-shard") != 1 {
		t.Fatalf("calls = %v", h.calls)
	}
}

// §14 case 18: an empty roster writes map.started and map.finished and
// enters the join exactly once with empty, ordered inputs.
func TestConformanceMapEmptyRosterJoinsOnce(t *testing.T) {
	h := newMapHarness(t, `{"schemaVersion":1,"items":[]}`)
	const runID = "map-empty"
	machine := mapExampleMachine(t, mapOptions{width: 2})
	if result := h.start(t, runID, machine); result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q", result.Phase)
	}
	events := h.events(t, runID)
	assertMapJournalVerifies(t, events, machine)
	started := singleEvent(t, events, journal.EventMapStarted)
	if started.MapRoster == nil || started.MapRoster.Items == nil || len(started.MapRoster.Items) != 0 {
		t.Fatalf("map.started roster = %+v, want an empty array", started.MapRoster)
	}
	finished := singleEvent(t, events, journal.EventMapFinished)
	if len(finished.Completeness) != 0 || finished.Target != "collate" {
		t.Fatalf("map.finished = %+v, want an empty record routed to the join", finished)
	}
	if len(eventsOfType(events, journal.EventBranchStarted)) != 0 || h.callCount("run-shard") != 0 {
		t.Fatal("an empty roster started an item")
	}
	if joinRunsOf(events, "collate") != 1 {
		t.Fatalf("join ran %d times, want once", joinRunsOf(events, "collate"))
	}
	completeness, results, _ := h.join(t)
	if completeness == nil || results == nil || len(completeness) != 0 || len(results) != 0 {
		t.Fatalf("join inputs = %#v / %#v, want empty ordered sets", completeness, results)
	}
}

func joinRunsOf(events []journal.Event, join string) int {
	count := 0
	for _, event := range eventsOfType(events, journal.EventStageStarted) {
		if event.Stage == join {
			count++
		}
	}
	return count
}

// §14 case 20: roster integrity flows through mapped input admission, output
// integrity and the join without upgrading provenance. contextFrom keeps the
// planner's own artifact out of run-shard's context, so the bindings are the
// only graded input that reaches it.
func TestConformanceMapRosterIntegrityFlowsThroughBindings(t *testing.T) {
	isolated := []string{"run-shard"}
	t.Run("output integrity", func(t *testing.T) {
		h := newMapHarness(t, mapExampleRoster)
		h.rosterIntegrity = apiv1.IntegrityUnapproved
		const runID = "map-integrity"
		if result := h.start(t, runID, mapExampleMachine(t, mapOptions{width: 3, contextFrom: isolated})); result.Phase != journal.PhaseCompleted {
			t.Fatalf("phase = %q", result.Phase)
		}
		events := h.events(t, runID)
		if got := singleEvent(t, events, journal.EventMapStarted).MapRoster.Source.Integrity; got != apiv1.IntegrityUnapproved {
			t.Fatalf("frozen roster integrity = %q", got)
		}
		for _, event := range eventsOfType(events, journal.EventStageFinished) {
			if (event.Stage == "run-shard" || event.Stage == "collate") && event.Integrity != apiv1.IntegrityUnapproved {
				t.Fatalf("%s branch %d finished %q, want the roster's unapproved grade", event.Stage, event.Branch, event.Integrity)
			}
		}
		_, results, _ := h.join(t)
		for _, result := range results {
			if result.Integrity != apiv1.IntegrityUnapproved {
				t.Fatalf("mapResults %s integrity = %q", result.Identity, result.Integrity)
			}
		}
	})
	t.Run("admission", func(t *testing.T) {
		h := newMapHarness(t, mapExampleRoster)
		h.rosterIntegrity = apiv1.IntegrityUnapproved
		const runID = "map-admission"
		result, err := h.runner.Start(context.Background(), StartInput{
			RunID: runID, Gaggle: "demo", Trigger: journal.Trigger{Kind: journal.TriggerManual},
			Machine: mapExampleMachine(t, mapOptions{width: 1, contextFrom: isolated, minimum: apiv1.IntegrityMaintainer}),
		})
		var admission *apiv1.IntegrityAdmissionError
		if !errors.As(err, &admission) || admission.Input != "shard" || result.Phase != journal.PhaseFailed {
			t.Fatalf("Start = %+v, %v; want a failed run refusing the shard binding", result, err)
		}
		if h.callCount("run-shard") != 0 {
			t.Fatal("an item below its minimum integrity was dispatched")
		}
		refused := false
		for _, event := range eventsOfType(h.events(t, runID), journal.EventError) {
			if event.Error != nil && event.Error.Code == apiv1.IntegrityAdmissionErrorCode && event.Stage == "run-shard" &&
				strings.Contains(event.Error.Message, `input "shard"`) {
				refused = true
			}
		}
		if !refused {
			t.Fatal("no integrity admission refusal naming the shard binding was journaled")
		}
	})
	t.Run("resume keeps the journaled grade", func(t *testing.T) {
		h := newMapHarness(t, "never read")
		machine := mapExampleMachine(t, mapOptions{width: 3, contextFrom: isolated})
		const runID = "map-integrity-resume"
		mapFixtureJournal(t, h.runsDir, runID, machine, apiv1.IntegrityUnapproved, nil)
		if result, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine}); err != nil || result.Phase != journal.PhaseCompleted {
			t.Fatalf("Resume = %+v, %v", result, err)
		}
		for _, event := range eventsOfType(h.events(t, runID), journal.EventStageFinished) {
			if event.Stage == "run-shard" && event.Integrity != apiv1.IntegrityUnapproved {
				t.Fatalf("resumed item finished %q, want the journaled unapproved grade", event.Integrity)
			}
		}
	})
}

// taskInputGrades grades each binding with the roster's integrity and a map
// join's inputs with the weakest of the roster and the item results.
func TestTaskInputGradesCarryRosterIntegrity(t *testing.T) {
	machine := mapExampleMachine(t, mapOptions{})
	def, _ := machine.Map("run-shards")
	task, _ := machine.Task("run-shard")
	items := mapFixtureItems(t)
	par := newMapExec(def, journal.MapRoster{Source: journal.FanOutSourceRef{Integrity: apiv1.IntegrityDerived}, Items: items}, 7)
	grades := taskInputGrades(taskFrame{t: task, in: StartInput{Machine: machine}, mapItem: par.itemBinding(2)})
	if len(grades) != 3 || grades["shard"] != apiv1.IntegrityDerived || grades["shardIdentity"] != apiv1.IntegrityDerived || grades["shardDigest"] != apiv1.IntegrityDerived {
		t.Fatalf("binding grades = %v", grades)
	}
	if par.itemBinding(0) != nil || par.itemBinding(4) != nil || newParallelExec(apiv1.Parallel{}).itemBinding(1) != nil {
		t.Fatal("a binding resolved outside the roster")
	}
	par.mapVisit.join = &mapJoin{rosterIntegrity: apiv1.IntegrityDerived, resultsIntegrity: apiv1.IntegrityUnapproved}
	collate, _ := machine.Task("collate")
	grades = taskInputGrades(taskFrame{t: collate, in: StartInput{Machine: machine}, fanIn: par})
	if grades[runsemantics.MapCompletenessInput] != apiv1.IntegrityDerived || grades[runsemantics.MapResultsInput] != apiv1.IntegrityUnapproved {
		t.Fatalf("join grades = %v", grades)
	}
}

// §14 case 22 for maps: fault injection after map.started, during several
// item stages, after every settlement but before map.finished, and after
// map.finished but before the join. Resume never re-runs or re-reads the
// planner, never re-runs a settled item, and joins exactly once.
func TestConformanceMapResumeNeverRerunsSource(t *testing.T) {
	boundaries := []struct {
		name  string
		tail  func([]journal.MapItem, *time.Time) []mapFixtureStep
		items int
	}{
		{"after map.started", nil, 3},
		{"during item stages", func(items []journal.MapItem, _ *time.Time) []mapFixtureStep {
			return []mapFixtureStep{
				{event: mapBranchEvent(items, journal.EventBranchStarted, 1, "")},
				{event: journal.Event{Type: journal.EventStageStarted, Branch: 1, Stage: "run-shard", Attempt: 1}},
				{event: mapBranchEvent(items, journal.EventBranchStarted, 2, "")},
				{event: journal.Event{Type: journal.EventStageStarted, Branch: 2, Stage: "run-shard", Attempt: 1}},
				{event: journal.Event{Type: journal.EventStageFinished, Branch: 2, Stage: "run-shard", Attempt: 1,
					Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"shard": "unit-b", "attempt": 1}, Integrity: apiv1.IntegrityDerived}},
			}
		}, 2},
		{"after every settlement", func(items []journal.MapItem, _ *time.Time) []mapFixtureStep {
			var steps []mapFixtureStep
			for id := 1; id <= 3; id++ {
				steps = append(steps, mapItemSettled(items, id, journal.BranchSucceeded)...)
			}
			return steps
		}, 0},
		{"after map.finished", func(items []journal.MapItem, _ *time.Time) []mapFixtureStep {
			var steps []mapFixtureStep
			var completeness []journal.BranchOutcome
			for id := 1; id <= 3; id++ {
				steps = append(steps, mapItemSettled(items, id, journal.BranchSucceeded)...)
				completeness = append(completeness, journal.BranchOutcome{Branch: id, Name: items[id-1].Identity, Status: journal.BranchSucceeded,
					MapItemIdentity: items[id-1].Identity, MapItemDigest: items[id-1].Digest})
			}
			return append(steps, mapFixtureStep{event: journal.Event{Type: journal.EventMapFinished, Parallel: "run-shards", Target: "collate", Completeness: completeness}})
		}, 0},
	}
	for _, boundary := range boundaries {
		for _, width := range []int32{1, 3} {
			t.Run(fmt.Sprintf("%s/width-%d", boundary.name, width), func(t *testing.T) {
				h := newMapHarness(t, "never read")
				machine := mapExampleMachine(t, mapOptions{width: width})
				runID := fmt.Sprintf("map-resume-%d", width)
				mapFixtureJournal(t, h.runsDir, runID, machine, apiv1.IntegrityDerived, boundary.tail)
				result, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
				if err != nil || result.Phase != journal.PhaseCompleted {
					t.Fatalf("Resume = %+v, %v", result, err)
				}
				if h.callCount("plan") != 0 {
					t.Fatal("resume re-ran the planner")
				}
				if got := h.callCount("run-shard"); got != boundary.items {
					t.Fatalf("resume ran %d item attempts, want %d (settled items never re-run)", got, boundary.items)
				}
				events := h.events(t, runID)
				singleEvent(t, events, journal.EventMapStarted)
				finished := singleEvent(t, events, journal.EventMapFinished)
				if got := mapCompletenessSummary(finished.Completeness); got != "1:unit-a:succeeded,2:unit-b:succeeded,3:unit-c:succeeded" {
					t.Fatalf("completeness = %s", got)
				}
				if joinRunsOf(events, "collate") != 1 {
					t.Fatalf("join ran %d times", joinRunsOf(events, "collate"))
				}
				_, results, _ := h.join(t)
				for i, result := range results {
					if result.Identity != mapExampleIdentities[i] || result.Outputs["shard"] != mapExampleIdentities[i] {
						t.Fatalf("mapResults[%d] = %+v, want %s's own result", i, result, mapExampleIdentities[i])
					}
				}
			})
		}
	}
}

// A map.finished that routed past the join (fail_fast onFailure) resumes to
// that route, not to the join.
func TestMapResumeAfterFailureRouteFollowsOnFailure(t *testing.T) {
	h := newMapHarness(t, "never read")
	machine := mapExampleMachine(t, mapOptions{width: 1, policy: apiv1.BranchFailFast, onFailure: "shards-failed"})
	const runID = "map-resume-failure-route"
	mapFixtureJournal(t, h.runsDir, runID, machine, apiv1.IntegrityDerived, func(items []journal.MapItem, _ *time.Time) []mapFixtureStep {
		steps := mapItemSettled(items, 1, journal.BranchFailed)
		completeness := []journal.BranchOutcome{}
		for id := 1; id <= 3; id++ {
			status := journal.BranchCancelled
			if id == 1 {
				status = journal.BranchFailed
			} else {
				steps = append(steps, mapFixtureStep{event: mapBranchEvent(items, journal.EventBranchFinished, id, journal.BranchCancelled)})
			}
			completeness = append(completeness, journal.BranchOutcome{Branch: id, Name: items[id-1].Identity, Status: status,
				MapItemIdentity: items[id-1].Identity, MapItemDigest: items[id-1].Digest})
		}
		return append(steps, mapFixtureStep{event: journal.Event{Type: journal.EventMapFinished, Parallel: "run-shards", Target: "shards-failed", Completeness: completeness}})
	})
	result, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	if h.callCount("shards-failed") != 1 || h.callCount("collate") != 0 || h.callCount("run-shard") != 0 {
		t.Fatalf("calls = %v, want only the onFailure route", h.calls)
	}
}

// An empty roster interrupted between map.started and map.finished resumes
// into the join exactly once.
func TestMapEmptyRosterResumeJoinsOnce(t *testing.T) {
	h := newMapHarness(t, "never read")
	machine := mapExampleMachine(t, mapOptions{width: 2})
	const runID = "map-empty-resume"
	mapFixtureJournalItems(t, h.runsDir, runID, machine, apiv1.IntegrityDerived, []journal.MapItem{}, nil)
	result, err := h.runner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	events := h.events(t, runID)
	if len(singleEvent(t, events, journal.EventMapFinished).Completeness) != 0 || joinRunsOf(events, "collate") != 1 {
		t.Fatal("an interrupted empty roster did not join exactly once")
	}
}

func TestMapAdmissionSupportedByBothRuntimes(t *testing.T) {
	machine := mapExampleMachine(t, mapOptions{})
	if err := workflow.RefuseUnsupportedFanOut(machine.Def.Spec); err != nil {
		t.Fatalf("engine admission refused a map: %v", err)
	}
	r := &Runner{cfg: Config{RunsDir: t.TempDir()}}
	if err := r.admitExecution(context.Background(), machine); err != nil {
		t.Fatalf("local admission refused a map: %v", err)
	}
}

// A roster that cannot be frozen fails the map with fanout_source_invalid
// before map.started and follows onFailure, or @abort when none is declared.
// It never runs as an empty map.
func TestMapInvalidRosterFailsBeforeMapStarted(t *testing.T) {
	duplicate := `{"schemaVersion":1,"items":[{"identity":"dup","item":{}},{"identity":"dup","item":{}}]}`
	for _, tc := range []struct {
		name  string
		opts  mapOptions
		phase journal.RunPhase
		route string
	}{
		{"abort", mapOptions{}, journal.PhaseAborted, ""},
		{"onFailure", mapOptions{policy: apiv1.BranchFailFast, onFailure: "shards-failed"}, journal.PhaseCompleted, "shards-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMapHarness(t, duplicate)
			runID := "map-invalid-" + tc.name
			if result := h.start(t, runID, mapExampleMachine(t, tc.opts)); result.Phase != tc.phase {
				t.Fatalf("phase = %q, want %q", result.Phase, tc.phase)
			}
			events := h.events(t, runID)
			if len(eventsOfType(events, journal.EventMapStarted)) != 0 || h.callCount("run-shard") != 0 || h.callCount("collate") != 0 {
				t.Fatal("an unfrozen roster fanned out or joined")
			}
			refused := false
			for _, event := range eventsOfType(events, journal.EventError) {
				refused = refused || (event.Parallel == "run-shards" && event.Error != nil && event.Error.Code == FanOutSourceInvalidCode)
			}
			if !refused {
				t.Fatal("no fanout_source_invalid failure recorded for the map")
			}
			if tc.route != "" && h.callCount(tc.route) != 1 {
				t.Fatalf("onFailure %s ran %d times", tc.route, h.callCount(tc.route))
			}
		})
	}
}

// A root consumer's invalid-handoff reroute may not re-enter the mapped task:
// it runs only for a frozen roster item, inside the map.
func TestInvalidHandoffRerouteRefusesMappedProducer(t *testing.T) {
	machine := mapExampleMachine(t, mapOptions{})
	if reason := invalidHandoffRerouteUnsupported(machine, "", nil, "run-shard"); !strings.Contains(reason, `map "run-shards"`) {
		t.Fatalf("reroute into the mapped task = %q, want a refusal naming the map", reason)
	}
	if reason := invalidHandoffRerouteUnsupported(machine, "", nil, "plan"); reason != "" {
		t.Fatalf("reroute into a root producer refused: %q", reason)
	}
}

// --- fixtures ---------------------------------------------------------------

type mapFixtureStep struct {
	at    time.Time
	event journal.Event
}

func mapFixtureItems(t *testing.T) []journal.MapItem {
	t.Helper()
	items := make([]journal.MapItem, 0, len(mapExampleIdentities))
	for _, identity := range mapExampleIdentities {
		item := json.RawMessage(`{"packages":["./internal/` + strings.TrimPrefix(identity, "unit-") + `/..."]}`)
		digest, err := fanout.ItemDigest(identity, item)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, journal.MapItem{Identity: identity, Digest: digest, Item: item})
	}
	return items
}

func mapBranchEvent(items []journal.MapItem, kind journal.EventType, id int, status journal.BranchStatus) journal.Event {
	item := items[id-1]
	event := journal.Event{Type: kind, Branch: id, Parallel: "run-shards", BranchName: item.Identity,
		MapItemIdentity: item.Identity, MapItemDigest: item.Digest, BranchStatus: status}
	if kind == journal.EventBranchStarted {
		event.Stage = "run-shard"
	}
	return event
}

// mapItemSettled is one item's complete record: started, its stage run, and
// settled with status.
func mapItemSettled(items []journal.MapItem, id int, status journal.BranchStatus) []mapFixtureStep {
	identity := items[id-1].Identity
	result := apiv1.ResultSuccess
	if status == journal.BranchFailed {
		result = apiv1.ResultFailure
	}
	return []mapFixtureStep{
		{event: mapBranchEvent(items, journal.EventBranchStarted, id, "")},
		{event: journal.Event{Type: journal.EventStageStarted, Branch: id, Stage: "run-shard", Attempt: 1}},
		{event: journal.Event{Type: journal.EventStageFinished, Branch: id, Stage: "run-shard", Attempt: 1, Status: string(result),
			Outputs: map[string]any{"shard": identity, "attempt": 1}, Integrity: apiv1.IntegrityDerived}},
		{event: mapBranchEvent(items, journal.EventBranchFinished, id, status)},
	}
}

// mapFixtureJournal writes an interrupted run of the §6.1 map: the planner
// finished (its artifacts deliberately unreadable, so any re-read fails), the
// three-item roster is frozen in map.started, and tail follows.
func mapFixtureJournal(t *testing.T, runsDir, runID string, machine *workflow.Machine, integrity apiv1.Integrity, tail func([]journal.MapItem, *time.Time) []mapFixtureStep) {
	t.Helper()
	mapFixtureJournalItems(t, runsDir, runID, machine, integrity, mapFixtureItems(t), tail)
}

func mapFixtureJournalItems(t *testing.T, runsDir, runID string, machine *workflow.Machine, integrity apiv1.Integrity, items []journal.MapItem, tail func([]journal.MapItem, *time.Time) []mapFixtureStep) {
	t.Helper()
	clock := time.Now()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "demo", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	def, _ := machine.Map("run-shards")
	jr.SetMachineState("run-shards")
	missing := journal.Ref{Path: "artifacts/sha256-missing", Digest: "sha256:" + strings.Repeat("0", 64), Size: 10, MediaType: "application/json", Integrity: integrity}
	steps := []mapFixtureStep{
		{event: journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1}},
		{event: journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: string(apiv1.ResultSuccess), Artifacts: []journal.Ref{missing, missing}}},
		{event: journal.Event{Type: journal.EventMapStarted, Parallel: "run-shards", MapRoster: &journal.MapRoster{
			Source:        journal.FanOutSourceRef{Task: "plan", Slot: "shards", Visit: 1, Attempt: 1, Digest: missing.Digest, Integrity: integrity},
			FailurePolicy: def.FailurePolicy, ItemTimeoutSeconds: def.ItemTimeoutSeconds, MaxConcurrent: fanout.EffectiveMaxConcurrent(def),
			Items: items,
		}}},
	}
	if tail != nil {
		steps = append(steps, tail(items, &clock)...)
	}
	for _, step := range steps {
		clock = time.Now()
		if !step.at.IsZero() {
			clock = step.at
		}
		if err := jr.Append(step.event); err != nil {
			t.Fatalf("append %s: %v", step.event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}
