package childpod

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func parentGitContractFixture(t *testing.T) Contract {
	t.Helper()
	c := parentContractFixture()
	object := strings.Repeat("a", 40)
	ref, err := recovery.RefForSnapshot(c.Identity.RunID, object)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("opaque bounded carrier")
	record := recovery.Record{Version: 1, RunID: c.Identity.RunID, RepositoryKey: (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}).CanonicalKey(), Ref: ref, BaseSHA: object, SnapshotSHA: object, PatchDigest: journal.Digest(nil), ArchiveDigest: journal.Digest(data), ArchiveFormat: "full", ArchiveBytes: int64(len(data)), CreatedAt: c.StartedAt, RetainUntil: c.StartedAt.Add(24 * time.Hour)}
	carrier := Carrier{Snapshot: recovery.PortableSnapshot{Record: record, TreeSHA: object}, Bundle: data}
	c.Workspace, c.GitState = &carrier, &GitState{Head: carrier, Index: carrier}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParentGitStateCannotBeDroppedOrCrossRole(t *testing.T) {
	c := parentGitContractFixture(t)
	for name, change := range map[string]func(*Contract){
		"missing state": func(c *Contract) { c.GitState = nil },
		"scratch state": func(c *Contract) { c.Workspace = nil },
		"child role": func(c *Contract) {
			c.ParentOrigin, c.ParentBranch = nil, 0
			c.Identity = requestFixture().Identity
		},
		"foreign source": func(c *Contract) {
			c.GitState.Head.Snapshot.Record.RepositoryKey = (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "other"}).CanonicalKey()
		},
		"different capture":   func(c *Contract) { c.GitState.Index.Snapshot.Record.CreatedAt = c.StartedAt.Add(time.Second) },
		"different omissions": func(c *Contract) { c.GitState.Index.Snapshot.Policy.ExcludedPaths = []string{"private"} },
		"tampered bundle":     func(c *Contract) { c.GitState.Head.Bundle = []byte("replacement") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := c
			state := *c.GitState
			changed.GitState = &state
			change(&changed)
			data, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeContract(data, journal.Digest(data)); err == nil {
				t.Fatal("invalid parent state accepted")
			}
		})
	}
}

func TestParentOutputRequiresGitStateAndExactCapture(t *testing.T) {
	c := parentGitContractFixture(t)
	digest := journal.Digest([]byte("contract"))
	for _, valid := range []bool{true, false} {
		out := Output{Version: 1, ContractDigest: digest, Workspace: c.Workspace}
		if valid {
			out.GitState = c.GitState
		}
		data, _ := json.Marshal(out)
		_, err := DecodeOutput(data, journal.Digest(data), digest, c)
		if (err == nil) != valid {
			t.Fatalf("output state validation: valid=%v, error=%v", valid, err)
		}
	}
	foreign := parentGitContractFixture(t)
	foreign.Workspace.Snapshot.Record.CreatedAt = c.StartedAt.Add(time.Second)
	foreign.GitState.Head.Snapshot.Record.CreatedAt = foreign.Workspace.Snapshot.Record.CreatedAt
	foreign.GitState.Index.Snapshot.Record.CreatedAt = foreign.Workspace.Snapshot.Record.CreatedAt
	out := Output{Version: 1, ContractDigest: digest, Workspace: foreign.Workspace, GitState: foreign.GitState}
	data, _ := json.Marshal(out)
	if _, err := DecodeOutput(data, journal.Digest(data), digest, c); err == nil {
		t.Fatal("output substituted an internally consistent foreign capture")
	}
}
