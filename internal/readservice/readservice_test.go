package readservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func testDefinitions() *instance.ConfigSet {
	return &instance.ConfigSet{Manifest: &apiv1.Manifest{
		Spec: apiv1.ManifestSpec{
			Instance: apiv1.InstanceRef{Name: "clubhouse", Environment: apiv1.EnvironmentDev},
		},
	}}
}

func TestLocalHealthProjectsIdentityReadinessAndFreshness(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	eventTime := time.Date(2026, 7, 16, 10, 0, 0, 0, time.FixedZone("test", -7*60*60))
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir(), journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(l.SchedulerDir(), "events.jsonl"), eventTime, eventTime); err != nil {
		t.Fatal(err)
	}

	service, err := NewLocal(LocalSources{Layout: l, Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	observedAt := eventTime.Add(time.Minute).UTC()
	service.now = func() time.Time { return observedAt }
	if err := service.ReloadDefinitions(testDefinitions(), eventTime); err != nil {
		t.Fatal(err)
	}

	got, err := service.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.APIVersion != APIVersion || got.SchemaVersion != SchemaVersion {
		t.Fatalf("versions = %q/%q", got.APIVersion, got.SchemaVersion)
	}
	if !got.Ready {
		t.Fatal("health should report ready")
	}
	if got.Instance.Name != "clubhouse" || got.Instance.Environment != apiv1.EnvironmentDev {
		t.Fatalf("instance = %+v", got.Instance)
	}
	if !got.Freshness.ObservedAt.Equal(observedAt) {
		t.Fatalf("observedAt = %s, want %s", got.Freshness.ObservedAt, observedAt)
	}
	if got.Freshness.JournalUpdatedAt == nil || !got.Freshness.JournalUpdatedAt.Equal(eventTime) {
		t.Fatalf("journalUpdatedAt = %v, want %s", got.Freshness.JournalUpdatedAt, eventTime.UTC())
	}
	if !got.Freshness.DefinitionsLoadedAt.Equal(eventTime) {
		t.Fatalf("definitionsLoadedAt = %s, want %s", got.Freshness.DefinitionsLoadedAt, eventTime.UTC())
	}
}

func TestLocalHealthUsesReloadedDefinitionsSnapshot(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{Layout: l, Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	loadedAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.FixedZone("test", -7*60*60))
	reloaded := testDefinitions()
	reloaded.Manifest.Spec.Instance = apiv1.InstanceRef{
		Name:        "reloaded-clubhouse",
		Environment: apiv1.EnvironmentStaging,
	}
	if err := service.ReloadDefinitions(reloaded, loadedAt); err != nil {
		t.Fatal(err)
	}

	got, err := service.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Instance.Name != "reloaded-clubhouse" || got.Instance.Environment != apiv1.EnvironmentStaging {
		t.Fatalf("instance = %+v", got.Instance)
	}
	if !got.Freshness.DefinitionsLoadedAt.Equal(loadedAt) {
		t.Fatalf("definitionsLoadedAt = %s, want %s", got.Freshness.DefinitionsLoadedAt, loadedAt.UTC())
	}
}

func TestLocalHealthSurfacesJournalReadError(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	service, err := NewLocal(LocalSources{Layout: l, Definitions: testDefinitions()}, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "read instance journal freshness") {
		t.Fatalf("Health error = %v", err)
	}
}

func TestNewLocalRequiresSources(t *testing.T) {
	if _, err := NewLocal(LocalSources{}, func() bool { return false }); err == nil {
		t.Fatal("expected missing manifest error")
	}
	if _, err := NewLocal(LocalSources{Definitions: testDefinitions()}, nil); err == nil {
		t.Fatal("expected missing readiness function error")
	}
}
