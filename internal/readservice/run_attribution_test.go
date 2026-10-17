package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestRunAttributionPreservesRecordWithoutTelemetryOrAudit(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	dir := filepath.Join(layout.RunsDir(), runID)
	want, err := creditgraph.ReadRunRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	service := &Local{sources: LocalSources{Layout: layout}}
	for _, status := range []creditgraph.RecordStatus{creditgraph.RecordComplete, creditgraph.RecordInsufficientEvidence, creditgraph.RecordFailed} {
		want.Status = status
		writeAttributionTestJSON(t, filepath.Join(dir, creditgraph.RecordFileName), want)
		before := snapshotAttributionFiles(t, root)
		got, err := service.RunAttribution(context.Background(), runID, "core")
		if err != nil {
			t.Fatal(err)
		}
		if got.Availability != "recorded" || got.RunPhase != journal.PhaseFailed || !reflect.DeepEqual(got.Record, &want) {
			t.Fatalf("record changed: %#v", got)
		}
		if got.InputBytes <= 0 || len(got.Evidence) != len(want.Evidence) {
			t.Fatalf("missing read/evidence accounting: %#v", got)
		}
		span, artifact := false, false
		for _, check := range got.Evidence {
			span = span || check.Kind == "span"
			artifact = artifact || check.Kind == "artifact"
			if check.Status != "verified" {
				t.Fatal(check)
			}
		}
		if !span || !artifact {
			t.Fatalf("missing typed evidence: %#v", got.Evidence)
		}
		if !reflect.DeepEqual(before, snapshotAttributionFiles(t, root)) {
			t.Fatal("read mutated filesystem")
		}
	}
}
func TestRunAttributionRefusesInvalidAndOversizedInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string, *creditgraph.RunRecord)
		limit  bool
	}{
		{"identity", func(t *testing.T, _ string, r *creditgraph.RunRecord) { r.RunID = "foreign" }, false},
		{"workflow", func(t *testing.T, _ string, r *creditgraph.RunRecord) { r.Workflow = "foreign" }, false},
		{"evidence sequence", func(t *testing.T, _ string, r *creditgraph.RunRecord) { r.Evidence[0].JournalSequence = 99999 }, false},
		{"evidence run", func(t *testing.T, _ string, r *creditgraph.RunRecord) { r.Evidence[0].RunID = "foreign" }, false},
		{"traversal", func(t *testing.T, _ string, r *creditgraph.RunRecord) { r.Evidence[0].JournalPath = "../events.jsonl" }, false},
		{"record limit", func(t *testing.T, _ string, r *creditgraph.RunRecord) {
			r.Failure = strings.Repeat("x", attributionRecordBytes)
		}, true},
		{"tampered blob", func(t *testing.T, dir string, r *creditgraph.RunRecord) {
			for _, e := range r.Evidence {
				if e.ArtifactPath != "" {
					if err := os.WriteFile(filepath.Join(dir, e.ArtifactPath), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
					return
				}
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, runID := seedStoredAttributionRun(t)
			layout := instance.NewLayout(root)
			dir := filepath.Join(layout.RunsDir(), runID)
			record, err := creditgraph.ReadRunRecord(dir)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, dir, &record)
			writeAttributionTestJSON(t, filepath.Join(dir, creditgraph.RecordFileName), record)
			got, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), runID, "core")
			if tc.limit {
				if err != nil || got.Availability != "limit-exceeded" || got.Record != nil {
					t.Fatalf("partial/incorrect limit: %+v %v", got, err)
				}
			} else if !errors.Is(err, ErrArtifactIntegrity) {
				t.Fatalf("want integrity refusal, got %v", err)
			}
		})
	}
}
func TestRunAttributionMissingEvidenceAndRecordAreDistinct(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	dir := filepath.Join(layout.RunsDir(), runID)
	record, err := creditgraph.ReadRunRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range record.Evidence {
		if link.ArtifactPath != "" {
			if err := os.Remove(filepath.Join(dir, link.ArtifactPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
	service := &Local{sources: LocalSources{Layout: layout}}
	got, err := service.RunAttribution(context.Background(), runID, "core")
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.Status != record.Status {
		t.Fatal("missing evidence changed recorded analysis")
	}
	missing := 0
	for _, e := range got.Evidence {
		if e.Status == "unavailable" {
			missing++
		}
	}
	if missing == 0 {
		t.Fatal("missing evidence concealed")
	}
	if err := os.Remove(filepath.Join(dir, creditgraph.RecordFileName)); err != nil {
		t.Fatal(err)
	}
	got, err = service.RunAttribution(context.Background(), runID, "core")
	if err != nil || got.Availability != "absent" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestRunAttributionScopeCancellationAndBoundedFiles(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	service := &Local{sources: LocalSources{Layout: layout}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.RunAttribution(ctx, runID, "core"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, gaggle := range []string{"", "../core", "core/other"} {
		if _, err := service.RunAttribution(context.Background(), runID, gaggle); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(err)
		}
	}
	if _, err := service.RunAttribution(context.Background(), runID, "foreign"); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatal(err)
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()
	b := &attributionReadBudget{ctx: context.Background(), root: handle, remaining: 4}
	if err := os.WriteFile(filepath.Join(root, "small"), []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.read("small", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := b.read("small", 4); !errors.Is(err, errAttributionLimit) {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.read("escape", 10); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatal(err)
	}
}
func writeAttributionTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func snapshotAttributionFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = journal.Digest(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunAttributionPreservesRedactedSpanAnalysis(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	registry, scrubber := journal.DefaultScrubber()
	runID := "redacted-attribution"
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: runID, Gaggle: "core", Workflow: "work", WorkflowVersion: 1, WorkflowDigest: "sha256:workflow"}, nil, journal.WithScrubber(scrubber))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	span, err := run.RecordSpan("work", "trace", []byte("private marker to remove"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	for _, e := range events {
		if e.Ref != nil && e.Ref.Digest == span.Digest {
			sequence = e.Seq
		}
	}
	record := creditgraph.RunRecord{Schema: creditgraph.RecordSchemaVersion, Status: creditgraph.RecordComplete, RunID: runID, Gaggle: "core", Workflow: "work", WorkflowVersion: 1, WorkflowDigest: "sha256:workflow", Attribution: creditgraph.Attribution{Schema: creditgraph.AttributionSchemaVersion, RunID: runID, Contributions: []creditgraph.Contribution{{NodeID: "tool", Score: -0.5, Share: 0.5}}}, Evidence: []creditgraph.AttributionEvidenceLink{{RunID: runID, NodeID: "tool", JournalSequence: int64(sequence), JournalPath: "events.jsonl", ArtifactPath: span.Path, ArtifactDigest: span.Digest, ArtifactMediaType: span.MediaType}}}
	writeAttributionTestJSON(t, filepath.Join(run.Dir(), creditgraph.RecordFileName), record)
	registry.Register([]byte("private marker to remove"))
	replacement, err := run.Redact(span, "fixture redaction")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Path != span.Path || replacement.Digest == span.Digest {
		t.Fatal("fixture did not exercise in-place span redaction")
	}
	got, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), runID, "core")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Record, &record) || len(got.Evidence) != 1 || got.Evidence[0].Status != "redacted" {
		t.Fatalf("redaction replaced/hidden analysis: %+v", got)
	}
}

func TestRunAttributionReadsScopedRunWithoutNamespaceScan(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	scoped := layout.ForGaggle("core").RunsDir()
	if err := os.MkdirAll(scoped, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(layout.RunsDir(), runID), filepath.Join(scoped, runID)); err != nil {
		t.Fatal(err)
	}
	// An unrelated unreadable/symlinked namespace must not be traversed.
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(layout.GagglesDir(), "unrelated")); err != nil {
		t.Fatal(err)
	}
	got, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), runID, "core")
	if err != nil || got.Availability != "recorded" {
		t.Fatalf("scoped lookup failed: %+v %v", got, err)
	}
}

func TestRunAttributionSupportsRuntimeNamesAndCompatibilityAlias(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	gaggle := "core team"
	runID := "alias-attribution"
	scoped := layout.ForGaggle(gaggle).RunsDir()
	run, err := journal.Create(scoped, journal.RunIdentity{RunID: runID, Gaggle: gaggle, Workflow: "work"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), scoped); err != nil {
		t.Fatal(err)
	}
	got, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), runID, gaggle)
	if err != nil || got.Availability != "pending" {
		t.Fatalf("runtime name/alias rejected: %+v %v", got, err)
	}
}

func TestRunAttributionHandlesAbsoluteRuntimeAliasWithoutEscaping(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "contained"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			root, _, runID := seedStoredAttributionRun(t)
			layout := instance.NewLayout(root)
			scoped := layout.ForGaggle("core").RunsDir()
			if err := os.MkdirAll(filepath.Dir(scoped), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(layout.RunsDir(), scoped); err != nil {
				t.Fatal(err)
			}
			target := scoped
			if external {
				target = t.TempDir()
			}
			var err error
			if runtime.GOOS == "windows" {
				// The runtime helper creates an absolute junction without
				// requiring Windows symlink privileges.
				err = instance.CreateLegacyRuntimeAlias(layout.RunsDir(), target)
			} else {
				err = os.Symlink(target, layout.RunsDir())
			}
			if err != nil {
				t.Fatal(err)
			}
			aliasTarget, err := os.Readlink(layout.RunsDir())
			if err != nil || !filepath.IsAbs(aliasTarget) {
				t.Fatalf("fixture is not an absolute runtime alias: %q %v", aliasTarget, err)
			}
			got, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), runID, "core")
			if external {
				if !errors.Is(err, ErrArtifactIntegrity) {
					t.Fatalf("external alias accepted: %+v %v", got, err)
				}
			} else if err != nil || got.Availability != "recorded" {
				t.Fatalf("contained absolute alias rejected: %+v %v", got, err)
			}
		})
	}
}

