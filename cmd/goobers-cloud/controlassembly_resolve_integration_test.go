//go:build cloudintegration

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/startpin"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/cloud/startinput"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestControlResolveAcceptedPinnedInputAsControllerRole(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEST_POSTGRES_URL")
	instanceID, apiURL, controllerURL := controlRoleDatabase(t)
	controller, err := pgxpool.New(t.Context(), controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	var role string
	if err := controller.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("controller authenticates as %q: %v", role, err)
	}
	api, err := pgxpool.New(t.Context(), apiURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	root := t.TempDir()
	candidate := cloudconfig.Candidate{
		Digest: "sha256:resolver-fixture",
		Config: &instance.Config{
			Placement: &instance.PlacementConfig{SelfExecution: "deny"},
			Runners:   []instance.RunnerEntry{{Name: "worker", Host: "ghcr.io/example/worker:v1", Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Shell: true}}},
		},
		Set: &instance.ConfigSet{
			Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "goobers"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repo", Branch: "main"}}}},
			Workflows: []apiv1.Workflow{{ObjectMeta: metav1.ObjectMeta{Name: "implementation"}, DSLVersion: "3.0", Spec: apiv1.WorkflowSpec{
				Gaggle: "goobers", Start: "build", Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}}},
			}}},
		},
	}
	candidate.Pins, err = startpin.Compute(root, candidate.Config, candidate.Set)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := cloudconfig.NewConfiguration(
		configurationAssessor,
		&recordingActivator{}, func(context.Context) (cloudconfig.Candidate, error) { return candidate, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	acquired, released := 0, 0
	planes := controlPlanes{Configuration: configuration, generations: resolveGenerationSource(func(_ context.Context, digest string) (childworkflow.Generation, error) {
		if digest != candidate.Digest {
			t.Fatalf("acquired generation %q, want durable pin %q", digest, candidate.Digest)
		}
		acquired++
		return childworkflow.Generation{Directory: root, Set: candidate.Set, Release: func() error { released++; return nil }}, nil
	})}
	db := databaseServices{InstanceID: instanceID, Pools: &cloudpg.Pools{Transaction: controller, Direct: controller}}
	if err := constructResolve(t.Context(), db, controlAuth{}, nil, nil, &planes); err != nil {
		t.Fatal(err)
	}
	pin := candidate.Pins["goobers/implementation"]
	envelope := startintent.Envelope{Kind: startintent.Kind,
		Request: startintent.Request{Gaggle: "goobers", Workflow: "implementation"},
		Target:  startintent.Target{Gaggle: "goobers", Workflow: "implementation", ConfigGeneration: candidate.Digest, WorkflowDigest: pin.WorkflowDigest, GooberDigest: pin.GooberDigest},
	}
	payload, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	apiStore, err := acceptance.New(api, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	accepted, _, err := apiStore.Accept(t.Context(), "resolve-fixture", "operator", payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	store, err := acceptance.New(controller, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.ByRun(t.Context(), accepted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planes.Resolve(t.Context(), record); err == nil || !strings.Contains(err.Error(), "no instance config") || acquired != 0 {
		t.Fatalf("resolve before first reload = %v, acquisitions %d", err, acquired)
	}
	if err := configuration.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	input, err := planes.Resolve(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	if input.InstanceID != instanceID || input.RunID != accepted.RunID || input.Gaggle != "goobers" || input.WorkflowName != "implementation" ||
		input.ConfigGeneration != candidate.Digest || input.WorkflowDigest != pin.WorkflowDigest || input.GooberDigest != pin.GooberDigest || !input.LiveJournal || !input.SelfExecutionDenied {
		t.Fatalf("resolved identity or pins differ: %+v", input)
	}
	if input.Spec.Start == "" || input.RepoRef.Owner == "" || input.DSLVersion == "" {
		t.Fatalf("missing pinned definition: %+v", input)
	}
	// Reopening the durable row resolves the same immutable input.
	retry, err := planes.Resolve(t.Context(), record)
	if err != nil || !reflect.DeepEqual(input, retry) {
		t.Fatalf("retry changed pinned input: %v", err)
	}
	// A forged workflow pin must still fail in the composed path.
	envelope.Target.WorkflowDigest = "sha256:wrong"
	record.Payload, err = envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planes.Resolve(t.Context(), record); !errors.Is(err, startinput.ErrPinDrift) {
		t.Fatalf("pin drift = %v", err)
	}
	if acquired != 3 || released != acquired {
		t.Fatalf("generation acquisitions/releases = %d/%d, want 3/3", acquired, released)
	}
}
