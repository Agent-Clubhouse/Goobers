package providers

import (
	"context"
	"strings"
)

// PatchNativeWorkItem uses one atomic ADO JSON Patch with a /rev test and one
// native field value. It never closes through multiple process transitions or
// rewrites description format, acceptance criteria or unrelated tags.
func (p *ADOProvider) PatchNativeWorkItem(ctx context.Context, req NativeWorkItemPatch) (NativeWorkItemPatchResult, error) {
	var result NativeWorkItemPatchResult
	if err := p.requireWorkItemScope(p.project(req.Repository)); err != nil {
		return result, err
	}
	if err := ValidateNativeWorkItemPatch(req, ProviderADO); err != nil {
		return result, err
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return result, err
	}
	if err = checkNativeEditIdentity(current, req); err != nil {
		return result, err
	}
	raw, err := rawADOWorkItem(current)
	if err != nil {
		return result, err
	}
	field, value, err := p.nativeField(ctx, req, current)
	if err != nil {
		return result, err
	}
	patch := []adoPatchOperation{{Op: "test", Path: "/rev", Value: raw.Rev}, {Op: "add", Path: "/fields/" + field, Value: value}}
	result.MutationAttempted = true
	result.Item, err = p.patchADOWorkItem(WithoutMutationRetries(ctx), req.Repository, req.ID, patch)
	if err != nil {
		return result, err
	}
	result.Acknowledged = true
	p.recordMutation(ctx, "issue", req.ID, "workbench-"+req.Field, req.Repository)
	return result, nil
}

func (p *ADOProvider) nativeField(ctx context.Context, req NativeWorkItemPatch, current WorkItem) (string, string, error) {
	switch req.Field {
	case "title":
		return "System.Title", *req.Value, nil
	case "description":
		return "System.Description", *req.Value, nil
	case "labels":
		return "System.Tags", strings.Join(req.Values, "; "), nil
	case "assignees":
		value := ""
		if len(req.Values) > 0 {
			value = req.Values[0]
		}
		return "System.AssignedTo", value, nil
	case "state":
		states, err := p.adoWorkItemStateCategories(ctx, req.Repository, current.Type)
		if err != nil {
			return "", "", err
		}
		state, ok := findADOWorkItemState(states, *req.Value)
		if !ok || state.Name != *req.Value {
			return "", "", ErrNativeEdit
		}
		return "System.State", state.Name, nil
	default:
		return "", "", ErrNativeEdit
	}
}
