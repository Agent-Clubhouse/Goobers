package interactiveaccess

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

func authorityFixture(t *testing.T) (RestartAuthority, *journal.Reader, journal.RunIdentity, string) {
	t.Helper()
	root := t.TempDir()
	source := journal.RunIdentity{RunID: "source", Gaggle: "web", Workflow: "work", WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers"))}
	run, err := journal.Create(root, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(root, source.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "https://identity.test:443", Subject: "user:alice", Roles: []httpapi.Role{httpapi.RoleOperate, httpapi.RoleView}, Groups: []string{"team-z", "team-a", "team-a"}, Name: "not-retained"}
	a, err := NewRestartAuthority(p, source, "epoch", "repair", events[len(events)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	ref := p.Issuer + ":" + p.Subject
	run, err = journal.CreateContinuation(root, journal.ContinuationRequest{RunID: a.EpochID, SourceRunID: source.RunID, ExpectedTerminalSeq: a.SourceTerminalSeq, Operator: ref, Target: a.Stage, Inputs: map[string][]byte{RestartAuthorityInputName: raw}, InputIntegrity: map[string]apiv1.Integrity{RestartAuthorityInputName: apiv1.IntegrityTrusted}, InputSource: map[string]string{RestartAuthorityInputName: ref}})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, a.EpochID)
	reader, err = journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return a, reader, id, dir
}

func TestRestartAuthorityRetainsSeparateHumanClaimsAndExactEpoch(t *testing.T) {
	a, reader, id, _ := authorityFixture(t)
	loaded, err := LoadRestartAuthority(reader, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, loaded) || loaded.Principal().Issuer != "https://identity.test:443" || loaded.Principal().Subject != "user:alice" || !reflect.DeepEqual(loaded.Groups, []string{"team-a", "team-z"}) {
		t.Fatalf("changed authority: %+v", loaded)
	}
	p := loaded.Principal()
	p.Groups[0] = "changed"
	if loaded.Groups[0] != "team-a" {
		t.Fatal("principal aliases retained claims")
	}
	raw, err := loaded.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("not-retained")) {
		t.Fatal("display claim retained as authority")
	}
	for name, change := range map[string]func(*journal.RunIdentity){
		"epoch":    func(i *journal.RunIdentity) { i.RunID = "different" },
		"source":   func(i *journal.RunIdentity) { i.ContinuedFromRunID = "different" },
		"sequence": func(i *journal.RunIdentity) { i.SourceTerminalSeq++ },
		"gaggle":   func(i *journal.RunIdentity) { i.Gaggle = "other" },
		"stage":    func(i *journal.RunIdentity) { i.RequestedTarget = "other" },
		"workflow": func(i *journal.RunIdentity) { i.WorkflowDigest = journal.Digest([]byte("different")) },
		"goober":   func(i *journal.RunIdentity) { i.GooberDigest = journal.Digest([]byte("different")) },
		"actor":    func(i *journal.RunIdentity) { i.Operator = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := id
			change(&changed)
			if _, err := LoadRestartAuthority(reader, changed); err == nil {
				t.Fatal("foreign authority accepted")
			}
		})
	}
}

func TestRestartAuthorityRefusesUntrustedOrCorruptedInput(t *testing.T) {
	_, reader, id, dir := authorityFixture(t)
	id.Inputs[0].Integrity = apiv1.IntegrityUnapproved
	if _, err := LoadRestartAuthority(reader, id); err == nil {
		t.Fatal("untrusted input accepted")
	}
	id.Inputs[0].Integrity = apiv1.IntegrityTrusted
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(id.Inputs[0].Ref.Path)), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRestartAuthority(reader, id); err == nil {
		t.Fatal("corrupted authority accepted")
	}
}

func TestRestartAuthorityRejectsNonHumanAndNonCanonicalRecords(t *testing.T) {
	a, _, _, _ := authorityFixture(t)
	source := journal.RunIdentity{RunID: a.SourceRunID, Gaggle: a.Gaggle, WorkflowDigest: a.WorkflowDigest, GooberDigest: a.GooberDigest}
	for _, p := range []httpapi.Principal{
		{Issuer: "goobers/pod", Subject: "pod", Roles: []httpapi.Role{httpapi.RoleAdmin}},
		{Issuer: a.Issuer, Subject: a.Subject, Roles: []httpapi.Role{httpapi.RoleView}},
		{Issuer: a.Issuer, Subject: a.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}, Scopes: []string{"credentials"}},
	} {
		if _, err := NewRestartAuthority(p, source, a.EpochID, a.Stage, a.SourceTerminalSeq); err == nil {
			t.Fatal("nonhuman authority accepted")
		}
	}
	raw, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{string(raw) + " {}", strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(raw), `"version":1`, `"version":1,"token":"fake"`, 1), " " + string(raw)} {
		if _, err := ParseRestartAuthority([]byte(changed)); err == nil {
			t.Fatal("noncanonical authority accepted")
		}
	}
}
