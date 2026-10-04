package workbench

import (
	"slices"
	"strconv"
)

func suggestionEndpointRef(set SourceSet, endpoint SuggestionEndpoint) (NodeRef, error) {
	if endpoint.Creation != nil {
		if endpoint.Ref != nil || endpoint.Evidence != nil {
			return NodeRef{}, ErrSuggestion
		}
		creation := endpoint.Creation
		source, ok := suggestionSource(set, creation.SourceBindingID)
		if !ok || source.Spec.Kind != "backlog" || !textValue(creation.RequestID, 128) {
			return NodeRef{}, ErrSuggestion
		}
		// This temporary identity is used only for direction/self-edge validation.
		// It is never returned to a source adapter or authoritative graph projector.
		return NodeRef{GaggleID: set.Scope.GaggleID, SourceBindingID: creation.SourceBindingID, Kind: "work-item", SourceID: "provisional:" + creation.RequestID}, nil
	}
	if endpoint.Ref == nil || endpoint.Evidence == nil || set.Scope.ValidateRef(*endpoint.Ref) != nil {
		return NodeRef{}, ErrSuggestion
	}
	source, ok := suggestionSource(set, endpoint.Ref.SourceBindingID)
	if !ok || validateSuggestionEvidence(set.Scope, source, *endpoint.Ref, *endpoint.Evidence) != nil {
		return NodeRef{}, ErrSuggestion
	}
	return *endpoint.Ref, nil
}

func suggestionSource(set SourceSet, binding string) (BoundSource, bool) {
	for _, source := range set.Sources {
		if source.Spec.Name == binding {
			return source, true
		}
	}
	return BoundSource{}, false
}

func validateSuggestionEvidence(scope Scope, source BoundSource, ref NodeRef, value SuggestionEvidence) error {
	digest, err := SourceTargetDigest(scope, source)
	if err != nil || digest != value.SourceTargetDigest {
		return ErrSuggestion
	}
	switch source.Spec.Kind {
	case "backlog":
		if ref.Kind != "work-item" && ref.Kind != "milestone" && ref.Kind != "pull-request" {
			return ErrSuggestion
		}
		if !textValue(value.NativeRevision, 512) || value.Path != "" || value.RepositoryRevision != nil || (value.NativeLocator != "" && !validSuggestionLocator(value.NativeLocator)) {
			return ErrSuggestion
		}
	case "documents":
		if ref.Kind != "document" && ref.Kind != "objective-document" {
			return ErrSuggestion
		}
		if value.NativeRevision != "" || value.NativeLocator != "" || value.RepositoryRevision == nil || !slices.Contains(source.Spec.Paths, value.Path) {
			return ErrSuggestion
		}
		pins := value.RepositoryRevision
		if !validMetadataRevision(MetadataRevision{Commit: pins.Commit, BlobID: pins.BlobID, ContentDigest: pins.ContentDigest}) {
			return ErrSuggestion
		}
	default:
		return ErrSuggestion
	}
	return nil
}

func validSuggestionLocator(value string) bool {
	if len(value) > 19 {
		return false
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == value
}
