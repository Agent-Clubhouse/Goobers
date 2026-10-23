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

func TestDirConfigurationReportsAnInvalidDirectoryAndKeepsTheRunningGeneration(t *testing.T) {
	t.Parallel()
	root := demoDir(t)
	activator := &countingActivator{}
	cfg, err := NewDirConfiguration(acceptAll, activator, DirSource{Root: root, InstanceID: "instance-a"})
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
	if err := os.WriteFile(filepath.Join(root, "config", "manifest.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		clock = clock.Add(DefaultPollInterval + time.Second)
		if err := cfg.Reload(t.Context()); err != nil {
			t.Fatalf("an invalid directory failed the tick: %v", err)
		}
	}
	if cfg.Status().Applied != good || activator.count() != 1 || len(reported) != 2 {
		t.Fatalf("applied=%q activations=%d reported=%d, want the good generation kept and the problem re-reported each poll", cfg.Status().Applied, activator.count(), len(reported))
	}
}

// projectedVolume lays root out the way kubelet publishes a ConfigMap or
// projected volume: real files in a timestamped directory, ..data pointing at
// it, and every top-level entry a symlink through ..data.
func projectedVolume(t *testing.T, source, stamp string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "mount")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	publishProjected(t, root, source, stamp)
	return root
}

func publishProjected(t *testing.T, root, source, stamp string) {
	t.Helper()
	dir := filepath.Join(root, "..2026_"+stamp)
	if err := os.CopyFS(dir, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(root, "..data_tmp")
	if err := os.Symlink("..2026_"+stamp, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		link := filepath.Join(root, entry.Name())
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join("..data", entry.Name()), link); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirSourceReadsAKubernetesProjectedVolumeAndFollowsAtomicSwaps(t *testing.T) {
	t.Parallel()
	plain := demoDir(t)
	mount := projectedVolume(t, plain, "01")
	want, err := (DirSource{Root: plain, InstanceID: "instance-a"}).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source := DirSource{Root: mount, InstanceID: "instance-a"}
	got, err := source.Load(t.Context())
	if err != nil {
		t.Fatalf("a projected volume was refused: %v", err)
	}
	if got.Digest != want.Digest || got.Config == nil || got.Set == nil {
		t.Fatalf("projected digest %q != plain directory digest %q", got.Digest, want.Digest)
	}
	before, err := source.Fingerprint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The kubelet updates by publishing a new timestamped directory and swapping
	// ..data; the old directory is removed afterwards.
	touchDefinition(t, plain)
	publishProjected(t, mount, plain, "02")
	if err := os.RemoveAll(filepath.Join(mount, "..2026_01")); err != nil {
		t.Fatal(err)
	}
	after, err := source.Fingerprint(t.Context())
	if err != nil || after == before {
		t.Fatalf("an atomic swap was not seen: %q -> %q (%v)", before, after, err)
	}
	next, err := source.Load(t.Context())
	if err != nil || next.Digest == got.Digest {
		t.Fatalf("swapped content not loaded: %q (%v)", next.Digest, err)
	}
}

func TestDirConfigurationReportsAnUnreadableDirectoryWithoutFailingTheTick(t *testing.T) {
	t.Parallel()
	root := demoDir(t)
	cfg, err := NewDirConfiguration(acceptAll, &countingActivator{}, DirSource{Root: root, InstanceID: "instance-a"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg.now = func() time.Time { return clock }
	var reported []error
	cfg.SetReporter(func(err error) { reported = append(reported, err) })
	moved := root + ".away"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Reload(t.Context()); err != nil || len(reported) != 1 {
		t.Fatalf("an unreadable directory: err=%v reported=%d, want a reported problem and no tick failure", err, len(reported))
	}
	// Throttled until the interval passes, then retried; recovery applies.
	if err := cfg.Reload(t.Context()); err != nil || len(reported) != 1 {
		t.Fatalf("re-read inside the interval: err=%v reported=%d", err, len(reported))
	}
	if err := os.Rename(moved, root); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(DefaultPollInterval + time.Second)
	if err := cfg.Reload(t.Context()); err != nil || cfg.Status().Applied == "" {
		t.Fatalf("recovery after the directory returned: err=%v applied=%q", err, cfg.Status().Applied)
	}
}

// TestDirSourceNeverPairsOneVersionsArchiveWithAnothersPolicy swaps the source
// between the real read stages. A single swap must be refused as unstable; an
// A to B to A swap, where the before and after content hashes agree, must still
// yield a candidate whose digest and policy belong to the same version.
func TestDirSourceNeverPairsOneVersionsArchiveWithAnothersPolicy(t *testing.T) {
	t.Parallel()
	for _, rollback := range []bool{false, true} {
		name := "one swap"
		if rollback {
			name = "A to B to A"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, b := demoDir(t), demoDir(t)
			// A allows cloud execution, B denies it; their definitions differ too.
			file := filepath.Join(a, "instance.yaml")
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(strings.Replace(string(raw), "selfExecution: deny", "selfExecution: allow", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			touchDefinition(t, b)
			versionA, err := (DirSource{Root: a, InstanceID: "i"}).Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			versionB, err := (DirSource{Root: b, InstanceID: "i"}).Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if versionA.Digest == versionB.Digest || versionA.Config.SelfExecutionPolicy() == versionB.Config.SelfExecutionPolicy() {
				t.Fatal("fixture versions must differ in archive and policy")
			}

			root := filepath.Join(t.TempDir(), "current")
			if err := os.Symlink(a, root); err != nil {
				t.Fatal(err)
			}
			swap := func(target string) {
				next := root + ".next"
				if err := os.Symlink(target, next); err != nil {
					t.Error(err)
					return
				}
				if err := os.Rename(next, root); err != nil {
					t.Error(err)
				}
			}
			source := DirSource{Root: root, InstanceID: "i", afterCapture: func() { swap(b) }, beforeRecapture: func() {
				if rollback {
					swap(a)
				}
			}}
			got, err := source.Load(t.Context())
			if !rollback {
				if !errors.Is(err, ErrUnstable) {
					t.Fatalf("a single swap = %+v, %v; want ErrUnstable", got.Digest, err)
				}
				return
			}
			if errors.Is(err, ErrUnstable) {
				return // declining a moving source is allowed
			}
			if err != nil {
				t.Fatal(err)
			}
			coherentA := got.Digest == versionA.Digest && got.Config.SelfExecutionPolicy() == versionA.Config.SelfExecutionPolicy()
			coherentB := got.Digest == versionB.Digest && got.Config.SelfExecutionPolicy() == versionB.Config.SelfExecutionPolicy()
			if !coherentA && !coherentB {
				t.Fatalf("torn candidate: digest %q with policy %q (A=%q/%q B=%q/%q)", got.Digest, got.Config.SelfExecutionPolicy(),
					versionA.Digest, versionA.Config.SelfExecutionPolicy(), versionB.Digest, versionB.Config.SelfExecutionPolicy())
			}
		})
	}
}

func TestDirSourceParsesOnlyFromTheCapturedBytes(t *testing.T) {
	t.Parallel()
	root := demoDir(t)
	// Edit the source after the capture but before the parse: the candidate must
	// still describe the captured version, and the move is then declined.
	source := DirSource{Root: root, InstanceID: "i"}
	original, err := source.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source.afterCapture = func() {
		file := filepath.Join(root, "instance.yaml")
		raw, _ := os.ReadFile(file)
		_ = os.WriteFile(file, []byte(strings.Replace(string(raw), "selfExecution: deny", "selfExecution: allow", 1)), 0o600)
	}
	if _, err := source.Load(t.Context()); !errors.Is(err, ErrUnstable) {
		t.Fatalf("an in-place edit during the read = %v, want ErrUnstable", err)
	}
	if original.Config.SelfExecutionPolicy() != "deny" {
		t.Fatalf("baseline policy = %q", original.Config.SelfExecutionPolicy())
	}
}

// TestDirSourceResolvesOneVersionForDefinitionsAndPolicy swaps a projected
// volume between the lookups of config/ and instance.yaml. Both paths must come
// from the single resolution, so the candidate is one coherent version (or the
// move is declined), never version A's definitions with version B's policy.
func TestDirSourceResolvesOneVersionForDefinitionsAndPolicy(t *testing.T) {
	t.Parallel()
	a, b := demoDir(t), demoDir(t)
	file := filepath.Join(a, "instance.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(string(raw), "selfExecution: deny", "selfExecution: allow", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	touchDefinition(t, b)
	versionA, err := (DirSource{Root: a, InstanceID: "i"}).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	versionB, err := (DirSource{Root: b, InstanceID: "i"}).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, calls := range []int{1, 2} { // swap at the first resolution only, then at every one
		mount := projectedVolume(t, a, "01")
		seen := 0
		source := DirSource{Root: mount, InstanceID: "i", afterResolveConfig: func() {
			seen++
			if calls == 2 || seen == 1 {
				publishProjected(t, mount, map[bool]string{true: b, false: a}[seen%2 == 1], "0"+string(rune('1'+seen)))
			}
		}}
		got, err := source.Load(t.Context())
		if err != nil {
			continue // declining a moving source is allowed
		}
		coherentA := got.Digest == versionA.Digest && got.Config.SelfExecutionPolicy() == versionA.Config.SelfExecutionPolicy()
		coherentB := got.Digest == versionB.Digest && got.Config.SelfExecutionPolicy() == versionB.Config.SelfExecutionPolicy()
		if !coherentA && !coherentB {
			t.Fatalf("calls=%d: torn candidate: digest %q with policy %q", calls, got.Digest, got.Config.SelfExecutionPolicy())
		}
	}
}

func TestDirSourceRefusesAnInstanceFileOutsideTheConfigVersion(t *testing.T) {
	t.Parallel()
	root := demoDir(t)
	elsewhere := filepath.Join(t.TempDir(), "instance.yaml")
	data, err := os.ReadFile(filepath.Join(root, "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewhere, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "instance.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "instance.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := (DirSource{Root: root, InstanceID: "i"}).Load(t.Context()); err == nil {
		t.Fatal("an instance.yaml that resolves outside config/'s version directory was accepted")
	}
}
