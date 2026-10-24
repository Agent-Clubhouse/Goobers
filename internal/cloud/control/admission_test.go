package control

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type recordingAdmission struct {
	calls      *[]string
	prepareErr error
	stopErr    error
	deps       AdmissionDeps
}

type inertAdmission struct{}

func (inertAdmission) Prepare(context.Context) error { return nil }
func (inertAdmission) Tick(context.Context) error    { return nil }
func (inertAdmission) Stop(context.Context) error    { return nil }

func inertAdmissionFactory(context.Context, AdmissionDeps) (Admission, error) {
	return inertAdmission{}, nil
}

func (a *recordingAdmission) Prepare(context.Context) error {
	*a.calls = append(*a.calls, "admission.prepare")
	return a.prepareErr
}
func (a *recordingAdmission) Tick(context.Context) error {
	*a.calls = append(*a.calls, "admission.tick")
	return nil
}
func (a *recordingAdmission) Stop(context.Context) error {
	*a.calls = append(*a.calls, "admission.stop")
	return a.stopErr
}

func admissionRuntime(calls *[]string, admission *recordingAdmission, buildErr error) Runtime {
	step := func(name string) func(context.Context) error {
		return func(context.Context) error { *calls = append(*calls, name); return nil }
	}
	return Runtime{
		Configuration: &countedConfiguration{}, Pool: &pgxpool.Pool{}, InstanceID: "instance", Lease: &lease.Manager{},
		Temporal: struct{ client.Client }{}, TaskQueue: "cloud",
		Resolve: func(context.Context, triggerqueue.Record) (engine.RunInput, error) { return engine.RunInput{}, nil },
		Services: func(context.Context, *lease.Handle) (EpochServices, error) {
			return EpochServices{
				RebuildBeforeReap: step("rebuild"), ReconcileUncertain: step("uncertain"), ReattachAwaiters: step("attach"),
				L1: step("l1"), L2: step("l2"), L3: step("l3"), Stop: step("stop"),
				RestoreObservations: func(context.Context, []Observation) error { return nil },
				CaptureObservations: func(context.Context) ([]Observation, error) { return nil, nil },
			}, nil
		},
		Admission: func(_ context.Context, deps AdmissionDeps) (Admission, error) {
			if buildErr != nil {
				return nil, buildErr
			}
			admission.deps = deps
			return admission, nil
		},
	}
}

// Prepare runs after the awaiters are reattached (and so after uncertain
// starts), never before; Stop ends the admission along with the epoch's own
// stop, and a failing step is joined, not lost.
func TestAdmissionPreparesAfterAttachAndStopsWithTheEpoch(t *testing.T) {
	var calls []string
	admission := &recordingAdmission{calls: &calls, stopErr: errors.New("admission stop failed")}
	runtime := admissionRuntime(&calls, admission, nil)
	handle := &lease.Handle{InstanceID: "instance", Epoch: 7}
	services, err := runtime.Build(t.Context(), handle)
	if err != nil {
		t.Fatal(err)
	}
	if admission.deps.Handle != handle || admission.deps.Store == nil || admission.deps.Starter == nil || admission.deps.Resolve == nil {
		t.Fatalf("admission deps = %+v", admission.deps)
	}
	if err := services.ReattachAwaiters(t.Context()); err != nil {
		t.Fatal(err)
	}
	err = services.Stop(t.Context())
	if !errors.Is(err, admission.stopErr) {
		t.Fatalf("Stop = %v, want the admission's stop error joined", err)
	}
	want := []string{"attach", "admission.prepare", "stop", "admission.stop"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// A failing Prepare blocks serving: the host never marks the epoch ready.
func TestAdmissionPrepareFailureBlocksTheEpoch(t *testing.T) {
	var calls []string
	admission := &recordingAdmission{calls: &calls, prepareErr: errors.New("reconcile failed")}
	services, err := admissionRuntime(&calls, admission, nil).Build(t.Context(), &lease.Handle{InstanceID: "instance", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := services.ReattachAwaiters(t.Context()); !errors.Is(err, admission.prepareErr) {
		t.Fatalf("ReattachAwaiters = %v, want the prepare failure", err)
	}
}

func TestBuildRefusesWithoutAdmission(t *testing.T) {
	var calls []string
	runtime := admissionRuntime(&calls, &recordingAdmission{calls: &calls}, nil)
	runtime.Admission = nil
	if _, err := runtime.Build(t.Context(), &lease.Handle{InstanceID: "instance", Epoch: 1}); err == nil || !strings.Contains(err.Error(), "requires scheduler admission") {
		t.Fatalf("Build = %v, want scheduler admission refusal", err)
	}
}

func TestAdmissionBuildFailureFailsTheEpochBuild(t *testing.T) {
	var calls []string
	failure := errors.New("no scheduler")
	if _, err := admissionRuntime(&calls, &recordingAdmission{calls: &calls}, failure).Build(t.Context(), &lease.Handle{InstanceID: "instance", Epoch: 1}); !errors.Is(err, failure) {
		t.Fatalf("Build = %v, want the admission failure", err)
	}
}
