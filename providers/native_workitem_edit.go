package providers

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NativeWorkItemEditor provides one faithful native field PATCH. Implementations
// send at most one mutation request and keep lost outcomes explicitly uncertain.
type NativeWorkItemEditor interface {
	PatchNativeWorkItem(context.Context, NativeWorkItemPatch) (NativeWorkItemPatchResult, error)
}

// NativeWorkItemPatch is a trusted host command, not an authority-bearing body.
// No comments, status mirroring, remote idempotency markers or implicit state
// transitions are added. ADO description format and acceptance criteria survive.
type NativeWorkItemPatch struct {
	Repository                            RepositoryRef
	ID, StableID, ExpectedRevision, Field string
	Value                                 *string
	Values                                []string
}

// NativeWorkItemPatchResult distinguishes rejection before a mutation attempt
// from a request that may have committed. Acknowledged requires a successful
// provider response; readers must still verify the exact desired field.
type NativeWorkItemPatchResult struct {
	MutationAttempted bool
	Acknowledged      bool
	Item              WorkItem
}

// ErrNativeEdit rejects unsupported or unsafe native edits before mutation.
var ErrNativeEdit = errors.New("provider: invalid native work item edit")

// ValidateNativeWorkItemPatch enforces the shared payload envelope. Provider
// methods repeat it so calling them directly cannot bypass request bounds.
func ValidateNativeWorkItemPatch(req NativeWorkItemPatch, kind ProviderKind) error {
	if !nativePositiveID(req.ID) || !nativePositiveID(req.StableID) || !nativeEditText(req.ExpectedRevision, 256, false) {
		return ErrNativeEdit
	}
	switch req.Field {
	case "title", "description", "state":
		return validateNativeScalar(req, kind)
	case "labels", "assignees":
		return validateNativeSet(req, kind)
	default:
		return ErrNativeEdit
	}
}

func validateNativeScalar(req NativeWorkItemPatch, kind ProviderKind) error {
	if req.Value == nil || len(req.Values) > 0 {
		return ErrNativeEdit
	}
	limit := 192 << 10
	if req.Field == "title" {
		limit = 4096
	}
	if req.Field == "state" {
		limit = 128
	}
	if !nativeEditText(*req.Value, limit, req.Field == "description") {
		return ErrNativeEdit
	}
	if req.Field == "state" && kind == ProviderGitHub && *req.Value != "open" && *req.Value != "closed" {
		return ErrNativeEdit
	}
	return nil
}

func validateNativeSet(req NativeWorkItemPatch, kind ProviderKind) error {
	if req.Value != nil {
		return ErrNativeEdit
	}
	limit := 128
	if req.Field == "assignees" {
		limit = 10
		if kind == ProviderADO {
			limit = 1
		}
	}
	if len(req.Values) > limit {
		return ErrNativeEdit
	}
	seen := map[string]bool{}
	for _, value := range req.Values {
		if !nativeEditText(value, 400, false) || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return ErrNativeEdit
		}
		key := strings.ToLower(value)
		if seen[key] || (kind == ProviderADO && req.Field == "labels" && strings.Contains(value, ";")) {
			return ErrNativeEdit
		}
		seen[key] = true
	}
	return nil
}

func nativePositiveID(value string) bool {
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == value
}
func nativeEditText(value string, limit int, empty bool) bool {
	return (empty || strings.TrimSpace(value) != "") && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func checkNativeEditIdentity(item WorkItem, req NativeWorkItemPatch) error {
	if item.ID != req.ID || item.StableID != req.StableID {
		return ErrNativeEdit
	}
	if err := checkWorkItemRevision(item, req.ExpectedRevision); err != nil {
		return err
	}
	if req.Field == "labels" {
		labels := item.Labels
		if item.Provider == ProviderADO {
			if tags, ok := item.Fields["System.Tags"].(string); ok {
				labels = strings.Split(tags, ";")
			}
		}
		if !slices.Equal(nativeControlLabels(labels), nativeControlLabels(req.Values)) {
			return ErrNativeEdit
		}
	}
	return nil
}

func nativeControlLabels(values []string) []string {
	var result []string
	for _, value := range values {
		label := strings.ToLower(strings.TrimSpace(value))
		if strings.HasPrefix(label, "goobers:") || strings.HasPrefix(label, "goobers/status:") {
			result = append(result, label)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}
