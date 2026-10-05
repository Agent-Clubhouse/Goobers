package runner

import (
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestOwnedBranchRecorderRetainsConcurrentAttribution(t *testing.T) {
	_, run, _ := childOriginRuntime(t, &childOriginGoober{})
	var group sync.WaitGroup
	for branch := 1; branch <= 2; branch++ {
		group.Go(func() {
			recorder, err := OwnedBranchRecorder(run, branch)
			if err != nil {
				t.Error(err)
				return
			}
			owned, actual, err := OwnedJournalScope(recorder)
			if err != nil || actual != branch {
				t.Error(actual, err)
				return
			}
			if err := owned.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "work", Runner: map[string]any{"kind": "owned-scope"}}); err != nil {
				t.Error(err)
			}
			if _, err := owned.RecordArtifactBoundedWithIntegrity("owned.txt", []byte{byte(branch)}, apiv1.IntegrityTrusted, 16); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	reader, _ := journal.OpenReadOnly(run.Dir())
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]int{}
	for _, event := range events {
		if event.Runner["kind"] == "owned-scope" || event.Type == journal.EventArtifactRecorded && event.Name == "owned.txt" {
			seen[event.Branch]++
		}
	}
	if seen[1] != 2 || seen[2] != 2 || seen[0] != 0 {
		t.Fatal(seen)
	}
	// Structural similarity is not proof that the runner supplied this writer.
	wrapped := struct{ *journal.Run }{run}
	if _, _, err := OwnedJournalScope(wrapped); err == nil {
		t.Fatal("foreign wrapper accepted")
	}
}
