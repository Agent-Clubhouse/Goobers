package interactiveaccess

import (
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

func TestHumanRestartAuthorityBindsChildEpochWithoutDelegation(t *testing.T) {
	root := t.TempDir()
	source := journal.RunIdentity{RunID: strings.Repeat("a", 32), Gaggle: "own", Workflow: "generated", WorkflowDigest: journal.Digest([]byte("machine")), GooberDigest: journal.Digest([]byte("goober")), ConfigGeneration: journal.Digest([]byte("config"))}
	source.Child = &journal.ChildLineage{Gaggle: source.Gaggle, ParentRunID: strings.Repeat("b", 32), ParentWorkflow: "parent", StageOccurrence: "work/visit-1", InvocationKey: "key", AcceptanceID: "trigger-" + source.RunID, SourceDigest: journal.Digest([]byte("source")), EnvelopeDigest: journal.Digest([]byte("envelope"))}
	run, err := journal.Create(root, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(filepath.Join(root, source.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	actor := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	a, err := NewRestartAuthority(actor, source, strings.Repeat("c", 32), "work", events[len(events)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	child := *source.Child
	child.ExecutionEpoch = 1
	child.PriorResultRef = journal.Digest([]byte("prior result"))
	child.RestartDigest = journal.Digest([]byte("admitted plan"))
	next, err := journal.CreateContinuation(root, journal.ContinuationRequest{RunID: a.EpochID, SourceRunID: a.SourceRunID, ExpectedTerminalSeq: a.SourceTerminalSeq, Operator: "issuer:human", Target: a.Stage, ChildContinuation: &child, Inputs: map[string][]byte{RestartAuthorityInputName: raw}, InputIntegrity: map[string]apiv1.Integrity{RestartAuthorityInputName: apiv1.IntegrityTrusted}, InputSource: map[string]string{RestartAuthorityInputName: "issuer:human"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
	rd, err = journal.OpenReadOnly(filepath.Join(root, a.EpochID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := LoadRestartAuthority(rd, id); err != nil || got.EpochID != id.RunID {
		t.Fatal(got, err)
	}
	for _, mode := range []string{"unlinked", "actor", "result"} {
		t.Run(mode, func(t *testing.T) {
			changed := id
			lineage := *id.Child
			changed.Child = &lineage
			switch mode {
			case "unlinked":
				lineage.ExecutionEpoch = 0
			case "actor":
				changed.Operator = "another"
			case "result":
				lineage.PriorResultRef = ""
			}
			if _, err := LoadRestartAuthority(rd, changed); err == nil {
				t.Fatal("unbound child authority accepted")
			}
		})
	}
}
