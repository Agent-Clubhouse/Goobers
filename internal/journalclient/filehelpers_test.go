package journalclient

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestJournalQuiescentBefore(t *testing.T) {
	since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setMod := func(at time.Time) {
		t.Helper()
		if err := os.Chtimes(events, at, at); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		since time.Time
		mod   time.Time
		want  bool
	}{
		{"zero since never prunes", time.Time{}, since.Add(-24 * time.Hour), false},
		{"well before cutoff prunes", since, since.Add(-time.Hour), true},
		{"within clock slack kept", since, since.Add(-30 * time.Second), false},
		{"exactly at slack boundary kept", since, since.Add(-journalClockSlack), false},
		{"after cutoff kept", since, since.Add(time.Hour), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMod(tc.mod)
			if got := journalQuiescentBefore(dir, tc.since); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("missing journal never prunes", func(t *testing.T) {
		if journalQuiescentBefore(filepath.Join(dir, "absent"), since) {
			t.Fatal("stat failure must not prune")
		}
	})
}

func TestItemIDsIntersect(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both nil", nil, nil, false},
		{"a empty", nil, []string{"1"}, false},
		{"b empty", []string{"1"}, []string{}, false},
		{"overlap", []string{"1", "2"}, []string{"3", "2"}, true},
		{"disjoint", []string{"1", "2"}, []string{"3", "4"}, false},
		{"identical", []string{"7"}, []string{"7"}, true},
		{"prefix is not a match", []string{"7"}, []string{"71"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := itemIDsIntersect(tc.a, tc.b); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

type unpushedFixture struct {
	sidecar []byte // nil: no sidecar recorded
	diff    []byte // recorded as a blob when non-nil
	touch   bool   // record a branch ref-touch after the sidecar
}

func seedUnpushedRun(t *testing.T, runsDir, runID string, at time.Time, fx unpushedFixture) string {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: "g"},
		nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	if fx.diff != nil {
		if _, err := run.RecordArtifact("implement/unpushed.diff", fx.diff); err != nil {
			t.Fatal(err)
		}
	}
	if fx.sidecar != nil {
		if _, err := run.RecordStageArtifact("implement", 1, "", "implement"+UnpushedDiffMetaArtifactSuffix, fx.sidecar); err != nil {
			t.Fatal(err)
		}
	}
	if fx.touch {
		if err := run.Append(journal.Event{Type: journal.EventRefTouched, Stage: "push", Attempt: 1,
			ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: "run/x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(runsDir, runID)
}

func unpushedSidecar(t *testing.T, schema string, items []string, diff []byte) []byte {
	t.Helper()
	var a unpushedDiffArtifact
	a.Schema = schema
	a.RunID = "prior"
	a.Stage = "implement"
	a.Attempt = 2
	a.ItemIDs = items
	a.Branch = "run/prior"
	a.BaseRef = "main"
	a.DiffBytes = len(diff)
	a.Diff.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(diff))
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUnpushedWorkFromRun(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	since := at.Add(-time.Hour)
	diff := []byte("diff --git a/x b/x\n+hello world\n")
	schema := UnpushedDiffSchemaPrefix + "v1"
	good := unpushedSidecar(t, schema, []string{"7301", "9"}, diff)
	items := []string{"7301"}

	t.Run("extracts stranded diff", func(t *testing.T) {
		runsDir := t.TempDir()
		dir := seedUnpushedRun(t, runsDir, "r1", at, unpushedFixture{sidecar: good, diff: diff})
		var warns []string
		f := &FileCrossRun{Warn: func(s string) { warns = append(warns, s) }}
		w := f.unpushedWorkFromRun(dir, items, since, 1<<20)
		if w == nil {
			t.Fatalf("expected work; warns=%v", warns)
		}
		if w.RunID != "prior" || w.Stage != "implement" || w.Attempt != 2 || w.Branch != "run/prior" || w.BaseRef != "main" {
			t.Fatalf("unexpected work: %+v", w)
		}
		if w.Diff != string(diff) || w.DiffTruncated || w.DiffBytes != len(diff) || w.DiffDigest == "" {
			t.Fatalf("unexpected diff fields: %+v", w)
		}
		if !w.RecordedAt.Equal(at) {
			t.Fatalf("RecordedAt = %v, want %v", w.RecordedAt, at)
		}
	})

	t.Run("truncates inline diff", func(t *testing.T) {
		dir := seedUnpushedRun(t, t.TempDir(), "r1", at, unpushedFixture{sidecar: good, diff: diff})
		w := (&FileCrossRun{}).unpushedWorkFromRun(dir, items, since, 5)
		if w == nil || !w.DiffTruncated || w.Diff != string(diff[:5]) {
			t.Fatalf("unexpected work: %+v", w)
		}
	})

	t.Run("missing diff blob still discoverable", func(t *testing.T) {
		dir := seedUnpushedRun(t, t.TempDir(), "r1", at, unpushedFixture{sidecar: good})
		var warns []string
		f := &FileCrossRun{Warn: func(s string) { warns = append(warns, s) }}
		w := f.unpushedWorkFromRun(dir, items, since, 1<<20)
		if w == nil || w.Diff != "" || w.DiffDigest == "" {
			t.Fatalf("unexpected work: %+v", w)
		}
		if len(warns) == 0 {
			t.Fatal("expected a warning for the unreadable diff")
		}
	})

	nilCases := []struct {
		name string
		fx   unpushedFixture
		ids  []string
		sin  time.Time
	}{
		{"no sidecar", unpushedFixture{diff: diff}, items, since},
		{"malformed sidecar", unpushedFixture{sidecar: []byte("{not json")}, items, since},
		{"wrong schema", unpushedFixture{sidecar: unpushedSidecar(t, "other/v1", []string{"7301"}, diff), diff: diff}, items, since},
		{"item mismatch", unpushedFixture{sidecar: good, diff: diff}, []string{"42"}, since},
		{"no requested items", unpushedFixture{sidecar: good, diff: diff}, nil, since},
		{"sidecar before since", unpushedFixture{sidecar: good, diff: diff}, items, at.Add(time.Hour)},
		{"published after diff", unpushedFixture{sidecar: good, diff: diff, touch: true}, items, since},
	}
	for _, tc := range nilCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedUnpushedRun(t, t.TempDir(), "r1", at, tc.fx)
			if w := (&FileCrossRun{}).unpushedWorkFromRun(dir, tc.ids, tc.sin, 1<<20); w != nil {
				t.Fatalf("expected nil, got %+v", w)
			}
		})
	}

	t.Run("not a journal directory", func(t *testing.T) {
		if w := (&FileCrossRun{}).unpushedWorkFromRun(t.TempDir(), items, since, 1<<20); w != nil {
			t.Fatalf("expected nil, got %+v", w)
		}
	})

	t.Run("unreadable events warns", func(t *testing.T) {
		dir := seedUnpushedRun(t, t.TempDir(), "r1", at, unpushedFixture{sidecar: good, diff: diff})
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("not json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if w := (&FileCrossRun{}).unpushedWorkFromRun(dir, items, since, 1<<20); w != nil {
			t.Fatalf("expected nil, got %+v", w)
		}
	})
}
