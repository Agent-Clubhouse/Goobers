package runner

import (
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildCredentialCustodyReopensAndRejectsTamperedSource(t *testing.T) {
	in := childWorkspaceStart(nil)
	ceiling := credentials.NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"})
	in.ChildCredentials = &ceiling
	inputs := map[string][]byte{}
	integrity := map[string]apiv1.Integrity{}
	if err := pinChildCredentials(&in, inputs, integrity); err != nil {
		t.Fatal(err)
	}
	ceiling.AllowedKeys[0] = "repo:push"
	if in.ChildCredentials.AllowedKeys[0] != "agent:model" {
		t.Fatal("caller mutated admitted ceiling")
	}
	id := journal.RunIdentity{RunID: in.RunID, Gaggle: in.Gaggle, Workflow: "generated", ConfigGeneration: journal.Digest([]byte("config")), WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers")), Child: in.Child}
	jr, err := journal.Create(t.TempDir(), id, inputs, journal.WithInputIntegrity(integrity))
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err = rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	var resumed StartInput
	if err := restoreChildCredentials(rd, id, &resumed); err != nil {
		t.Fatal(err)
	}
	ctx, err := childCredentialContext(t.Context(), StartInput{Child: id.Child, ChildCredentials: resumed.ChildCredentials})
	if err != nil {
		t.Fatal(err)
	}
	if keys := credentials.FilterChildCredentialKeys(ctx, []string{"repo:push", "agent:model"}); len(keys) != 1 || keys[0] != "agent:model" {
		t.Fatalf("resumed keys=%v", keys)
	}
	id.Child.SourceDigest = journal.Digest([]byte("foreign"))
	if _, err := PinnedChildCredentials(rd, id); err == nil {
		t.Fatal("foreign source accepted stored ceiling")
	}
	id.Child.SourceDigest = in.Child.SourceDigest
	for _, input := range id.Inputs {
		if input.Name == childCredentialInput {
			if err := os.WriteFile(filepath.Join(jr.Dir(), input.Ref.Path), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := PinnedChildCredentials(rd, id); err == nil {
		t.Fatal("tampered ceiling accepted")
	}
}
