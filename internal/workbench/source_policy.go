package workbench

import (
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func validateObjectiveSelector(selector *apiv1.WorkbenchObjectiveSelector) error {
	if selector == nil {
		return nil
	}
	if len(selector.IDs)+len(selector.Types)+len(selector.Labels) == 0 {
		return errors.New("objective selector must name native IDs, types or labels")
	}
	if err := validateStrings(selector.IDs, 128, 512); err != nil {
		return err
	}
	if err := validateStrings(selector.Types, 32, 256); err != nil {
		return err
	}
	return validateStrings(selector.Labels, 32, 256)
}

func validateStrings(values []string, maxCount, maxBytes int) error {
	if len(values) > maxCount {
		return errors.New("workbench: configured selector exceeds count bound")
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !textValue(value, maxBytes) || seen[value] {
			return errors.New("workbench: configured values must be bounded, nonempty and unique")
		}
		seen[value] = true
	}
	return nil
}

func validateSourceWrites(kind string, writes *apiv1.WorkbenchWrites) error {
	if writes == nil {
		return nil
	}
	if len(writes.Fields) > 5 || len(writes.Relationships) > 6 {
		return errors.New("source write allowlist exceeds bound")
	}
	seen := map[string]bool{}
	for _, field := range writes.Fields {
		name := string(field)
		valid := name == "title" || name == "description" || name == "state" || name == "labels" || name == "assignees"
		if !valid || seen[name] || kind == "relationships" || (kind == "documents" && name != "title" && name != "description") {
			return errors.New("source write field is unknown, duplicated or unavailable for its source kind")
		}
		seen[name] = true
	}
	return validateRelationshipWrites(kind, writes.Relationships)
}

func validateRelationshipWrites(kind string, values []apiv1.WorkbenchRelationship) error {
	seen := map[apiv1.WorkbenchRelationship]bool{}
	for _, value := range values {
		switch value {
		case "parent-of", "blocked-by", "contributes-to", "references", "milestone-member", "implemented-by":
		default:
			return errors.New("unknown authored relationship permission")
		}
		if seen[value] || (kind == "documents" && value != "contributes-to" && value != "references") {
			return errors.New("relationship permission is duplicated or unavailable for its source kind")
		}
		seen[value] = true
	}
	return nil
}

// AllowsField reports only the source allowlist. It is not permission: the
// current human action, provider capability and source revision must also pass.
func (s BoundSource) AllowsField(field apiv1.WorkbenchField) bool {
	if s.Spec.Writes != nil {
		for _, allowed := range s.Spec.Writes.Fields {
			if allowed == field {
				return true
			}
		}
	}
	return false
}

// AllowsRelationship reports only the source relationship allowlist.
func (s BoundSource) AllowsRelationship(kind apiv1.WorkbenchRelationship) bool {
	if s.Spec.Writes != nil {
		for _, allowed := range s.Spec.Writes.Relationships {
			if allowed == kind {
				return true
			}
		}
	}
	return false
}