func TestAttributionRuntimeAliasReplacementCannotRedirectLookup(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	scoped := layout.ForGaggle("core").RunsDir()
	if err := os.MkdirAll(filepath.Dir(scoped), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(layout.RunsDir(), scoped); err != nil {
		t.Fatal(err)
	}
	if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), scoped); err != nil {
		t.Fatal(err)
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()
	runRoot, err := attributionRunRoot(handle, layout, runID, "core")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runRoot.Close() }()
	if err := os.Remove(layout.RunsDir()); err != nil {
		t.Fatal(err)
	}
	if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	budget := &attributionReadBudget{ctx: context.Background(), root: runRoot, remaining: attributionInputBytes}
	out := RunAttributionResult{RunID: runID, Gaggle: "core"}
	if err := loadRunAttribution(budget, &out); err != nil || out.Availability != "recorded" {
		t.Fatalf("alias replacement redirected lookup: %+v %v", out, err)
	}
}

func TestAttributionLegacyFallbackReplacementCannotRedirectLookup(t *testing.T) {
	for _, initial := range []string{"ordinary", "missing"} {
		for _, targetKind := range []string{"unexpected", "other-gaggle"} {
			t.Run(initial+"/"+targetKind, func(t *testing.T) {
				root, _, runID := seedStoredAttributionRun(t)
				layout := instance.NewLayout(root)
				if initial == "missing" {
					scoped := layout.ForGaggle("core").RunsDir()
					if err := os.MkdirAll(filepath.Dir(scoped), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(layout.RunsDir(), scoped); err != nil {
						t.Fatal(err)
					}
				}
				handle, err := os.OpenRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = handle.Close() }()
				runRoot, err := attributionRunRoot(handle, layout, runID, "core")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = runRoot.Close() }()
				if initial == "ordinary" {
					if err := os.Rename(layout.RunsDir(), filepath.Join(root, "original-runs")); err != nil {
						t.Fatal(err)
					}
				}
				target := filepath.Join(root, "unexpected-runs")
				if targetKind == "other-gaggle" {
					target = layout.ForGaggle("other").RunsDir()
				}
				if err := os.MkdirAll(filepath.Join(target, runID), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, runID, "run.yaml"), []byte("must not read this journal"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), target); err != nil {
					t.Fatal(err)
				}
				budget := &attributionReadBudget{ctx: context.Background(), root: runRoot, remaining: attributionInputBytes}
				out := RunAttributionResult{RunID: runID, Gaggle: "core"}
				if err := loadRunAttribution(budget, &out); err != nil || out.Availability != "recorded" {
					t.Fatalf("legacy replacement redirected pinned lookup: %+v %v", out, err)
				}
			})
		}
	}
}

