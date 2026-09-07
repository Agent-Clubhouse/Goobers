package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestRunnerProjectsSidecarMergeConfirmation(t *testing.T) {
	workspace := t.TempDir()
	confirmation := providers.MergeConfirmation{RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", MergeSHA: "commit"}
	data, err := json.Marshal(mutationFact{Provider: "github", Kind: "pr", ID: "9", Operation: "merge", MergeConfirmation: &confirmation})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, mutationsSidecarFile), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	facts, issues := readMutationSidecar(workspace)
	if len(issues) != 0 || len(facts) != 1 {
		t.Fatalf("sidecar = %+v, issues=%v", facts, issues)
	}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "merge-proof", Workflow: "landing", Gaggle: "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	done := make(chan error)
	close(done)
	heartbeat := stageHeartbeat{stop: make(chan struct{}), done: done}
	if err := finishTaskDispatch(run, heartbeat, "land", 1, journal.AttemptPolicy, facts, nil); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v, error=%v", events, err)
	}
	data, err = json.Marshal(events[1].Runner["mergeConfirmation"])
	if err != nil {
		t.Fatal(err)
	}
	var got providers.MergeConfirmation
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != confirmation || events[1].ExternalRef == nil || events[1].ExternalRef.ID != got.PullID {
		t.Fatalf("journal dropped/crossed repository identity: %+v, event=%+v", got, events[1])
	}
}
