package workbench

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func suggestionFixture(t *testing.T) (SourceSet, RelationshipSuggestion) {
	t.Helper()
	set := metadataFixture(t)
	from := NodeRef{GaggleID: "web", SourceBindingID: "backlog", Kind: "work-item", SourceID: "123"}
	to := NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: objectiveID}
	nativeDigest, _ := SourceTargetDigest(set.Scope, set.Sources[0])
	docDigest, _ := SourceTargetDigest(set.Scope, set.Sources[1])
	return set, RelationshipSuggestion{Kind: "contributes-to", Rationale: "The item implements the objective's retry policy.",
		From: SuggestionEndpoint{Ref: &from, Evidence: &SuggestionEvidence{SourceTargetDigest: nativeDigest, NativeRevision: "42"}},
		To:   SuggestionEndpoint{Ref: &to, Evidence: &SuggestionEvidence{SourceTargetDigest: docDigest, Path: "objectives/payments.md", RepositoryRevision: &SuggestionRepositoryRevision{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}}},
	}
}
func suggestionBytes(t *testing.T, values ...RelationshipSuggestion) []byte {
	t.Helper()
	raw, err := json.Marshal(RelationshipSuggestions{SchemaVersion: SuggestionSchemaVersion, Suggestions: values})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func suggestionOrigin(raw []byte) SuggestionOrigin {
	return SuggestionOrigin{RunID: "run-real", StageID: "curate", Attempt: 1, ArtifactPath: "suggestions.json", ArtifactDigest: metadataContentDigest(raw)}
}

func TestSuggestionsBindDeduplicatesByEvidenceWithoutCreatingAcceptedEdges(t *testing.T) {
	set, value := suggestionFixture(t)
	repeated := value
	repeated.Rationale = "Another rationale for the same relationship."
	raw := suggestionBytes(t, value, repeated)
	origin := suggestionOrigin(raw)
	bound, err := BindSuggestions(raw, set, origin)
	if err != nil || len(bound) != 1 || bound[0].Proposal.Rationale != value.Rationale || bound[0].Origin != origin {
		t.Fatal(bound, err)
	}
	origin.RunID = "another-run"
	other, err := BindSuggestions(raw, set, origin)
	if err != nil || other[0].Key != bound[0].Key {
		t.Fatal("concrete proposal incorrectly scoped to producer", err)
	}
	edge, evidence, err := MaterializeSuggestion(set, bound[0], nil)
	if err != nil || edge.From != *value.From.Ref || edge.To != *value.To.Ref || len(evidence) != 2 || set.Scope.ValidateEdge(edge) != nil {
		t.Fatal(edge, evidence, err)
	}
	value.From.Evidence.NativeRevision = "43"
	raw = suggestionBytes(t, value)
	changed, err := BindSuggestions(raw, set, suggestionOrigin(raw))
	if err != nil || changed[0].Key == bound[0].Key {
		t.Fatal("revision did not change suggestion identity", err)
	}
	newEdge, _, err := MaterializeSuggestion(set, changed[0], nil)
	if err != nil || newEdge.EdgeID != edge.EdgeID {
		t.Fatal("evidence refresh changed relationship identity", err)
	}
}

func TestSuggestionsRejectUnboundedForgedAndStaleArtifacts(t *testing.T) {
	cases := map[string]func(*RelationshipSuggestion){
		"foreign gaggle":           func(v *RelationshipSuggestion) { v.From.Ref.GaggleID = "elsewhere" },
		"unknown binding":          func(v *RelationshipSuggestion) { v.From.Ref.SourceBindingID = "unknown" },
		"forged target":            func(v *RelationshipSuggestion) { v.From.Evidence.SourceTargetDigest = strings.Repeat("f", 64) },
		"missing native revision":  func(v *RelationshipSuggestion) { v.From.Evidence.NativeRevision = "" },
		"native repository pins":   func(v *RelationshipSuggestion) { v.From.Evidence.RepositoryRevision = v.To.Evidence.RepositoryRevision },
		"document native revision": func(v *RelationshipSuggestion) { v.To.Evidence.NativeRevision = "42" },
		"undeclared path":          func(v *RelationshipSuggestion) { v.To.Evidence.Path = "secret.md" },
		"invalid blob":             func(v *RelationshipSuggestion) { v.To.Evidence.RepositoryRevision.BlobID = "not-a-blob" },
		"wrong source kind":        func(v *RelationshipSuggestion) { v.To.Ref.Kind = "work-item" },
		"journal relation":         func(v *RelationshipSuggestion) { v.Kind = "observed-in-run" },
		"hierarchy to document":    func(v *RelationshipSuggestion) { v.Kind = "parent-of" },
		"empty rationale":          func(v *RelationshipSuggestion) { v.Rationale = "" },
		"large rationale":          func(v *RelationshipSuggestion) { v.Rationale = strings.Repeat("a", 4097) },
		"mixed endpoint": func(v *RelationshipSuggestion) {
			v.From.Creation = &SuggestionCreation{SourceBindingID: "backlog", RequestID: "create-1"}
		},
		"missing evidence": func(v *RelationshipSuggestion) { v.From.Evidence = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			set, value := suggestionFixture(t)
			change(&value)
			if _, err := ParseSuggestions(suggestionBytes(t, value), set); err == nil {
				t.Fatal("invalid suggestion accepted")
			}
		})
	}
	set, value := suggestionFixture(t)
	valid := suggestionBytes(t, value)
	malformed := [][]byte{
		bytes.Replace(valid, []byte(`"schemaVersion":`), []byte(`"schemaVersion":"wrong","schemaVersion":`), 1),
		bytes.Replace(valid, []byte(`"kind":"contributes-to"`), []byte(`"kind":"contributes-to","Kind":"references"`), 1),
		bytes.Replace(valid, []byte(`"suggestions":`), []byte(`"actor":"operator","suggestions":`), 1),
		append(append([]byte(nil), valid...), []byte(` {}`)...),
		[]byte("schemaVersion: relationship-suggestions/v1\nsuggestions: []"),
		suggestionBytes(t, make([]RelationshipSuggestion, MaxSuggestions+1)...),
		bytes.Repeat([]byte(" "), MaxSuggestionBytes+1),
	}
	for i, raw := range malformed {
		if _, err := ParseSuggestions(raw, set); err == nil {
			t.Fatalf("accepted malformed artifact %d", i)
		}
	}
	origin := suggestionOrigin(valid)
	origin.ArtifactDigest = strings.Repeat("0", 64)
	if _, err := BindSuggestions(valid, set, origin); err == nil {
		t.Fatal("forged provenance accepted")
	}
	set.Sources[0].BacklogIdentity.Name = "different"
	if _, err := ParseSuggestions(valid, set); err == nil {
		t.Fatal("rebound source retained old suggestion")
	}
}