func TestAttributionLegacyFallbackRejectsAliasBeforeOpen(t *testing.T) {
	for _, initial := range []string{"ordinary", "missing"} {
		for _, targetKind := range []string{"unexpected", "other-gaggle"} {
			t.Run(initial+"/"+targetKind, func(t *testing.T) {
				root := t.TempDir()
				layout := instance.NewLayout(root)
				if initial == "ordinary" {
					if err := os.Mkdir(layout.RunsDir(), 0700); err != nil {
						t.Fatal(err)
					}
				}
				handle, err := os.OpenRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = handle.Close() }()
				legacy, err := attributionLegacyRunsDir(handle, layout, "core")
				if err != nil || legacy != instance.RunsDirName {
					t.Fatalf("legacy fallback not selected: %q %v", legacy, err)
				}
				if initial == "ordinary" {
					if err := os.Rename(layout.RunsDir(), filepath.Join(root, "original-runs")); err != nil {
						t.Fatal(err)
					}
				}
				target := filepath.Join(root, "unexpected-runs")
				if targetKind == "other-gaggle" {
					target = layout.ForGaggle("other").RunsDir()
				}
				if err := os.MkdirAll(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), target); err != nil {
					t.Fatal(err)
				}
				dir, _, err := openAttributionDirectory(handle, legacy)
				if dir != nil {
					_ = dir.Close()
				}
				if !errors.Is(err, ErrArtifactIntegrity) {
					t.Fatalf("replacement alias was opened as an ordinary fallback: %v", err)
				}
			})
		}
	}
}

