package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestStaticSchedulerMapBindsFrozenRosterAndJoinsInOrder(t *testing.T) {
	in, acts, calls := staticMapFixture(t, `{
		"schemaVersion":1,
		"items":[
			{"identity":"b","item":{"number":2}},
			{"identity":"a","item":{"number":9007199254740992}}
		]
	}`)
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("map result: %+v", out)
	}
	got := calls.snapshot()
	if len(got.items) != 2 || got.items["a"] != "9007199254740992" || got.items["b"] != "2" {
		t.Fatalf("mapped exact-number inputs = %+v", got.items)
	}
	if len(got.completeness) != 2 || len(got.results) != 2 {
		t.Fatalf("join completeness=%+v results=%+v", got.completeness, got.results)
	}
	for i, identity := range []string{"a", "b"} {
		if got.completeness[i].Branch != i+1 ||
			got.completeness[i].MapItemIdentity != identity ||
			got.results[i].Identity != identity ||
			got.results[i].Outputs["seen"] != identity {
			t.Fatalf("ordered join[%d] completeness=%+v result=%+v", i, got.completeness[i], got.results[i])
		}
	}

	var started, finished *journal.Event
	for _, op := range out.Projection.Ops {
		event := op.Event
		if event == nil {
			continue
		}
		switch event.Type {
		case journal.EventMapStarted:
			started = event
		case journal.EventMapFinished:
			finished = event
		}
	}
	if started == nil || started.MapRoster == nil || len(started.MapRoster.Items) != 2 ||
		started.MapRoster.Items[0].Identity != "a" ||
		len(started.MapRoster.Items[0].Item) != 0 ||
		started.MapRoster.RosterDigest == "" {
		t.Fatalf("map.started = %+v", started)
	}
	if finished == nil || len(finished.Completeness) != 2 ||
		finished.Completeness[0].MapItemIdentity != "a" ||
		finished.Completeness[1].MapItemIdentity != "b" {
		t.Fatalf("map.finished = %+v", finished)
	}
}

func TestStaticSchedulerMapEmptyRosterRunsJoin(t *testing.T) {
	in, acts, calls := staticMapFixture(t, `{"schemaVersion":1,"items":[]}`)
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("empty map result: %+v", out)
	}
	got := calls.snapshot()
	if len(got.items) != 0 || got.joinCalls != 1 ||
		got.completeness == nil || len(got.completeness) != 0 ||
		got.results == nil || len(got.results) != 0 {
		t.Fatalf("empty map calls = %+v", got)
	}
}

func TestStaticSchedulerMapSourceFailureRoutesOnFailure(t *testing.T) {
	in, acts, calls := staticMapFixture(t, `{"schemaVersion":1,"items":[]}`)
	in.Run.Spec.Tasks[0].ArtifactSlots[0].MaxSize = 1
	in.Run.Spec.Maps[0].FailurePolicy = apiv1.BranchFailFast
	in.Run.Spec.Maps[0].OnFailure = "join"
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("map source failure: %+v", out)
	}

	got := calls.snapshot()
	if len(got.items) != 0 || got.joinCalls != 1 {
		t.Fatalf("calls after source failure = %+v", got)
	}
	for _, op := range out.Projection.Ops {
		event := op.Event
		if event != nil && event.Type == journal.EventError && event.Parallel == "items" &&
			event.Error != nil && event.Error.Code == runner.FanOutSourceInvalidCode {
			return
		}
	}
	t.Fatalf("map source failure did not record fanout_source_invalid: %+v", out.Projection.Ops)
}

