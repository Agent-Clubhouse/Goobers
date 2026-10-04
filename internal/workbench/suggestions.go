package workbench

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

const (
	// SuggestionSchemaVersion identifies proposals, never accepted source edges.
	SuggestionSchemaVersion = "relationship-suggestions/v1"
	// MaxSuggestionBytes bounds one agent-authored artifact.
	MaxSuggestionBytes = 256 << 10
	// MaxSuggestions bounds the candidates in one artifact.
	MaxSuggestions = 100
)

// RelationshipSuggestions contains no producer authority. Run/stage attribution
// comes from the host's actual artifact journal, separately from these bytes.
type RelationshipSuggestions struct {
	SchemaVersion string                   `json:"schemaVersion" yaml:"schemaVersion"`
	Suggestions   []RelationshipSuggestion `json:"suggestions" yaml:"suggestions"`
}

// RelationshipSuggestion proposes one addition. Rejection cannot remove a source
// edge. The ordinary source owner and command path determine acceptance.
type RelationshipSuggestion struct {
	Kind      string             `json:"kind" yaml:"kind"`
	From      SuggestionEndpoint `json:"from" yaml:"from"`
	To        SuggestionEndpoint `json:"to" yaml:"to"`
	Rationale string             `json:"rationale" yaml:"rationale"`
}

// SuggestionEndpoint names either a source object with claimed read evidence, or
// an uncreated work item. Neither form grants visibility or proves existence.
type SuggestionEndpoint struct {
	Ref      *NodeRef            `json:"ref,omitempty" yaml:"ref,omitempty"`
	Evidence *SuggestionEvidence `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	Creation *SuggestionCreation `json:"creation,omitempty" yaml:"creation,omitempty"`
}

// SuggestionEvidence pins what the agent claims it read. Acceptance must compare
// it with an authorized fresh source observation; parser success is not proof.
type SuggestionEvidence struct {
	SourceTargetDigest string                        `json:"sourceTargetDigest" yaml:"sourceTargetDigest"`
	NativeRevision     string                        `json:"nativeRevision,omitempty" yaml:"nativeRevision,omitempty"`
	Path               string                        `json:"path,omitempty" yaml:"path,omitempty"`
	RepositoryRevision *SuggestionRepositoryRevision `json:"repositoryRevision,omitempty" yaml:"repositoryRevision,omitempty"`
}

// SuggestionRepositoryRevision has the same exact pins as MetadataRevision, with
// explicit YAML names for the closed artifact decoder.
type SuggestionRepositoryRevision struct {
	Commit        string `json:"commit" yaml:"commit"`
	BlobID        string `json:"blobId" yaml:"blobId"`
	ContentDigest string `json:"contentDigest" yaml:"contentDigest"`
}

// SuggestionCreation is a provisional request in the producing run/stage/attempt.
// Only a host-verified durable creation receipt can resolve it to a stable item.
type SuggestionCreation struct {
	SourceBindingID string `json:"sourceBindingId" yaml:"sourceBindingId"`
	RequestID       string `json:"requestId" yaml:"requestId"`
}

// ErrSuggestion refuses invalid, out-of-scope or stale proposal evidence.
var ErrSuggestion = errors.New("workbench: invalid or stale relationship suggestion")

// ParseSuggestions validates bounded, closed JSON. It performs no provider reads,
// source mutation or graph projection. Repeated suggestions are allowed here and
// deduplicated after binding the host origin in BindSuggestions.
func ParseSuggestions(raw []byte, set SourceSet) (RelationshipSuggestions, error) {
	if len(raw) == 0 || len(raw) > MaxSuggestionBytes || !utf8.Valid(raw) || !json.Valid(raw) || set.Scope.Validate() != nil {
		return RelationshipSuggestions{}, ErrSuggestion
	}
	node, err := parseSourceYAML(raw)
	if err != nil {
		return RelationshipSuggestions{}, ErrSuggestion
	}
	var artifact RelationshipSuggestions
	if err := decodeClosed(node, &artifact); err != nil {
		return artifact, ErrSuggestion
	}
	if artifact.SchemaVersion != SuggestionSchemaVersion || len(artifact.Suggestions) == 0 || len(artifact.Suggestions) > MaxSuggestions {
		return artifact, ErrSuggestion
	}
	for _, suggestion := range artifact.Suggestions {
		if err := validateSuggestion(set, suggestion); err != nil {
			return RelationshipSuggestions{}, err
		}
	}
	return artifact, nil
}

func validateSuggestion(set SourceSet, value RelationshipSuggestion) error {
	if !textValue(value.Rationale, 4096) {
		return ErrSuggestion
	}
	from, err := suggestionEndpointRef(set, value.From)
	if err != nil {
		return err
	}
	to, err := suggestionEndpointRef(set, value.To)
	if err != nil {
		return err
	}
	edge := Edge{EdgeID: "edge-00000000-0000-0000-0000-000000000000", Kind: value.Kind, From: from, To: to, Rationale: value.Rationale}
	if set.Scope.ValidateEdge(edge) != nil {
		return ErrSuggestion
	}
	return nil
}
