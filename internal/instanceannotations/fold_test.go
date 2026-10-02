package instanceannotations

import (
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// itemRepoAnnotationEvent builds a journal.EventRunnerAnnotation the fold's
// itemRepositories records, used below as the vehicle for exercising the
// fold's generic reset/compaction/restart behavior (any annotation kind the
// fold understands would do; failure-streak moved off this fold entirely
// onto the scheduler-state KV plane per Goobers#3025).
func itemRepoAnnotationEvent(runID, itemID string, repo providers.RepositoryRef) journal.Event {
	return journal.Event{Type: journal.EventRunnerAnnotation, RunID: runID, Runner: map[string]any{
		"annotation": ItemRepositoryAnnotation, "key": ItemRepositoryKey(runID, itemID), "itemId": itemID,
		"provider": string(repo.Provider), "owner": repo.Owner, "name": repo.Name, "kind": "issue",
	}}
}

func recordedRepo(fold *Fold, schedulerDir, runID, itemID string) (providers.RepositoryRef, error) {
	found, err := fold.ItemRepositories(schedulerDir, runID, []string{itemID})
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	return found[itemID].Repository, nil
}

func TestInstanceAnnotationFoldResetsAcrossCompactionRestartAndRemoval(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return old }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(itemRepoAnnotationEvent("run-1", "17", repo)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	log, _, err = journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithClock(func() time.Time { return recent }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	fold := ForInstance(layout.SchedulerDir())
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != repo {
		t.Fatalf("pre-compaction repo = %+v, %v; want %+v", got, err, repo)
	}
	if _, err := journal.CompactInstanceEvents(layout.SchedulerDir(), recent.Add(-time.Hour), recent.Add(-time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("post-compaction retained repo = %+v, %v; want zero value", got, err)
	}
	if got, err := recordedRepo(new(Fold), layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("restart repo = %+v, %v; want zero value", got, err)
	}
	if err := os.RemoveAll(layout.SchedulerDir()); err != nil {
		t.Fatal(err)
	}
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("removed-journal repo = %+v, %v; want zero value", got, err)
	}
}

func TestInstanceAnnotationFoldResetsWhenJournalIsRecreatedBeforeNextRead(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(itemRepoAnnotationEvent("run-1", "17", repo)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	fold := new(Fold)
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != repo {
		t.Fatalf("initial repo = %+v, %v; want %+v", got, err, repo)
	}

	// Recreate generation zero without giving the fold an opportunity to see
	// the journal while it is absent. Path+generation alone cannot distinguish
	// this from the former file.
	if err := os.RemoveAll(layout.SchedulerDir()); err != nil {
		t.Fatal(err)
	}
	log, _, err = journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := recordedRepo(fold, layout.SchedulerDir(), "run-1", "17"); err != nil || got != (providers.RepositoryRef{}) {
		t.Fatalf("recreated-journal repo = %+v, %v; want zero value", got, err)
	}
}

func TestRetentionOwnershipRejectsAmbiguousOrIncompleteRepository(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	event := itemRepoAnnotationEvent("run", "17", repo)
	fold := &Fold{}
	fold.apply([]journal.Event{event})
	if fold.itemReposByRun["run"]["17"].Repository.Provider != "" {
		t.Fatal("legacy ownership authorized")
	}
	// A fresh fold accepts a complete record, but conflicting history is sticky.
	fold = &Fold{}
	event.Runner["repositoryKey"] = repo.CanonicalKey()
	fold.apply([]journal.Event{event})
	if fold.itemReposByRun["run"]["17"].Repository != repo {
		t.Fatal("complete ownership rejected")
	}
	changed := itemRepoAnnotationEvent("run", "17", providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "another", Name: "repo"})
	changed.Runner["repositoryKey"] = "different-host-or-repository"
	fold.apply([]journal.Event{changed, event})
	if fold.itemReposByRun["run"]["17"].Repository.Provider != "" {
		t.Fatal("ambiguous historical ownership authorized")
	}
}
