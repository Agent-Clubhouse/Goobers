package readservice

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/version"
)

// rootlessSources are a rootless service's required sources, kept outside the
// working directory the service runs in.
func rootlessSources(t *testing.T) LocalSources {
	t.Helper()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	definitions := testDefinitions()
	definitions.Gaggles = []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}
	return LocalSources{Rootless: true, Definitions: definitions, ReadModel: store,
		Journal: fileJournalReader(instance.NewLayout(t.TempDir()))}
}

// fileTree records every path under root with its size and modification time.
func fileTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		tree[path] = fmt.Sprint(info.ModTime().UnixNano(), info.Mode(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// plantLocalTraps writes, into dir, every file a rooted service would read
// from its instance root, each claiming something a rootless service must
// never report.
func plantLocalTraps(t *testing.T, dir string) {
	t.Helper()
	layout := instance.NewLayout(dir)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("trap-item", "trap-run", "trap-workflow", time.Hour); err != nil || !ok {
		t.Fatalf("plant claim: %v %v", ok, err)
	}
	if err := selfupdate.WriteCheck(dir, selfupdate.CheckResult{UpdateAvailable: true, LatestVersion: "v99.0.0",
		CurrentVersion: version.Get().Version, CheckedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, trap := range []string{layout.TelemetryDB(), filepath.Join(layout.SchedulerDir(), "backprop-audit", "state.json"),
		filepath.Join(dir, "config", "gaggles", "web", ".template"), filepath.Join(dir, instance.RootIdentityFileName)} {
		if err := os.MkdirAll(filepath.Dir(trap), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(trap, []byte("trap"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A rootless service run from a working directory that holds an instance's
// files neither reads nor writes any of them: every answer that only a local
// root could give is ErrUnavailable (503) or omitted by design.
func TestRootlessServiceNeverTouchesTheWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	plantLocalTraps(t, cwd)
	t.Chdir(cwd)
	before := fileTree(t, cwd)
	service, err := NewLocal(rootlessSources(t), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := service.ActiveClaims(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("active claims = %v, want unavailable (the trap claim must not be read)", err)
	}
	runs := []RunSummary{{ID: "trap-run"}}
	if err := service.decorateOperatorClaims(ctx, runs, time.Now()); err != nil || runs[0].Operator.Claim.LeaseStatus != "unavailable" {
		t.Fatalf("run claim decoration = %q (%v), want unavailable", runs[0].Operator.Claim.LeaseStatus, err)
	}
	health, err := service.Health(ctx)
	if err != nil || health.Update != nil {
		t.Fatalf("health = %+v (%v), want no update-check verdict", health.Update, err)
	}
	inventory, err := service.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.InstanceRoot != "" || inventory.RootIdentity == nil || inventory.RootIdentity.ID != "" || inventory.RootIdentity.IdentityProblem == "" {
		t.Fatalf("root identity = %+v at %q, want an explicit rootless problem", inventory.RootIdentity, inventory.InstanceRoot)
	}
	if inventory.Fleet == nil || inventory.Fleet.Associated || inventory.Fleet.Reason == "" {
		t.Fatalf("fleet = %+v, want an explicit rootless reason", inventory.Fleet)
	}
	gaggles, err := service.Gaggles(ctx, PageRequest{})
	if err != nil || len(gaggles.Items) != 1 || gaggles.Items[0].Template != nil {
		t.Fatalf("gaggles = %+v (%v), want the trap template unread", gaggles, err)
	}
	if _, err := service.StageAttempts(ctx, "trap-run", "stage"); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("stage attempts = %v", err)
	}
	if _, err := service.RunSpans(ctx, "trap-run"); !errors.Is(err, ErrTelemetryUnavailable) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("run spans = %v, want telemetry unavailable", err)
	}
	if _, err := service.TelemetryStats(ctx, TelemetryStatsRequest{}); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("telemetry stats = %v, want telemetry unavailable", err)
	}
	if _, err := service.TelemetryAttribution(ctx, TelemetryAttributionRequest{}); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("telemetry attribution = %v, want telemetry unavailable", err)
	}
	// Every remaining read path, for its file-system side effects.
	_, _ = service.PortalConfig(ctx)
	_, _ = service.ListRuns(ctx, RunListOptions{})
	_, _ = service.GetRun(ctx, "trap-run")
	_, _ = service.RunEvents(ctx, "trap-run")
	_, _ = service.RunChildren(ctx, "trap-run", "")
	_, _ = service.Goobers(ctx, "web", PageRequest{})
	_, _ = service.Workflows(ctx, "web", PageRequest{})
	_, _ = service.Connections(ctx, "web")
	_, _ = service.ListStatusRuns(ctx, StatusRunOptions{})
	_, _ = service.SchedulerStatus(ctx)
	_, _ = service.TimeToFirstPR(ctx)
	_, _ = service.TelemetryCosts(ctx, TelemetryCostRequest{})
	_, _ = service.TelemetryErrorSignatures(ctx, TelemetryErrorSignaturesRequest{})
	_, _ = service.TelemetryErrors(ctx, TelemetryErrorsRequest{})
	_, _ = service.TelemetryImplementationOutcomes(ctx, TelemetryImplementationOutcomesRequest{})
	_, _ = service.AddressableAgents(ctx, "trap-run")
	_, _ = service.QueueEligibility(ctx, "web", "implement")
	_, _ = service.Workflow(ctx, "web", "implement")

	if after := fileTree(t, cwd); len(after) != len(before) {
		t.Fatalf("rootless reads changed the working directory: %d paths before, %d after", len(before), len(after))
	} else {
		for path, stamp := range before {
			if after[path] != stamp {
				t.Fatalf("rootless reads touched %s", path)
			}
		}
	}
}

func TestRootlessClaimSnapshotFeedsEveryClaimView(t *testing.T) {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("item-1", "run-1", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	sources := rootlessSources(t)
	sources.ClaimSnapshot = func(context.Context) (ClaimView, error) { return ledger, nil }
	service, err := NewLocal(sources, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	active, err := service.ActiveClaims(context.Background())
	if err != nil || len(active.Claims) != 1 || active.Claims[0].ItemID != "item-1" {
		t.Fatalf("active claims = %+v (%v), want the snapshot's claim", active, err)
	}
	runs := []RunSummary{{ID: "run-1"}}
	if err := service.decorateOperatorClaims(context.Background(), runs, time.Now()); err != nil || runs[0].Operator.Claim.LeaseStatus != "active" {
		t.Fatalf("run claim = %q (%v), want active from the snapshot", runs[0].Operator.Claim.LeaseStatus, err)
	}
}

func TestRootlessServiceRequiresItsSourcesAndNoLocalOnes(t *testing.T) {
	for name, mutate := range map[string]func(*LocalSources){
		"no journal":       func(s *LocalSources) { s.Journal = nil },
		"no read model":    func(s *LocalSources) { s.ReadModel = nil },
		"an instance root": func(s *LocalSources) { s.Layout = instance.NewLayout(t.TempDir()) },
		"a telemetry store": func(s *LocalSources) {
			db, err := rollup.Open(filepath.Join(t.TempDir(), "telemetry.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			s.Telemetry = db
		},
	} {
		t.Run(name, func(t *testing.T) {
			sources := rootlessSources(t)
			mutate(&sources)
			if _, err := NewLocal(sources, func() bool { return true }); err == nil {
				t.Fatal("rootless service accepted")
			}
		})
	}
}

// rootlessSweepSkips are lifecycle hooks, not reads: they start or wire
// background work and are exercised by their own tests.
var rootlessSweepSkips = map[string]bool{
	"StartActiveRunSampler": true, "StartSchedulerStateProjector": true, "WaitForInitialActiveRunSample": true,
	"ReloadDefinitions": true, "PublishDefinitionReload": true,
	"AttachProjectionHealth": true, "AttachIntakeDepth": true, "AttachStartupStatus": true,
}

// Every exported method of a rootless service, run from a working directory
// no one may read (mode 000): any path resolved against it fails with
// EACCES, so a single relative read or write anywhere fails the sweep.
func TestRootlessServiceSweepsEveryMethodWithAnUnreadableWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	plantLocalTraps(t, cwd)
	t.Chdir(cwd)
	if err := os.Chmod(cwd, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cwd, 0o700) })
	service, err := NewLocal(rootlessSources(t), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	ctxType := reflect.TypeFor[context.Context]()
	errType := reflect.TypeFor[error]()
	value := reflect.ValueOf(service)
	swept := 0
	for i := range value.NumMethod() {
		method := value.Type().Method(i)
		if rootlessSweepSkips[method.Name] {
			continue
		}
		fn := value.Method(i)
		args := make([]reflect.Value, fn.Type().NumIn())
		for a := range args {
			in := fn.Type().In(a)
			switch {
			case in == ctxType:
				args[a] = reflect.ValueOf(context.Background())
			case in.Kind() == reflect.String:
				args[a] = reflect.ValueOf("0123456789abcdef0123456789abcdef").Convert(in)
			default:
				args[a] = reflect.Zero(in)
			}
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("%s panicked: %v", method.Name, recovered)
				}
			}()
			results := fn.Call(args)
			for _, result := range results {
				if result.Type() == errType && !result.IsNil() {
					if err := result.Interface().(error); errors.Is(err, fs.ErrPermission) {
						t.Errorf("%s touched the working directory: %v", method.Name, err)
					}
				}
			}
		}()
		swept++
	}
	skipped := 0
	for name := range rootlessSweepSkips {
		if _, ok := value.Type().MethodByName(name); !ok {
			t.Errorf("sweep skip %s names no method; remove it", name)
			continue
		}
		skipped++
	}
	if swept+skipped != value.NumMethod() {
		t.Fatalf("swept %d + skipped %d != %d exported methods", swept, skipped, value.NumMethod())
	}
	t.Logf("swept %d of %d exported methods (%d lifecycle hooks skipped)", swept, value.NumMethod(), skipped)
}