func TestRunAttributionRejectsConflictingScopedCopy(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(filepath.Join(layout.ForGaggle("core").RunsDir(), runID), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), runID, "core"); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("conflicting run directories accepted: %v", err)
	}
}

func TestAttributionReadUsesCanonicalJournalPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "evidence", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evidence", "nested", "blob"), []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()
	budget := &attributionReadBudget{ctx: context.Background(), root: handle, remaining: attributionInputBytes}
	got, err := budget.read("evidence/nested/blob", 1024)
	if err != nil || string(got) != "evidence" {
		t.Fatalf("canonical journal path rejected: %q %v", got, err)
	}
	for _, name := range []string{`evidence\nested\blob`, "evidence/../evidence/nested/blob", "evidence//nested/blob", "../evidence/nested/blob"} {
		if _, err := budget.read(name, 1024); !errors.Is(err, ErrArtifactIntegrity) {
			t.Fatalf("noncanonical path %q accepted: %v", name, err)
		}
	}
}

func TestRunAttributionIgnoresOtherGaggleRuntimeAlias(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	scoped := layout.ForGaggle("core").RunsDir()
	if err := os.MkdirAll(filepath.Dir(scoped), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(layout.RunsDir(), scoped); err != nil {
		t.Fatal(err)
	}
	foreign := layout.ForGaggle("other").RunsDir()
	if err := os.MkdirAll(filepath.Join(foreign, runID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, runID, "run.yaml"), []byte("must not read this journal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), foreign); err != nil {
		t.Fatal(err)
	}
	service := &Local{sources: LocalSources{Layout: layout}}
	got, err := service.RunAttribution(context.Background(), runID, "core")
	if err != nil || got.Availability != "recorded" {
		t.Fatalf("foreign alias affected scoped run: %+v %v", got, err)
	}
	if _, err := service.RunAttribution(context.Background(), runID, "missing-gaggle"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign alias supplied a missing scoped run: %v", err)
	}
}

func TestRunAttributionRejectsUnexpectedInternalAlias(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	unexpected := filepath.Join(root, "unexpected-runs")
	if err := os.Rename(layout.RunsDir(), unexpected); err != nil {
		t.Fatal(err)
	}
	if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), unexpected); err != nil {
		t.Fatal(err)
	}
	// The unexpected target contains a valid record with matching identity;
	// that must not authorize reading an arbitrary directory inside the root.
	if _, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), runID, "core"); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("unexpected alias target accepted: %v", err)
	}
}

func TestRunAttributionMissingScopedAliasTargetIsNotFound(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := instance.CreateLegacyRuntimeAlias(layout.RunsDir(), layout.ForGaggle("core").RunsDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Local{sources: LocalSources{Layout: layout}}).RunAttribution(context.Background(), "missing-run", "core"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing scoped alias target became an integrity failure: %v", err)
	}
}
