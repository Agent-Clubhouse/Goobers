package cloudreader

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

type appliedSwitch struct {
	candidate atomic.Pointer[cloudconfig.Candidate]
}

func (a *appliedSwitch) Applied() (cloudconfig.Candidate, bool) {
	c := a.candidate.Load()
	if c == nil {
		return cloudconfig.Candidate{}, false
	}
	return *c, true
}

func (a *appliedSwitch) apply(digest, name string) {
	set := &instance.ConfigSet{Manifest: &apiv1.Manifest{Spec: apiv1.ManifestSpec{Instance: apiv1.InstanceRef{Name: name}}},
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}}
	a.candidate.Store(&cloudconfig.Candidate{Digest: digest, Config: &instance.Config{}, Set: set})
}

func testConfig(t *testing.T, applied AppliedConfiguration) Config {
	t.Helper()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root := instance.NewLayout(t.TempDir())
	backend := journal.NewFileReaderBackend(journal.FileReaderBackendConfig{
		ResolveRun:   func(_ context.Context, runID string) (string, error) { return root.FindRunDir(runID) },
		ResolveRoots: root.RunDirsContext, SchedulerDir: root.SchedulerDir(),
	})
	return Config{Journal: backend, ReadModel: store, Applied: applied, Ready: func() bool { return true }}
}

func TestReaderIsUnavailableUntilAGenerationIsApplied(t *testing.T) {
	t.Chdir(t.TempDir())
	applied := &appliedSwitch{}
	reader, err := New(testConfig(t, applied))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := reader.Health(ctx); !errors.Is(err, readservice.ErrUnavailable) {
		t.Fatalf("health before apply = %v, want unavailable", err)
	}
	if _, err := reader.ListRuns(ctx, readservice.RunListOptions{}); !errors.Is(err, readservice.ErrUnavailable) {
		t.Fatalf("runs before apply = %v, want unavailable", err)
	}
	if _, err := reader.Gaggles(ctx, readservice.PageRequest{}); !errors.Is(err, readservice.ErrUnavailable) {
		t.Fatalf("gaggles before apply = %v, want unavailable, never an empty inventory", err)
	}

	applied.apply("sha256:one", "first")
	first, err := reader.Instance(ctx)
	if err != nil || first.Name != "first" {
		t.Fatalf("instance after apply = %+v (%v)", first.Name, err)
	}
	built := reader.(*Reader).local
	if _, err := reader.Instance(ctx); err != nil || reader.(*Reader).local != built {
		t.Fatal("an unchanged generation rebuilt the read service")
	}

	applied.apply("sha256:two", "second")
	second, err := reader.Instance(ctx)
	if err != nil || second.Name != "second" || reader.(*Reader).local == built {
		t.Fatalf("instance after a new generation = %+v (%v), want the swapped definitions", second.Name, err)
	}
}

func TestReaderServesActiveClaimsOnlyWithAClaimSource(t *testing.T) {
	t.Chdir(t.TempDir())
	plain, err := New(testConfig(t, &appliedSwitch{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(readservice.ActiveClaimsReader); ok {
		t.Fatal("a reader without a claim source advertises active claims")
	}
	config := testConfig(t, &appliedSwitch{})
	config.ClaimSnapshot = func(context.Context) (readservice.ClaimView, error) { return nil, errors.New("unused") }
	withClaims, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := withClaims.(readservice.ActiveClaimsReader); !ok {
		t.Fatal("a reader with a claim source does not serve active claims")
	}
}

func TestNewRequiresItsSources(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"journal":    func(c *Config) { c.Journal = nil },
		"read model": func(c *Config) { c.ReadModel = nil },
		"applied":    func(c *Config) { c.Applied = nil },
		"ready":      func(c *Config) { c.Ready = nil },
	} {
		config := testConfig(t, &appliedSwitch{})
		mutate(&config)
		if _, err := New(config); err == nil {
			t.Errorf("reader built without its %s", name)
		}
	}
}

type runGaggles map[string]string

var errOutage = errors.New("read model down")

func (r runGaggles) GetRun(_ context.Context, runID string) (readmodel.RunRow, bool, error) {
	if runID == "outage" {
		return readmodel.RunRow{}, false, errOutage
	}
	gaggle, ok := r[runID]
	return readmodel.RunRow{RunID: runID, Gaggle: gaggle}, ok, nil
}

func TestContainsIsTheRunsOwnGaggleOnly(t *testing.T) {
	contains := Contains(runGaggles{"run-1": "web"})
	ctx := context.Background()
	for _, tc := range []struct {
		gaggle, run string
		want        bool
	}{{"web", "run-1", true}, {"api", "run-1", false}, {"web", "unknown", false}} {
		got, err := contains(ctx, tc.gaggle, tc.run)
		if err != nil || got != tc.want {
			t.Errorf("Contains(%s, %s) = %v, %v; want %v", tc.gaggle, tc.run, got, err, tc.want)
		}
	}
	if ok, err := contains(ctx, "web", "outage"); ok || !errors.Is(err, readservice.ErrUnavailable) || !errors.Is(err, errOutage) {
		t.Fatalf("read-model outage = %v, %v; want unavailable", ok, err)
	}
	if _, err := Contains(nil)(ctx, "web", "run-1"); !errors.Is(err, readservice.ErrUnavailable) {
		t.Fatalf("no read model = %v, want unavailable", err)
	}
}
