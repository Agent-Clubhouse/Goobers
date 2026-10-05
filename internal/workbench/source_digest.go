package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SourceTargetDigest binds a projection/cursor to exact configured target and
// classification semantics. It carries no read permission, secret, or mutable
// source revision. Readers must still validate the bound target and authorize it.
func SourceTargetDigest(scope Scope, source BoundSource) (string, error) {
	if scope.Validate() != nil || !scope.Bindings[source.Spec.Name] {
		return "", errors.New("workbench: invalid source digest scope")
	}
	var value any
	switch source.Spec.Kind {
	case "backlog":
		objectives := apiv1.WorkbenchObjectiveSelector{}
		if source.Spec.Objectives != nil {
			objectives = *source.Spec.Objectives
		}
		// Keep the original cursor representation: empty selector slices normalize
		// to nil because provider readers historically copied into nil slices.
		objectives.IDs = append([]string(nil), objectives.IDs...)
		objectives.Types = append([]string(nil), objectives.Types...)
		objectives.Labels = append([]string(nil), objectives.Labels...)
		value = struct {
			Gaggle, Binding string
			Target          apiv1.InteractiveRepositoryIdentity
			Objectives      apiv1.WorkbenchObjectiveSelector
		}{scope.GaggleID, source.Spec.Name, source.BacklogIdentity, objectives}
	case "documents", "relationships":
		if source.Spec.Repository == nil {
			return "", errors.New("workbench: repository source has no target")
		}
		value = struct {
			Gaggle, Binding, Kind, Branch string
			Target                        apiv1.InteractiveRepositoryIdentity
			Paths                         []string
		}{scope.GaggleID, source.Spec.Name, source.Spec.Kind, source.Repository.Branch, *source.Spec.Repository, source.Spec.Paths}
	default:
		return "", errors.New("workbench: unknown source kind")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// BacklogMutationTargetDigest binds an edit to its physical configured source.
// Objective classification and unrelated configuration changes are not mutation
// identity. Current action and field policy must still be checked on every call.
func BacklogMutationTargetDigest(scope Scope, source BoundSource) (string, error) {
	if scope.Validate() != nil || !scope.Bindings[source.Spec.Name] || source.Spec.Kind != "backlog" {
		return "", errors.New("workbench: invalid native mutation target")
	}
	raw, err := json.Marshal(struct {
		Gaggle, Binding string
		Target          apiv1.InteractiveRepositoryIdentity
	}{scope.GaggleID, source.Spec.Name, source.BacklogIdentity})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
