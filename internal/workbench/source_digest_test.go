package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSourceDigestPreservesExistingCursorRecipes(t *testing.T) {
	scope := Scope{GaggleID: "g", Bindings: map[string]bool{"items": true, "docs": true}}
	target := apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "org", Name: "repo"}
	sources := []BoundSource{
		{Spec: apiv1.WorkbenchSource{Name: "items", Kind: "backlog", Objectives: &apiv1.WorkbenchObjectiveSelector{IDs: []string{}, Types: []string{"Epic"}}}, BacklogIdentity: target},
		{Spec: apiv1.WorkbenchSource{Name: "docs", Kind: "documents", Repository: &target, Paths: []string{"one.md", "two.md"}}, Repository: apiv1.RepoRef{Branch: "main"}},
	}
	for _, source := range sources {
		var previous any
		if source.Spec.Kind == "backlog" {
			previous = struct {
				Gaggle, Binding string
				Target          apiv1.InteractiveRepositoryIdentity
				Objectives      apiv1.WorkbenchObjectiveSelector
			}{"g", "items", target, apiv1.WorkbenchObjectiveSelector{Types: []string{"Epic"}}}
		} else {
			previous = struct {
				Gaggle, Binding, Kind, Branch string
				Target                        apiv1.InteractiveRepositoryIdentity
				Paths                         []string
			}{"g", "docs", "documents", "main", target, []string{"one.md", "two.md"}}
		}
		raw, _ := json.Marshal(previous)
		sum := sha256.Sum256(raw)
		actual, err := SourceTargetDigest(scope, source)
		if err != nil || actual != hex.EncodeToString(sum[:]) {
			t.Fatalf("cursor changed: %s %v", actual, err)
		}
	}
}
func TestMutationTargetIdentityExcludesClassificationAndWriteAllowlist(t *testing.T) {
	scope := Scope{GaggleID: "g", Bindings: map[string]bool{"items": true, "other": true}}
	source := BoundSource{Spec: apiv1.WorkbenchSource{Name: "items", Kind: "backlog"}, BacklogIdentity: apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "org", Name: "repo"}}
	mutation, _ := BacklogMutationTargetDigest(scope, source)
	read, _ := SourceTargetDigest(scope, source)
	source.Spec.Objectives = &apiv1.WorkbenchObjectiveSelector{Types: []string{"Epic"}}
	source.Spec.Writes = &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title"}}
	changed, _ := BacklogMutationTargetDigest(scope, source)
	classified, _ := SourceTargetDigest(scope, source)
	if mutation != changed || read == classified {
		t.Fatal("source classification affected physical mutation identity")
	}
	for _, mutate := range []func(*BoundSource){func(s *BoundSource) { s.BacklogIdentity.Name = "other" }, func(s *BoundSource) { s.BacklogIdentity.Owner = "other" }, func(s *BoundSource) { s.Spec.Name = "other" }} {
		copy := source
		mutate(&copy)
		digest, err := BacklogMutationTargetDigest(scope, copy)
		if err != nil || digest == mutation {
			t.Fatal("physical source identity not bound", err)
		}
	}
	other := scope
	other.GaggleID = "other"
	digest, _ := BacklogMutationTargetDigest(other, source)
	if digest == mutation {
		t.Fatal("gaggle not bound")
	}
}