func TestStaticSchedulerMapRemoteBranchesUseCommittedOccurrenceIdentity(t *testing.T) {
	in, acts, _ := staticMapFixture(t, `{
		"schemaVersion":1,
		"items":[
			{"identity":"a","item":{"number":1}},
			{"identity":"b","item":{"number":2}}
		]
	}`)
	in.Run.Placements = []PinnedPlacement{{
		Stage: "alpha", Queue: dispatcher.QueueName("web", "win-ci"),
		Eligible: remoteEligible(),
	}}
	store := surrenderStore(t)
	for attempt := 1; attempt <= 2; attempt++ {
		putSurrendered(t, store, in.Run.RunID, "alpha", attempt, dispatcher.SurrenderedResult{
			Result: apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess, Outputs: map[string]any{"attempt": attempt},
				Integrity: apiv1.IntegrityMaintainer,
			},
		})
	}
	dispatched := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "win-ci", Pod: "mapped", Phase: corev1.PodSucceeded,
		SurrenderConfirmed: true, Disposed: true,
	}}
	acts.Dispatcher, acts.Surrenders = dispatched, store
	out := executeScheduler(t, in, acts)
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("remote map result: %+v", out)
	}
	attempts, _ := dispatched.recorded()
	if len(attempts) != 2 {
		t.Fatalf("remote map dispatches = %+v", attempts)
	}
	seenBranches, seenAttempts := map[int]bool{}, map[int]bool{}
	visit := attempts[0].FanOutVisit
	for _, attempt := range attempts {
		seenBranches[attempt.FanOutBranch] = true
		seenAttempts[attempt.PodAttempt] = true
		if visit == 0 || attempt.FanOutVisit != visit {
			t.Fatalf("remote map occurrence identity = %+v", attempts)
		}
	}
	if !seenBranches[1] || !seenBranches[2] || !seenAttempts[1] || !seenAttempts[2] {
		t.Fatalf("remote map identity = %+v", attempts)
	}
}

func TestStaticSchedulerMapEnforcesBindingAndJoinIntegrity(t *testing.T) {
	for _, target := range []string{"alpha", "join"} {
		t.Run(target, func(t *testing.T) {
			in, acts, calls := staticMapFixture(t, `{
				"schemaVersion":1,
				"items":[{"identity":"a","item":{"number":1}}]
			}`)
			taskByName(t, in.Run.Spec.Tasks, target).MinimumIntegrity = apiv1.IntegrityTrusted
			out := executeScheduler(t, in, acts)
			if out.Error == "" {
				t.Fatalf("%s admitted maintainer-grade map data: %+v", target, out)
			}
			got := calls.snapshot()
			if target == "alpha" && len(got.items) != 0 {
				t.Fatalf("mapped task ran before binding integrity refusal: %+v", got)
			}
			if target == "join" && got.joinCalls != 0 {
				t.Fatalf("join ran before map result integrity refusal: %+v", got)
			}
		})
	}
}

type staticMapCalls struct {
	mu           sync.Mutex
	items        map[string]string
	joinCalls    int
	completeness []journal.BranchOutcome
	results      []runner.MapItemResult
}

func (c *staticMapCalls) snapshot() staticMapCalls {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := staticMapCalls{
		items:        make(map[string]string, len(c.items)),
		joinCalls:    c.joinCalls,
		completeness: make([]journal.BranchOutcome, len(c.completeness)),
		results:      make([]runner.MapItemResult, len(c.results)),
	}
	copy(out.completeness, c.completeness)
	copy(out.results, c.results)
	for identity, number := range c.items {
		out.items[identity] = number
	}
	return out
}

func staticMapFixture(
	t *testing.T,
	roster string,
) (staticSchedulerInput, *Activities, *staticMapCalls) {
	return staticMapFixtureWith(t, roster, apiv1.IntegrityMaintainer, nil)
}

