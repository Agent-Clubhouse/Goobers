package projector

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestRegressionUnpublishedFirstRootDoesNotHidePublishedRun(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	id := "run-backend"
	if err := os.Mkdir(filepath.Join(first, id), 0700); err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(second, journal.RunIdentity{RunID: id, Gaggle: "g", Workflow: "wf"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	p := &Projector{}
	_, found, err := p.prepare(context.Background(), id, []string{first, second})
	if err != nil || !found {
		t.Fatalf("published journal hidden by an unpublished directory in first root: found=%v err=%v", found, err)
	}
}
