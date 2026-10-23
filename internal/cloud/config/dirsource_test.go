package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
)

type countingActivator struct {
	mu      sync.Mutex
	digests []string
}

func (a *countingActivator) Activate(_ context.Context, c Candidate) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.digests = append(a.digests, c.Digest)
	return nil
}

func (a *countingActivator) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.digests)
}

func acceptAll(*instance.Config, *instance.ConfigSet) Assessment { return Assessment{} }

func demoDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	// The cloud refuses a placement that can execute stages.
	file := filepath.Join(root, "instance.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(data, []byte("placement:\n  selfExecution: deny\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func touchDefinition(t *testing.T, root string) {
	t.Helper()
	var target string
	err := filepath.WalkDir(instance.NewLayout(root).ConfigDir(), func(path string, entry os.DirEntry, err error) error {
		if err == nil && target == "" && !entry.IsDir() && strings.HasSuffix(path, "instructions.md") {
			target = path
		}
		return err
	})
	if err != nil || target == "" {
		t.Fatalf("no instructions file to edit: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(data, []byte("\nedited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirSourceUsesTheDaemonsGenerationBuilder(t *testing.T) {
	t.Parallel()
	root := demoDir(t)
	source := DirSource{Root: root, InstanceID: "instance-a"}
	first, err := source.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	archive, digest, err := configgeneration.CaptureForInstance(t.Context(), instance.NewLayout(root).ConfigDir(), "instance-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != digest || string(first.Archive) != string(archive) || first.Config == nil || first.Set == nil {
		t.Fatalf("candidate %q differs from the daemon builder's %q", first.Digest, digest)
	}
	second, err := source.Load(t.Context())
	if err != nil || second.Digest != first.Digest {
		t.Fatalf("identical content produced %q then %q (%v)", first.Digest, second.Digest, err)
	}
	other, err := (DirSource{Root: root, InstanceID: "instance-b"}).Load(t.Context())
	if err != nil || other.Digest == first.Digest {
		t.Fatalf("another instance shares a deletable pin: %q (%v)", other.Digest, err)
	}
	if _, err := configgeneration.Decode(first.Archive, first.Digest); err != nil {
		t.Fatalf("archive does not verify: %v", err)
	}
}

func TestDirSourceFailsClosed(t *testing.T) {
	t.Parallel()
	if _, err := (DirSource{Root: filepath.Join(t.TempDir(), "absent"), InstanceID: "i"}).Load(t.Context()); err == nil {
		t.Fatal("missing directory loaded")
	}
	if _, err := (DirSource{Root: t.TempDir()}).Load(t.Context()); err == nil {
		t.Fatal("source without an instance loaded")
	}
	root := demoDir(t)
	if err := os.WriteFile(filepath.Join(root, "config", "manifest.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (DirSource{Root: root, InstanceID: "i"}).Load(t.Context()); err == nil {
		t.Fatal("invalid definitions loaded")
	}
	if _, err := NewDirConfiguration(acceptAll, &countingActivator{}, DirSource{Root: root, InstanceID: "i"}); err == nil {
		t.Fatal("boot accepted an invalid directory")
	}
	if _, err := NewDirConfiguration(acceptAll, &countingActivator{}, DirSource{Root: filepath.Join(t.TempDir(), "absent"), InstanceID: "i"}); err == nil {
		t.Fatal("boot accepted a missing directory")
	}
}

func TestDirConfigurationAppliesOnceAndFollowsContentAtThePollInterval(t *testing.T) {
	t.Parallel()
	root := demoDir(t)
	activator := &countingActivator{}
	cfg, err := NewDirConfiguration(acceptAll, activator, DirSource{Root: root, InstanceID: "instance-a"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg.now = func() time.Time { return clock }
	ctx := t.Context()
	if err := cfg.Reload(ctx); err != nil || activator.count() != 1 {
		t.Fatalf("first reload: activations=%d err=%v", activator.count(), err)
	}
	first := cfg.Status().Applied
	if first == "" {
		t.Fatal("nothing applied")
	}
	// Unchanged content, before and after the interval: no further activation.
	for range 3 {
		clock = clock.Add(DefaultPollInterval + time.Second)
		if err := cfg.Reload(ctx); err != nil || activator.count() != 1 {
			t.Fatalf("unchanged reload re-applied: activations=%d err=%v", activator.count(), err)
		}
	}
	// A change inside the interval waits for the next poll.
	touchDefinition(t, root)
	clock = clock.Add(time.Second)
	if err := cfg.Reload(ctx); err != nil || activator.count() != 1 {
		t.Fatalf("throttled reload applied early: activations=%d err=%v", activator.count(), err)
	}
	clock = clock.Add(DefaultPollInterval)
	if err := cfg.Reload(ctx); err != nil || activator.count() != 2 || cfg.Status().Applied == first {
		t.Fatalf("changed content not applied: activations=%d applied=%q err=%v", activator.count(), cfg.Status().Applied, err)
	}
}

func TestDirConfigurationKeepsTheRunningGenerationWhenACandidateIsRefused(t *testing.T) {
	t.Parallel()
	root := demoDir(t)
	activator := &countingActivator{}
	refuse := false
	assess := func(_ *instance.Config, set *instance.ConfigSet) Assessment {
		if !refuse {
			return Assessment{}
		}
		out := Assessment{}
		for _, w := range set.Workflows {
			out[workflowKey(w.Spec.Gaggle, w.Name)] = []Finding{{Code: "RUNTIME002", Message: "self placement"}}
		}
		return out
	}
	cfg, err := NewDirConfiguration(assess, activator, DirSource{Root: root, InstanceID: "instance-a"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg.now = func() time.Time { return clock }
	var reported []error
	cfg.SetReporter(func(err error) { reported = append(reported, err) })
	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	good := cfg.Status().Applied
	refuse = true
	touchDefinition(t, root)
	clock = clock.Add(DefaultPollInterval + time.Second)
	if err := cfg.Reload(t.Context()); err != nil {
		t.Fatalf("a refused candidate failed the tick: %v", err)
	}
	status := cfg.Status()
	if status.Applied != good || status.Refusal == nil || status.RejectedDigest == "" || activator.count() != 1 {
		t.Fatalf("status = %+v activations=%d, want the good generation kept and the refusal named", status, activator.count())
	}
	if len(reported) != 1 || !errors.Is(reported[0], ErrIneligible) {
		t.Fatalf("refusal not reported: %v", reported)
	}
	// The same refused content is not re-assessed on later polls.
	clock = clock.Add(DefaultPollInterval + time.Second)
	if err := cfg.Reload(t.Context()); err != nil || len(reported) != 1 {
		t.Fatalf("refused content re-reported: %v %v", reported, err)
	}
}
