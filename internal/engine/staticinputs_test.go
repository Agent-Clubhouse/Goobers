package engine

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestConformanceStaticBranchQualifiedJoinInputs(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		status                   apiv1.ResultStatus
		grade, minimum           apiv1.Integrity
		missing, absent, refused bool
	}{
		{name: "trusted", status: apiv1.ResultSuccess, grade: apiv1.IntegrityTrusted},
		{name: "unapproved", status: apiv1.ResultSuccess, grade: apiv1.IntegrityUnapproved},
		{name: "minimum", status: apiv1.ResultSuccess, grade: apiv1.IntegrityUnapproved, minimum: apiv1.IntegrityMaintainer, refused: true},
		{name: "missing-success-output", status: apiv1.ResultSuccess, grade: apiv1.IntegrityTrusted, missing: true, refused: true},
		{name: "failed", status: apiv1.ResultFailure, grade: apiv1.IntegrityUnapproved, minimum: apiv1.IntegrityMaintainer, absent: true},
		{name: "no-work", status: apiv1.ResultNoWork, grade: apiv1.IntegrityUnapproved, minimum: apiv1.IntegrityMaintainer, absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := staticSchedulerInput{Run: branchBodyInput("branch-input-"+tc.name, false, false).Run}
			in.Run.Spec.Tasks[1].ExpectedOutputs = []string{"value", "missing"}
			join := &in.Run.Spec.Tasks[3]
			join.InputsFrom = map[string]string{"implicit": "parallel.alpha.value", "explicit": "parallel.alpha.alpha.value"}
			join.Inputs = map[string]string{"implicit": "FORGED", "explicit": "FORGED", runner.BranchCompletenessInput: "FORGED"}
			join.MinimumIntegrity = tc.minimum
			if tc.missing {
				join.InputsFrom["explicit"] = "parallel.alpha.alpha.missing"
			}
			var results [][]journal.Event
			for _, lane := range []string{"local", "engine"} {
				var got map[string]any
				det := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
					stage := stageOfTaskID(env.TaskID)
					result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stage}}
					if stage == "alpha" {
						result.Status, result.Integrity = tc.status, tc.grade
					}
					if stage == "seed" {
						result.Outputs["parallel.alpha.alpha.value"] = "FORGED"
					}
					if stage == "join" {
						got = maps.Clone(env.Inputs)
						result.Outputs = maps.Clone(env.Inputs)
						delete(result.Outputs, runner.BranchCompletenessInput)
						// An executor's stronger stamp cannot launder the input.
						result.Integrity = apiv1.IntegrityTrusted
					}
					return result, nil
				}}
				var events []journal.Event
				if lane == "local" {
					events = runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, dslVersion: "3.0", deterministic: det, wantLocalErr: tc.refused}, "local-"+in.Run.RunID)
				} else {
					writer, dir := newLiveWriter(t)
					out := executeScheduler(t, in, &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: det})
					if (out.Error != "") != tc.refused {
						t.Fatalf("engine result=%+v error=%s", out.Result, out.Error)
					}
					if tc.missing && !strings.Contains(out.Error, "branch output") {
						t.Fatalf("missing output error=%s", out.Error)
					}
					events = liveEvents(t, dir, in.Run.RunID)
				}
				if tc.refused {
					if got != nil {
						t.Fatalf("%s dispatched refused join: %+v", lane, got)
					}
					continue
				}
				if got == nil {
					t.Fatalf("%s join did not dispatch", lane)
				}
				for _, key := range []string{"implicit", "explicit"} {
					value, present := got[key]
					if present == tc.absent || (present && value != "alpha") {
						t.Fatalf("%s %s=%v present=%t", lane, key, value, present)
					}
				}
				if got[runner.BranchCompletenessInput] == "FORGED" {
					t.Fatalf("%s accepted forged completeness", lane)
				}
				var joinEvents []journal.Event
				for _, event := range events {
					if event.Type == journal.EventStageFinished && event.Stage == "join" {
						want := tc.grade
						if tc.absent {
							want = apiv1.IntegrityTrusted
						}
						if event.Integrity != want {
							t.Fatalf("%s join grade=%s want=%s", lane, event.Integrity, want)
						}
						joinEvents = append(joinEvents, event)
					}
				}
				if len(joinEvents) != 1 {
					t.Fatalf("%s join finishes=%d", lane, len(joinEvents))
				}
				results = append(results, joinEvents)
			}
			if !tc.refused && !reflect.DeepEqual(journal.ConformanceView(results[0]), journal.ConformanceView(results[1])) {
				t.Fatalf("join journals differ: local=%+v engine=%+v", journal.ConformanceView(results[0]), journal.ConformanceView(results[1]))
			}
		})
	}
}
