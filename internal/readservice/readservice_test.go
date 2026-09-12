package readservice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workflow"
)

func testDefinitions() *instance.ConfigSet {
	return &instance.ConfigSet{Manifest: &apiv1.Manifest{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			workflow.PreviewFeaturesAnnotation: "true",
		}},
		Spec: apiv1.ManifestSpec{
			Instance: apiv1.InstanceRef{Name: "clubhouse", Environment: apiv1.EnvironmentDev},
		},
	}}
}

func TestLocalHealthProjectsIdentityReadinessAndFreshness(t *testing.T) {
	oldVersion, oldCommit, oldDate := version.Version, version.Commit, version.Date
	version.Version, version.Commit, version.Date = "v9.8.7", "abc1234", "2026-09-10T01:02:03Z"
	t.Cleanup(func() {
		version.Version, version.Commit, version.Date = oldVersion, oldCommit, oldDate
	})

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
	if err := service.ReloadDefinitions(testDefinitions(), nil, eventTime); err != nil {
		t.Fatal(err)
	}

	got, err := service.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.APIVersion != APIVersion || got.SchemaVersion != SchemaVersion {
		t.Fatalf("versions = %q/%q", got.APIVersion, got.SchemaVersion)
	}
	if got.Build != (BuildMetadata{Version: "v9.8.7", Commit: "abc1234", Date: "2026-09-10T01:02:03Z"}) {
		t.Fatalf("build = %+v", got.Build)
	}
	if !got.Ready {
		t.Fatal("health should report ready")
	}
	if !got.Healthy {
		t.Fatal("health without a daemon heartbeat should remain healthy")
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

func TestLocalHealthReportsStaleSchedulerHeartbeat(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	lastTickAt := time.Date(2026, time.July, 23, 9, 0, 0, 0, time.UTC)
	service, err := NewLocal(LocalSources{
		Layout:      l,
		Definitions: testDefinitions(),
		SchedulerHeartbeat: func() (time.Time, error) {
			return lastTickAt, nil
		},
		LivenessTimeout: time.Minute,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return lastTickAt.Add(2 * time.Minute) }

	got, err := service.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ready || got.Healthy {
		t.Fatalf("health = %+v, want ready but unhealthy", got)
	}
	if got.Freshness.LastSchedulerTickAt == nil || !got.Freshness.LastSchedulerTickAt.Equal(lastTickAt) {
		t.Fatalf("lastSchedulerTickAt = %v, want %s", got.Freshness.LastSchedulerTickAt, lastTickAt)
	}
	if got.Freshness.LastTickAgeMillis == nil || *got.Freshness.LastTickAgeMillis != 120_000 {
		t.Fatalf("lastTickAgeMillis = %v, want 120000", got.Freshness.LastTickAgeMillis)
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
	if err := service.ReloadDefinitions(reloaded, nil, loadedAt); err != nil {
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
	inventory, err := service.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Name != "reloaded-clubhouse" || inventory.Environment != apiv1.EnvironmentStaging {
		t.Fatalf("inventory instance = %+v", inventory)
	}
}

func TestLocalPortalConfigUsesEffectiveDefaults(t *testing.T) {
	service, err := NewLocal(LocalSources{
		Config:      &instance.Config{},
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.PortalConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Brand.Name != "goobers" || got.Brand.Tagline != "local operations" || got.Brand.ScopeMark != "G" {
		t.Fatalf("brand = %+v", got.Brand)
	}
	if got.Brand.LogoURL != nil || got.Brand.FaviconURL != nil {
		t.Fatalf("unexpected logo defaults: %+v", got.Brand)
	}
	if got.Support.Links == nil || len(got.Support.Links) != 0 {
		t.Fatalf("support links = %+v, want empty slice", got.Support.Links)
	}
}

func TestLocalPortalConfigProjectsConfiguredValues(t *testing.T) {
	service, err := NewLocal(LocalSources{
		Config: &instance.Config{Portal: instance.PortalConfig{
			Brand: instance.PortalBrandConfig{
				Name:       "Acme Ops",
				Tagline:    "AI workforce platform",
				ScopeMark:  "A",
				LogoURL:    "/assets/logo.svg",
				FaviconURL: "/assets/favicon.ico",
			},
			Theme: instance.PortalThemeConfig{
				AccentLight:     "#6847d9",
				AccentDark:      "#a98cff",
				AccentSoftLight: "#eee9ff",
				AccentSoftDark:  "#2c2445",
				AccentInkLight:  "#4c2db8",
				AccentInkDark:   "#c5b4ff",
			},
			Support: instance.PortalSupportConfig{
				DocsURL:   "https://acme.example/docs",
				IssuesURL: "https://acme.example/help",
				ChatURL:   "https://acme.example/chat",
				Links: []instance.PortalSupportLink{{
					Label: "Runbooks",
					URL:   "https://acme.example/runbooks",
				}},
			},
		}},
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.PortalConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Brand.LogoURL == nil || *got.Brand.LogoURL != "/assets/logo.svg" {
		t.Fatalf("logoUrl = %v", got.Brand.LogoURL)
	}
	if got.Theme.AccentDark == nil || *got.Theme.AccentDark != "#a98cff" {
		t.Fatalf("accentDark = %v", got.Theme.AccentDark)
	}
	if len(got.Support.Links) != 1 || got.Support.Links[0].Label != "Runbooks" {
		t.Fatalf("support links = %+v", got.Support.Links)
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

// The portal reads update availability from Health, so the read must be a
// local read that never contacts the release source, never fails the endpoint,
// and never restates a verdict computed against a different binary (#4920).
func TestHealthUpdateAvailability(t *testing.T) {
	running := version.Get().Version
	enabled := func(root string) *Local {
		return &Local{sources: LocalSources{Layout: instance.NewLayout(root), Config: &instance.Config{}}}
	}
	write := func(t *testing.T, root string, result selfupdate.CheckResult) {
		t.Helper()
		if err := selfupdate.WriteCheck(root, result); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(current string) selfupdate.CheckResult {
		return selfupdate.CheckResult{
			CurrentVersion: current, LatestVersion: "v99.0.0", UpdateAvailable: true,
			Channel: selfupdate.ChannelStable, CheckedAt: time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC),
		}
	}

	t.Run("absent before any check", func(t *testing.T) {
		if got := enabled(t.TempDir()).updateAvailability(); got != nil {
			t.Errorf("updateAvailability() = %+v, want nil before the first check", got)
		}
	})

	t.Run("reports a cached pending update", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, pending(running))
		got := enabled(root).updateAvailability()
		if got == nil {
			t.Fatal("updateAvailability() = nil, want the cached result")
		}
		if !got.Available || got.LatestVersion != "v99.0.0" || got.CurrentVersion != running {
			t.Errorf("updateAvailability() = %+v", got)
		}
		if got.Channel != selfupdate.ChannelStable || got.CheckedAt.IsZero() {
			t.Errorf("updateAvailability() = %+v, want the channel and check time carried", got)
		}
	})

	// The cached verdict is "latest > current" for whichever build performed
	// the check. A dev build never refreshes the cache at all, so a file left
	// by a released build would otherwise make the portal claim an update
	// forever — and the same stale claim survives a real upgrade whose next
	// check cannot complete.
	t.Run("refuses a verdict from a different build", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, pending("v0.0.1-not-the-running-build"))
		if got := enabled(root).updateAvailability(); got != nil {
			t.Errorf("updateAvailability() = %+v, want nil for a verdict computed against another build", got)
		}
	})

	// Turning the check off must silence the surface. Nothing refreshes or
	// removes the cache once the checker stops running, so a file left by an
	// earlier enabled run would assert a pending update forever.
	t.Run("disabled check reports nothing", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, pending(running))
		off := false
		local := &Local{sources: LocalSources{
			Layout: instance.NewLayout(root),
			Config: &instance.Config{UpdateCheck: &instance.UpdateCheckConfig{Enabled: &off}},
		}}
		if got := local.updateAvailability(); got != nil {
			t.Errorf("updateAvailability() = %+v, want nil when updateCheck.enabled is false", got)
		}
	})

	// Losing an advisory field must never take /api/v1/health down.
	t.Run("corrupt cache degrades to absent", func(t *testing.T) {
		root := t.TempDir()
		path := selfupdate.CheckPath(root)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := enabled(root).updateAvailability(); got != nil {
			t.Errorf("updateAvailability() = %+v, want nil for a corrupt cache", got)
		}
	})

	// /api/v1/health is the highest-frequency route and this value changes at
	// most once a day, so an unchanged file must not be re-decoded per call.
	t.Run("memoizes an unchanged cache", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, pending(running))
		local := enabled(root)
		if got := local.updateAvailability(); got == nil {
			t.Fatal("first read returned nil")
		}
		// Removing the file leaves the memo intact but its stat fails, which
		// is the observable proof the second read did not decode again.
		first := local.updateCheck.Load()
		if first == nil {
			t.Fatal("first read did not memoize")
		}
		if got := local.updateAvailability(); got == nil {
			t.Fatal("second read returned nil")
		}
		if second := local.updateCheck.Load(); second != first {
			t.Error("second read replaced the memo for an unchanged file")
		}
	})

	// A refreshed cache must be picked up, or the memo would pin the first
	// answer for the life of the daemon.
	t.Run("re-reads after the cache changes", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, pending(running))
		local := enabled(root)
		if got := local.updateAvailability(); got == nil || !got.Available {
			t.Fatalf("first read = %+v, want a pending update", got)
		}
		current := pending(running)
		current.UpdateAvailable = false
		current.LatestVersion = running
		// Force a distinct mtime: a same-second rewrite would otherwise be
		// indistinguishable on filesystems with coarse timestamps.
		if err := os.Chtimes(selfupdate.CheckPath(root), time.Now(), time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		write(t, root, current)
		if err := os.Chtimes(selfupdate.CheckPath(root), time.Now(), time.Now().Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		got := local.updateAvailability()
		if got == nil || got.Available {
			t.Errorf("second read = %+v, want the refreshed not-available result", got)
		}
	})
}
