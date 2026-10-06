package gagglehealth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type snapshotSourceFunc func(context.Context, string, []EvidenceDependency) (Snapshot, error)

func (f snapshotSourceFunc) Snapshot(ctx context.Context, gaggle string, dependencies []EvidenceDependency) (Snapshot, error) {
	return f(ctx, gaggle, dependencies)
}

type detectorFunc struct {
	name   string
	budget time.Duration
	run    func(context.Context, Snapshot, apiv1.GaggleHealthPolicy) ([]Observation, error)
}

func (d detectorFunc) Name() string                       { return d.name }
func (d detectorFunc) Dependencies() []EvidenceDependency { return []EvidenceDependency{EvidenceRuns} }
func (d detectorFunc) Budget() time.Duration              { return d.budget }
func (d detectorFunc) Evaluate(ctx context.Context, snapshot Snapshot, policy apiv1.GaggleHealthPolicy) ([]Observation, error) {
	return d.run(ctx, snapshot, policy)
}

func TestControllerEvaluatesMultipleGagglesAndIsolatesFailure(t *testing.T) {
	store := controllerStore(t)
	var alpha, beta atomic.Int32
	source := snapshotSourceFunc(func(_ context.Context, gaggle string, _ []EvidenceDependency) (Snapshot, error) {
		if gaggle == "broken" {
			return Snapshot{}, errors.New("broken snapshot")
		}
		return Snapshot{}, nil
	})
	controller, err := NewController(store, source, ControllerOptions{MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	policy := fastPolicy(t)
	counting := func(counter *atomic.Int32) detectorFunc {
		return detectorFunc{name: "count", run: func(context.Context, Snapshot, apiv1.GaggleHealthPolicy) ([]Observation, error) {
			counter.Add(1)
			return nil, nil
		}}
	}
	if err := controller.Start(context.Background(), []GaggleRegistration{
		{Name: "alpha", Policy: policy, Detectors: []Detector{counting(&alpha)}},
		{Name: "beta", Policy: policy, Detectors: []Detector{counting(&beta)}},
		{Name: "broken", Policy: policy},
	}); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	eventually(t, func() bool {
		broken, ok := controller.Status("broken")
		return alpha.Load() > 0 && beta.Load() > 0 && ok && broken.LastError != ""
	})
	broken, err := store.Snapshot("broken")
	if err != nil {
		t.Fatal(err)
	}
	if len(broken.Active) != 1 || broken.Active[0].Code != FindingControllerDegraded {
		t.Fatalf("broken gaggle findings = %+v", broken.Active)
	}
}

func TestControllerCoalescesWakeupsAndBoundsConcurrency(t *testing.T) {
	store := controllerStore(t)
	release := make(chan struct{})
	started := make(chan string, 4)
	var active, maximum atomic.Int32
	source := snapshotSourceFunc(func(ctx context.Context, gaggle string, _ []EvidenceDependency) (Snapshot, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- gaggle
		select {
		case <-release:
			return Snapshot{}, nil
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	})
	controller, err := NewController(store, source, ControllerOptions{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	policy := fastPolicy(t)
	if err := controller.Start(context.Background(), []GaggleRegistration{{Name: "alpha", Policy: policy}, {Name: "beta", Policy: policy}}); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	<-started
	for range 100 {
		controller.Wake("alpha")
	}
	close(release)
	eventually(t, func() bool {
		alpha, _ := controller.Status("alpha")
		beta, _ := controller.Status("beta")
		return !alpha.LastSuccessfulEvaluation.IsZero() && !beta.LastSuccessfulEvaluation.IsZero()
	})
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent evaluations = %d, want 1", maximum.Load())
	}
}

func TestControllerSnapshotDeadlineReleasesCapacityForOtherGaggles(t *testing.T) {
	store := controllerStore(t)
	started := make(chan string, 2)
	block := make(chan struct{})
	var blockedCalls, healthyCalls atomic.Int32
	source := snapshotSourceFunc(func(_ context.Context, gaggle string, _ []EvidenceDependency) (Snapshot, error) {
		if gaggle == "healthy" {
			healthyCalls.Add(1)
			return Snapshot{}, nil
		}
		blockedCalls.Add(1)
		started <- gaggle
		<-block
		return Snapshot{}, nil
	})
	controller, err := NewController(store, source, ControllerOptions{
		MaxConcurrent:    2,
		EvaluationBudget: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background(), []GaggleRegistration{
		{Name: "blocked-alpha", Policy: fastPolicy(t)},
		{Name: "blocked-beta", Policy: fastPolicy(t)},
		{Name: "healthy", Policy: fastPolicy(t)},
	}); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	defer close(block)

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case gaggle := <-started:
			seen[gaggle] = true
		case <-deadline:
			t.Fatal("blocked snapshots did not occupy all evaluation slots")
		}
	}
	healthyBeforeDeadline := healthyCalls.Load()
	controller.Wake("healthy")
	eventually(t, func() bool {
		for _, gaggle := range []string{"blocked-alpha", "blocked-beta"} {
			snapshot, snapshotErr := store.Snapshot(gaggle)
			status, ok := controller.Status(gaggle)
			if snapshotErr != nil || !ok || len(snapshot.Active) != 1 || status.LastError == "" {
				return false
			}
		}
		healthy, ok := controller.Status("healthy")
		return healthyCalls.Load() > healthyBeforeDeadline && ok && !healthy.LastSuccessfulEvaluation.IsZero()
	})
	for range 100 {
		controller.Wake("blocked-alpha")
		controller.Wake("blocked-beta")
	}
	time.Sleep(50 * time.Millisecond)
	if blockedCalls.Load() != 2 {
		t.Fatalf("uncooperative snapshot calls = %d, want 2", blockedCalls.Load())
	}
}

func TestControllerDetectorTimeoutCancellationAndRestartDedupe(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, fixedRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	source := snapshotSourceFunc(func(context.Context, string, []EvidenceDependency) (Snapshot, error) {
		return Snapshot{}, nil
	})
	timedOut := detectorFunc{name: "blocked", budget: 20 * time.Millisecond, run: func(ctx context.Context, _ Snapshot, _ apiv1.GaggleHealthPolicy) ([]Observation, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	controller, err := NewController(store, source, ControllerOptions{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background(), []GaggleRegistration{{Name: "alpha", Policy: fastPolicy(t), Detectors: []Detector{timedOut}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		status, _ := controller.Status("alpha")
		return status.LastError != ""
	})
	controller.Stop()
	before, err := store.Snapshot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Active) != 1 {
		t.Fatalf("active findings before restart = %d, want 1", len(before.Active))
	}
	key := before.Active[0].EpisodeKey
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(root, fixedRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	after, err := store.Snapshot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Active) != 1 || after.Active[0].EpisodeKey != key {
		t.Fatalf("restart changed episode identity: before=%s after=%+v", key, after.Active)
	}
}

func TestControllerResolvesSnapshotFailureAfterRecovery(t *testing.T) {
	store := controllerStore(t)
	var fail atomic.Bool
	fail.Store(true)
	source := snapshotSourceFunc(func(context.Context, string, []EvidenceDependency) (Snapshot, error) {
		if fail.Load() {
			return Snapshot{}, errors.New("snapshot unavailable")
		}
		return Snapshot{}, nil
	})
	controller, err := NewController(store, source, ControllerOptions{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background(), []GaggleRegistration{{Name: "alpha", Policy: fastPolicy(t)}}); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	eventually(t, func() bool {
		snapshot, snapshotErr := store.Snapshot("alpha")
		return snapshotErr == nil && len(snapshot.Active) == 1
	})
	fail.Store(false)
	controller.Wake("alpha")
	eventually(t, func() bool {
		snapshot, snapshotErr := store.Snapshot("alpha")
		return snapshotErr == nil && len(snapshot.Active) == 0 && len(snapshot.History) == 1
	})
}

func TestControllerMalformedObservationDegradesUntilRecovery(t *testing.T) {
	store := controllerStore(t)
	var malformed atomic.Bool
	malformed.Store(true)
	detector := detectorFunc{name: "malformed", run: func(context.Context, Snapshot, apiv1.GaggleHealthPolicy) ([]Observation, error) {
		if malformed.Load() {
			return []Observation{{Status: ObservationHealthy}}, nil
		}
		return nil, nil
	}}
	source := snapshotSourceFunc(func(context.Context, string, []EvidenceDependency) (Snapshot, error) {
		return Snapshot{}, nil
	})
	controller, err := NewController(store, source, ControllerOptions{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background(), []GaggleRegistration{{
		Name: "alpha", Policy: fastPolicy(t), Detectors: []Detector{detector},
	}}); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	eventually(t, func() bool {
		snapshot, snapshotErr := store.Snapshot("alpha")
		status, _ := controller.Status("alpha")
		return snapshotErr == nil && len(snapshot.Active) == 1 && status.LastError != ""
	})
	malformed.Store(false)
	controller.Wake("alpha")
	eventually(t, func() bool {
		snapshot, snapshotErr := store.Snapshot("alpha")
		status, _ := controller.Status("alpha")
		return snapshotErr == nil && len(snapshot.Active) == 0 && status.LastError == ""
	})
}

func TestControllerBoundsUncooperativeDetectorAndDisablesCleanly(t *testing.T) {
	store := controllerStore(t)
	block := make(chan struct{})
	var calls atomic.Int32
	detector := detectorFunc{name: "uncooperative", budget: 20 * time.Millisecond, run: func(context.Context, Snapshot, apiv1.GaggleHealthPolicy) ([]Observation, error) {
		calls.Add(1)
		<-block
		return nil, nil
	}}
	source := snapshotSourceFunc(func(context.Context, string, []EvidenceDependency) (Snapshot, error) { return Snapshot{}, nil })
	controller, err := NewController(store, source, ControllerOptions{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	policy := fastPolicy(t)
	if err := controller.Start(context.Background(), []GaggleRegistration{{Name: "alpha", Policy: policy, Detectors: []Detector{detector}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		status, _ := controller.Status("alpha")
		return status.LastError != ""
	})
	for range 100 {
		controller.Wake("alpha")
	}
	for range 20 {
		if err := controller.Reload([]GaggleRegistration{{Name: "alpha", Policy: policy, Detectors: []Detector{detector}}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("uncooperative detector calls = %d, want 1", calls.Load())
	}
	disabled := false
	policy.Enabled = &disabled
	if err := controller.Reload([]GaggleRegistration{{Name: "alpha", Policy: policy, Detectors: []Detector{detector}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := controller.Status("alpha"); ok {
		t.Fatal("disabled gaggle retained controller status")
	}
	controller.Stop()
	close(block)
}

func TestControllerReloadRetiresAndReplacesAtomically(t *testing.T) {
	store := controllerStore(t)
	var oldCalls, newCalls atomic.Int32
	blockOld := make(chan struct{})
	var once sync.Once
	oldDetector := detectorFunc{name: "old", run: func(context.Context, Snapshot, apiv1.GaggleHealthPolicy) ([]Observation, error) {
		oldCalls.Add(1)
		once.Do(func() { <-blockOld })
		return []Observation{controllerFinding(t, "alpha", "old-policy")}, nil
	}}
	newDetector := detectorFunc{name: "new", run: func(context.Context, Snapshot, apiv1.GaggleHealthPolicy) ([]Observation, error) {
		newCalls.Add(1)
		return nil, nil
	}}
	source := snapshotSourceFunc(func(context.Context, string, []EvidenceDependency) (Snapshot, error) { return Snapshot{}, nil })
	controller, err := NewController(store, source, ControllerOptions{MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	policy := fastPolicy(t)
	if err := controller.Start(context.Background(), []GaggleRegistration{{Name: "alpha", Policy: policy, Detectors: []Detector{oldDetector}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return oldCalls.Load() == 1 })
	if err := controller.Reload([]GaggleRegistration{{Name: "alpha", Policy: policy, Detectors: []Detector{newDetector}}, {Name: "beta", Policy: policy}}); err != nil {
		t.Fatal(err)
	}
	close(blockOld)
	eventually(t, func() bool {
		_, beta := controller.Status("beta")
		return newCalls.Load() > 0 && beta
	})
	snapshot, err := store.Snapshot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Active) != 0 {
		t.Fatalf("retired generation published findings: %+v", snapshot.Active)
	}
	controller.Stop()
}

func controllerFinding(t *testing.T, gaggle, worker string) Observation {
	t.Helper()
	identity := apiv1.GaggleHealthIdentity{Gaggle: gaggle, Worker: worker}
	key, err := EpisodeKey(FindingControllerDegraded, identity)
	if err != nil {
		t.Fatal(err)
	}
	return Observation{Status: ObservationFinding, Finding: &apiv1.GaggleHealthFinding{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		Code:          FindingControllerDegraded,
		Severity:      apiv1.GaggleHealthSeverityError,
		Contribution:  apiv1.GaggleHealthDegraded,
		Identity:      identity,
		EpisodeKey:    key,
		Summary:       "old policy finding",
		Confidence:    1,
		Repair: apiv1.GaggleHealthRepair{
			Disposition: apiv1.GaggleHealthRepairNotAttempted,
			FollowUp:    apiv1.GaggleHealthFollowUpNone,
		},
	}}
}

func controllerStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(t.TempDir(), fixedRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func fastPolicy(t *testing.T) apiv1.GaggleHealthPolicy {
	t.Helper()
	policy := DefaultPolicy()
	policy.EvaluationInterval = "30s"
	policy.Thresholds.TriggerSilence = "30s"
	policy.Thresholds.NoProgress = "30s"
	if err := ValidatePolicy(policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}