func staticMapFixtureWith(
	t *testing.T,
	roster string,
	integrity apiv1.Integrity,
	statuses map[string]apiv1.ResultStatus,
) (staticSchedulerInput, *Activities, *staticMapCalls) {
	t.Helper()
	in := schedulerInput(strings.ReplaceAll(t.Name(), "/", "-"), 2)
	in.Run.DSLVersion = "3.1"
	in.Run.Spec.Parallels = nil
	seed := taskByName(t, in.Run.Spec.Tasks, "seed")
	mapped := taskByName(t, in.Run.Spec.Tasks, "alpha")
	join := taskByName(t, in.Run.Spec.Tasks, "join")
	seed.Next, mapped.Next = "items", wf.TargetJoin
	in.Run.Spec.Tasks = []apiv1.Task{*seed, *mapped, *join}
	in.Run.Spec.Maps = []apiv1.Map{{
		Name: "items", Source: apiv1.FanOutSource{Task: "seed", Slot: "roster"},
		Task: "alpha",
		Bindings: map[string]apiv1.MapBinding{
			"item": apiv1.MapBindingItem, "identity": apiv1.MapBindingIdentity,
			"digest": apiv1.MapBindingDigest,
		},
		FailurePolicy: apiv1.BranchContinueOnError, Join: "join", MaxConcurrent: 2,
	}}
	source := []byte(roster)
	seed = &in.Run.Spec.Tasks[0]
	seed.ArtifactSlots = []apiv1.ArtifactSlot{{
		Name: "roster", MediaType: fanout.SourceMediaType, MaxSize: fanout.MaxSourceBytes,
	}}
	pointer := apiv1.ArtifactPointer{
		Path: "artifacts/roster.json", Digest: journal.Digest(source),
		MediaType: fanout.SourceMediaType, Size: int64(len(source)),
		Integrity: integrity,
	}
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), pointer.Digest, source); err != nil {
		t.Fatal(err)
	}
	writer, _ := newLiveWriter(t)
	calls := &staticMapCalls{items: map[string]string{}}
	acts := &Activities{
		Journal: writer, Workspaces: testWorkspaces(t), FanOutBlobs: store,
		Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			stage := stageOfTaskID(env.TaskID)
			switch stage {
			case "seed":
				return apiv1.ResultEnvelope{
					Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "seed"},
					Artifacts: []apiv1.ArtifactPointer{pointer},
				}, nil
			case "alpha":
				item, ok := env.Inputs["item"].(map[string]any)
				if !ok {
					t.Fatalf("mapped item = %#v", env.Inputs["item"])
				}
				number, ok := item["number"].(json.Number)
				if !ok {
					t.Fatalf("mapped exact number = %#v", item["number"])
				}
				identity, _ := env.Inputs["identity"].(string)
				if env.Inputs["digest"] == "" {
					t.Fatal("mapped digest is empty")
				}
				calls.mu.Lock()
				calls.items[identity] = number.String()
				calls.mu.Unlock()
				status := statuses[identity]
				if status == "" {
					status = apiv1.ResultSuccess
				}
				var failure *apiv1.ErrorInfo
				if status == apiv1.ResultFailure {
					failure = &apiv1.ErrorInfo{Code: "mapped_failed", Message: identity + " failed"}
				}
				return apiv1.ResultEnvelope{
					Status: status, Outputs: map[string]any{"seen": identity},
					Error: failure, Integrity: integrity,
				}, nil
			case "join":
				data, err := json.Marshal(env.Inputs[runner.MapCompletenessInput])
				if err != nil {
					return apiv1.ResultEnvelope{}, err
				}
				var completeness []journal.BranchOutcome
				if err := json.Unmarshal(data, &completeness); err != nil {
					return apiv1.ResultEnvelope{}, err
				}
				data, err = json.Marshal(env.Inputs[runner.MapResultsInput])
				if err != nil {
					return apiv1.ResultEnvelope{}, err
				}
				var results []runner.MapItemResult
				if err := json.Unmarshal(data, &results); err != nil {
					return apiv1.ResultEnvelope{}, err
				}
				calls.mu.Lock()
				calls.joinCalls++
				calls.completeness, calls.results = completeness, results
				calls.mu.Unlock()
			}
			return apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stage},
			}, nil
		}},
	}
	return in, acts, calls
}

func taskByName(t *testing.T, tasks []apiv1.Task, name string) *apiv1.Task {
	t.Helper()
	for i := range tasks {
		if tasks[i].Name == name {
			return &tasks[i]
		}
	}
	t.Fatalf("task %q not found", name)
	return nil
}