func TestSuggestionsProvisionalIdentityNeedsExactCreationReceipt(t *testing.T) {
	set, value := suggestionFixture(t)
	actualRef, actualEvidence := *value.From.Ref, *value.From.Evidence
	creation := SuggestionCreation{SourceBindingID: "backlog", RequestID: "create-1"}
	value.From = SuggestionEndpoint{Creation: &creation}
	raw := suggestionBytes(t, value)
	origin := suggestionOrigin(raw)
	bound, err := BindSuggestions(raw, set, origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := MaterializeSuggestion(set, bound[0], nil); err == nil {
		t.Fatal("uncreated item materialized")
	}
	different := origin
	different.Attempt++
	rebound, err := BindSuggestions(raw, set, different)
	if err != nil || rebound[0].Key == bound[0].Key {
		t.Fatal("provisional identity crossed attempt", err)
	}
	receipt := SuggestionCreationResolution{Origin: origin, Creation: creation, CommandID: "command-created", Ref: actualRef, Evidence: actualEvidence}
	edge, _, err := MaterializeSuggestion(set, bound[0], []SuggestionCreationResolution{receipt})
	if err != nil || edge.From != actualRef {
		t.Fatal(edge, err)
	}
	for name, change := range map[string]func(*SuggestionCreationResolution){
		"origin":          func(r *SuggestionCreationResolution) { r.Origin.RunID = "another" },
		"request":         func(r *SuggestionCreationResolution) { r.Creation.RequestID = "create-2" },
		"unconfirmed":     func(r *SuggestionCreationResolution) { r.CommandID = "" },
		"wrong item kind": func(r *SuggestionCreationResolution) { r.Ref.Kind = "milestone" },
		"foreign binding": func(r *SuggestionCreationResolution) { r.Ref.SourceBindingID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			r := receipt
			change(&r)
			if _, _, err := MaterializeSuggestion(set, bound[0], []SuggestionCreationResolution{r}); err == nil {
				t.Fatal("invalid creation receipt accepted")
			}
		})
	}
	if _, _, err := MaterializeSuggestion(set, bound[0], []SuggestionCreationResolution{receipt, receipt}); err == nil {
		t.Fatal("ambiguous creation accepted")
	}
	bound[0].Proposal.Rationale = "Altered rationale is still review text."
	bound[0].Proposal.Kind = "references"
	if _, _, err := MaterializeSuggestion(set, bound[0], []SuggestionCreationResolution{receipt}); err == nil {
		t.Fatal("changed relationship accepted with prior key")
	}
}
