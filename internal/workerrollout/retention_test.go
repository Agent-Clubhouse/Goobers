package workerrollout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/workerrouting"
)

type fakeStatusSource struct {
	status DeploymentStatus
	err    error
}

func (f fakeStatusSource) Status(context.Context) (DeploymentStatus, error) {
	return f.status, f.err
}

type fakeTemporal struct {
	routing workerrouting.Routing
	pages   map[string][][]Execution
	listErr error
	setErr  error
	set     []Execution
}

func (f *fakeTemporal) DescribeRouting(context.Context) (workerrouting.Routing, error) {
	return f.routing, nil
}

func (f *fakeTemporal) ListPinned(_ context.Context, build string, token []byte) ([]Execution, []byte, error) {
	if f.listErr != nil {
		return nil, nil, f.listErr
	}
	index := 0
	if len(token) != 0 {
		index = int(token[0])
	}
	pages := f.pages[build]
	if index >= len(pages) {
		return nil, nil, nil
	}
	var next []byte
	if index+1 < len(pages) {
		next = []byte{byte(index + 1)}
	}
	return pages[index], next, nil
}

func (f *fakeTemporal) SetAutoUpgrade(_ context.Context, execution Execution) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.set = append(f.set, execution)
	return nil
}

func blockedStatus() DeploymentStatus {
	return DeploymentStatus{
		TargetBuildID: "v4", CurrentBuildID: "v3", VersionCount: 3,
		Generation: 7, ObservedGeneration: 7,
		Deprecated: []DeprecatedVersion{
			{BuildID: "v2", Status: versionStatusDraining},
			{BuildID: "v1", Status: versionStatusDraining},
		},
	}
}

func TestRetentionUnblocksTargetWithOldestPinnedVersion(t *testing.T) {
	now := time.Now()
	temporal := &fakeTemporal{
		routing: workerrouting.Routing{Created: map[string]time.Time{
			"v1": now.Add(-3 * time.Hour),
			"v2": now.Add(-2 * time.Hour),
			"v3": now.Add(-time.Hour),
		}},
		pages: map[string][][]Execution{
			"v1": {
				{{WorkflowID: "run-1", RunID: "first"}},
				{{WorkflowID: "run-2", RunID: "second"}},
			},
		},
	}
	result, err := Reconcile(t.Context(), fakeStatusSource{status: blockedStatus()}, temporal, DefaultVersionLimit)
	if err != nil {
		t.Fatal(err)
	}
	if result.RetiredBuild != "v1" || result.Updated != 2 {
		t.Fatalf("result = %+v, want oldest build v1 and two updates", result)
	}
	if len(temporal.set) != 2 || temporal.set[0].WorkflowID != "run-1" || temporal.set[1].WorkflowID != "run-2" {
		t.Fatalf("AUTO_UPGRADE updates = %+v", temporal.set)
	}
}

func TestRetentionDoesNothingUntilControllerBlocksTarget(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*DeploymentStatus)
	}{
		{name: "below cap", mutate: func(s *DeploymentStatus) { s.VersionCount = 2 }},
		{name: "no target", mutate: func(s *DeploymentStatus) { s.TargetBuildID = "" }},
		{name: "target is current", mutate: func(s *DeploymentStatus) { s.TargetBuildID = s.CurrentBuildID }},
		{name: "target deployment exists", mutate: func(s *DeploymentStatus) { s.TargetHasDeployment = true }},
		{name: "controller has not observed spec", mutate: func(s *DeploymentStatus) { s.ObservedGeneration-- }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := blockedStatus()
			tc.mutate(&status)
			temporal := &fakeTemporal{}
			result, err := Reconcile(t.Context(), fakeStatusSource{status: status}, temporal, DefaultVersionLimit)
			if err != nil {
				t.Fatal(err)
			}
			if result != (Result{}) || len(temporal.set) != 0 {
				t.Fatalf("result = %+v, updates = %+v", result, temporal.set)
			}
		})
	}
}

func TestRetentionFailsClosedWithoutSafeOldestVersion(t *testing.T) {
	status := blockedStatus()
	status.Deprecated[0].EligibleForDeletion = true
	status.Deprecated[1].Status = "Current"
	_, err := Reconcile(t.Context(), fakeStatusSource{status: status}, &fakeTemporal{}, DefaultVersionLimit)
	if err == nil {
		t.Fatal("Reconcile accepted a blocked target without a safe draining predecessor")
	}
}

func TestRetentionSurfacesListAndUpdateFailures(t *testing.T) {
	now := time.Now()
	routing := workerrouting.Routing{Created: map[string]time.Time{"v1": now}}
	for _, tc := range []struct {
		name     string
		temporal *fakeTemporal
	}{
		{name: "list", temporal: &fakeTemporal{routing: routing, listErr: errors.New("visibility unavailable")}},
		{name: "update", temporal: &fakeTemporal{
			routing: routing,
			pages:   map[string][][]Execution{"v1": {{{WorkflowID: "run"}}}},
			setErr:  errors.New("update refused"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := blockedStatus()
			status.Deprecated = status.Deprecated[1:]
			if _, err := Reconcile(t.Context(), fakeStatusSource{status: status}, tc.temporal, DefaultVersionLimit); err == nil {
				t.Fatal("Reconcile hid a Temporal failure")
			}
		})
	}
}
