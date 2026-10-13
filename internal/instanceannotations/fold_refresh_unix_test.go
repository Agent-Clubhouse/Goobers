//go:build unix

package instanceannotations

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestFoldRefreshReturnsInstanceLogStateError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".instance-journal-id"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := new(Fold).refresh(dir); err == nil {
		t.Fatal("refresh error = nil, want invalid identity file error")
	}
}

func TestFoldRefreshReturnsReadAfterSeqError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("not json\n{\"seq\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := new(Fold).refresh(dir); err == nil {
		t.Fatal("refresh error = nil, want journal read error")
	}
}

// refreshWhileBlocked runs fold.refresh against a journal whose events file is
// a FIFO, so the tail read blocks in open until mutate has run. That holds the
// refresh between its two journal-state reads while the journal is changed.
func refreshWhileBlocked(t *testing.T, dir string, fold *Fold, mutate func(fifoPath string)) error {
	t.Helper()
	fifo := filepath.Join(dir, "events.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- fold.refresh(dir) }()
	time.Sleep(200 * time.Millisecond)
	parked := filepath.Join(dir, "parked")
	if err := os.Rename(fifo, parked); err != nil {
		t.Fatal(err)
	}
	mutate(fifo)
	release, err := os.OpenFile(parked, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release.Close() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("refresh did not return")
		return nil
	}
}

func TestFoldRefreshReturnsSecondInstanceLogStateError(t *testing.T) {
	dir := t.TempDir()
	err := refreshWhileBlocked(t, dir, new(Fold), func(string) {
		if err := os.WriteFile(filepath.Join(dir, ".instance-journal-id"), []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("refresh error = nil, want invalid identity file error")
	}
}

func TestFoldRefreshRetriesWhenJournalRotatesMidRefresh(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	dir := layout.SchedulerDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "owner", Name: "repo"}
	fold := new(Fold)
	err := refreshWhileBlocked(t, dir, fold, func(path string) {
		log, _, err := journal.OpenInstanceLog(dir)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = log.Close() }()
		if err := log.Append(itemRepoAnnotationEvent("run-1", "17", repo)); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := recordedRepo(fold, dir, "run-1", "17"); err != nil || got != repo {
		t.Fatalf("repo after rotation retry = %+v, %v; want %+v", got, err, repo)
	}
}

func TestFoldRefreshStopsWhenJournalVanishesMidRefresh(t *testing.T) {
	dir := t.TempDir()
	fold := new(Fold)
	err := refreshWhileBlocked(t, dir, fold, func(string) {})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err == nil && fold.itemRepos != nil {
		t.Fatalf("itemRepos = %v, want reset", fold.itemRepos)
	}
}
