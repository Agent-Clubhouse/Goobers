package harness

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	fake := &FakeAdapter{AdapterName: "fake"}
	if err := reg.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := reg.Get("fake")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name() != "fake" {
		t.Fatalf("Get returned adapter named %q", got.Name())
	}
	if _, err := reg.Get("missing"); err == nil {
		t.Fatal("expected an error for an unregistered name")
	}
}

func TestRegistryRejectsDuplicateName(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(&FakeAdapter{AdapterName: "dup"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Register(&FakeAdapter{AdapterName: "dup"}); err == nil {
		t.Fatal("expected an error registering a duplicate name")
	}
}

// thirdAdapter simulates adding a brand-new harness (e.g. "claude-code") to
// prove GBO-051's swappability claim: a new Adapter implementation, wired
// through the same Registry + Executor, with zero changes to either type.
type thirdAdapter struct{ ran bool }

func (a *thirdAdapter) Name() string                        { return "third-harness" }
func (a *thirdAdapter) Preflight(ctx context.Context) error { return nil }
func (a *thirdAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	a.ran = true
	if err := WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
		return Outcome{}, err
	}
	payload, err := readCompletion(req.Workspace, req.CompletionPath)
	return Outcome{Payload: payload}, err
}

// TestRegistrySwapRequiresNoExecutorChange proves swappability, not just
// asserts it (issue #19 acceptance criterion / GBO-051): fake, a second fake
// under a different name, and a brand-new third adapter type all load through
// the same Registry and drive the same Executor/Invoke call unchanged.
func TestRegistrySwapRequiresNoExecutorChange(t *testing.T) {
	reg := NewRegistry()
	writeSuccess := func(ctx context.Context, req RunRequest) error {
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}
	adapters := []Adapter{
		&FakeAdapter{AdapterName: "fake", Act: writeSuccess},
		&FakeAdapter{AdapterName: "fake-2", Act: writeSuccess},
		&thirdAdapter{},
	}
	for _, a := range adapters {
		if err := reg.Register(a); err != nil {
			t.Fatalf("Register(%s): %v", a.Name(), err)
		}
	}

	for _, name := range []string{"fake", "fake-2", "third-harness"} {
		t.Run(name, func(t *testing.T) {
			adapter, err := reg.Get(name)
			if err != nil {
				t.Fatalf("Get(%s): %v", name, err)
			}
			injector := testInjector(t, "", "", noopRegistrar{})
			rec := &fakeRecorder{}
			exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
			if err != nil {
				t.Fatalf("NewExecutor: %v", err)
			}
			result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
			if err != nil {
				t.Fatalf("Invoke via %s: %v", name, err)
			}
			if result.Status != apiv1.ResultSuccess {
				t.Fatalf("Status = %q, want success", result.Status)
			}
		})
	}
}
