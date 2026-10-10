package childpod

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestParentCustodySeparatesBranchJoinsAndLateSurrender(t *testing.T) {
	for _, mode := range []string{"pending", "terminal", "joined", "sibling-join", "newer-attempt", "forged-branch", "duplicate-join"} {
		t.Run(mode, func(t *testing.T) {
			id := requestFixture().Identity
			id.Child = nil
			run, err := journal.Create(t.TempDir(), id, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = run.Close() }()
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			contracts := map[int]string{}
			appendMarker := func(branch int, kind, digest string) {
				t.Helper()
				if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Branch: branch, Runner: map[string]any{"kind": kind, "contractDigest": digest}}); err != nil {
					t.Fatal(err)
				}
			}
			for branch := 1; branch <= 2; branch++ {
				seq, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1, Branch: branch}, false)
				if err != nil {
					t.Fatal(err)
				}
				events, err := reader.Events()
				if err != nil {
					t.Fatal(err)
				}
				c := parentContractFixture()
				c.Identity, c.ParentOrigin, c.ParentBranch, c.PodAttempt = id, origin, branch, int(seq)
				for _, e := range events {
					if e.Seq == seq {
						c.StartedAt = e.Time
					}
				}
				data, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				digest := journal.Digest(data)
				store := ParentBlobs{RunDir: run.Dir(), Identity: id}
				if err := store.Put(t.Context(), digest, data); err != nil {
					t.Fatal(err)
				}
				appendMarker(branch, ParentWriterStarted, digest)
				contracts[branch] = digest
			}
			switch mode {
			case "terminal":
				if err := run.Append(journal.Event{Type: journal.EventRunFinished}); err != nil {
					t.Fatal(err)
				}
			case "joined", "duplicate-join":
				appendMarker(1, ParentWriterJoined, contracts[1])
				if mode == "duplicate-join" {
					appendMarker(1, ParentWriterJoined, contracts[1])
				}
			case "sibling-join":
				appendMarker(2, ParentWriterJoined, contracts[2])
			case "newer-attempt":
				if _, _, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 2, Branch: 1}, true); err != nil {
					t.Fatal(err)
				}
			case "forged-branch":
				appendMarker(2, ParentWriterJoined, contracts[1])
			}
			pending, err := ParentCustodyPending(t.Context(), reader, contracts[1])
			wantPending := mode == "pending" || mode == "terminal" || mode == "sibling-join"
			if pending != wantPending || (wantPending && err != nil) {
				t.Fatalf("pending=%v err=%v", pending, err)
			}
			wantError := mode == "newer-attempt" || mode == "forged-branch" || mode == "duplicate-join"
			if wantError != errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
				t.Fatalf("custody corruption error = %v, want refusal = %v", err, wantError)
			}
			if mode == "joined" {
				if pending, err := ParentCustodyPending(t.Context(), reader, contracts[2]); !pending || err != nil {
					t.Fatal("sibling borrowed join", pending, err)
				}
			}

		})
	}
}
