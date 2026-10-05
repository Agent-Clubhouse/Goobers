package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SuggestionOrigin is supplied by the host from the actual emitting artifact.
// Never decode it from model output or use it as an authorization credential.
type SuggestionOrigin struct {
	RunID          string `json:"runId"`
	StageID        string `json:"stageId"`
	Attempt        int    `json:"attempt"`
	ArtifactPath   string `json:"artifactPath"`
	ArtifactDigest string `json:"artifactDigest"`
}

// BoundSuggestion is reviewable evidence. It is excluded from source graph input.
// Key deduplicates normalized relationship and evidence; rationale and producer
// changes do not turn a repeated concrete proposal into another relationship.
type BoundSuggestion struct {
	Key      string                 `json:"key"`
	Proposal RelationshipSuggestion `json:"proposal"`
	Origin   SuggestionOrigin       `json:"origin"`
}

// MatchesSuggestionKey checks normalized evidence integrity only. It grants no
// source visibility and does not replace current ParseSuggestions validation.
func MatchesSuggestionKey(bound BoundSuggestion) bool {
	return bound.Key == suggestionKey(bound.Proposal, bound.Origin)
}

// BindSuggestions verifies artifact bytes against host provenance and collapses
// repeated candidates in artifact order. The host must authorize the actual
// producer and configured source namespace before projecting these results.
func BindSuggestions(raw []byte, set SourceSet, origin SuggestionOrigin) ([]BoundSuggestion, error) {
	if !textValue(origin.RunID, 256) || !textValue(origin.StageID, 128) || origin.Attempt < 1 || !validSourcePath(origin.ArtifactPath) || origin.ArtifactDigest != metadataContentDigest(raw) {
		return nil, ErrSuggestion
	}
	artifact, err := ParseSuggestions(raw, set)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := make([]BoundSuggestion, 0, len(artifact.Suggestions))
	for _, value := range artifact.Suggestions {
		key := suggestionKey(value, origin)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, BoundSuggestion{Key: key, Proposal: value, Origin: origin})
	}
	return result, nil
}

func suggestionKey(value RelationshipSuggestion, origin SuggestionOrigin) string {
	value.Rationale = ""
	// Existing source references deduplicate across runs. A provisional creation
	// request is meaningful only in its actual producing execution occurrence.
	var creationScope any
	if value.From.Creation != nil || value.To.Creation != nil {
		creationScope = struct {
			Run, Stage string
			Attempt    int
		}{origin.RunID, origin.StageID, origin.Attempt}
	}
	raw, _ := json.Marshal(struct {
		Proposal      RelationshipSuggestion
		CreationScope any
	}{value, creationScope})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SuggestionCreationResolution is supplied only after inspecting a confirmed
// durable creation receipt for the exact origin and request. It is not model input.
type SuggestionCreationResolution struct {
	Origin    SuggestionOrigin
	Creation  SuggestionCreation
	CommandID string
	Ref       NodeRef
	Evidence  SuggestionEvidence
}

// MaterializeSuggestion resolves provisional endpoints but does not authorize or
// apply an edge. A caller must freshly verify both source observations, select the
// faithful owner and pass the result through the ordinary idempotent command path.
// Edge identity derives from the normalized relationship, not its evidence epoch.
func MaterializeSuggestion(set SourceSet, bound BoundSuggestion, resolutions []SuggestionCreationResolution) (Edge, []SuggestionEvidence, error) {
	if bound.Key != suggestionKey(bound.Proposal, bound.Origin) || len(resolutions) > 2 || validateSuggestion(set, bound.Proposal) != nil {
		return Edge{}, nil, ErrSuggestion
	}
	from, err := resolveSuggestionEndpoint(set, bound, bound.Proposal.From, resolutions)
	if err != nil {
		return Edge{}, nil, err
	}
	to, err := resolveSuggestionEndpoint(set, bound, bound.Proposal.To, resolutions)
	if err != nil {
		return Edge{}, nil, err
	}
	edge := Edge{Kind: bound.Proposal.Kind, From: *from.Ref, To: *to.Ref, Rationale: bound.Proposal.Rationale}
	raw, _ := json.Marshal(struct{ Kind, From, To string }{edge.Kind, edge.From.Key(), edge.To.Key()})
	sum := sha256.Sum256(raw)
	// A stable UUID-shaped ID prevents a refreshed suggestion from manufacturing
	// multiple persistent identities for the same directed relationship.
	sum[6] = (sum[6] & 0x0f) | 0x80
	sum[8] = (sum[8] & 0x3f) | 0x80
	edge.EdgeID = fmt.Sprintf("edge-%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	if set.Scope.ValidateEdge(edge) != nil {
		return Edge{}, nil, ErrSuggestion
	}
	return edge, []SuggestionEvidence{*from.Evidence, *to.Evidence}, nil
}

func resolveSuggestionEndpoint(set SourceSet, bound BoundSuggestion, endpoint SuggestionEndpoint, resolutions []SuggestionCreationResolution) (SuggestionEndpoint, error) {
	if endpoint.Creation == nil {
		return endpoint, nil
	}
	var result *SuggestionEndpoint
	for _, resolution := range resolutions {
		if resolution.Creation != *endpoint.Creation {
			continue
		}
		if result != nil || resolution.Origin != bound.Origin || !textValue(resolution.CommandID, 128) || resolution.Ref.Kind != "work-item" || resolution.Ref.SourceBindingID != endpoint.Creation.SourceBindingID {
			return SuggestionEndpoint{}, ErrSuggestion
		}
		resolved := SuggestionEndpoint{Ref: &resolution.Ref, Evidence: &resolution.Evidence}
		if _, err := suggestionEndpointRef(set, resolved); err != nil {
			return SuggestionEndpoint{}, err
		}
		result = &resolved
	}
	if result == nil {
		return SuggestionEndpoint{}, ErrSuggestion
	}
	return *result, nil
}
