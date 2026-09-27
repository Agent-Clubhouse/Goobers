package executor

import (
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestTrustedJournalPlaneReachesCLIContext(t *testing.T) {
	exec := &ShellExecutor{}
	env := apiv1.InvocationEnvelope{RunID: "run-1", TaskID: "run-1:merge-pr"}
	ctx := WithJournalPlane(t.Context(), JournalPlane{Endpoint: "http://daemon", Token: "scoped-journal-only"})
	actual := exec.runContextEnv(ctx, env)
	if !slices.Contains(actual, "GOOBERS_JOURNAL_ENDPOINT=http://daemon") || !slices.Contains(actual, "GOOBERS_JOURNAL_TOKEN=scoped-journal-only") {
		t.Fatalf("missing authoritative plane: %v", actual)
	}
	for _, value := range exec.runContextEnv(t.Context(), env) {
		if value == "GOOBERS_JOURNAL_TOKEN=scoped-journal-only" {
			t.Fatal("plane leaked between run contexts")
		}
	}
}
