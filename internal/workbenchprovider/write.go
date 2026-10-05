package workbenchprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

// NativeWriter is supplied by a separately authorized host write callback.
// It exposes no credential selector, autonomous status updater or arbitrary URL.
type NativeWriter interface {
	BacklogClient
	providers.NativeWorkItemEditor
}

// ErrUnsupportedEdit is a configured-source or actual-operation refusal.
var ErrUnsupportedEdit = errors.New("workbenchprovider: native edit is not supported or allowlisted")

// ErrUnverifiedEdit means an attempted mutation has no verified postcondition.
// Its receipt may include a current observation, never an invitation to replay.
var ErrUnverifiedEdit = errors.New("workbenchprovider: native edit outcome requires observation")

// BacklogWriter combines a bounded reader with a separately supplied native
// editor. The caller must hold current human authority across the whole call.
type BacklogWriter struct {
	reader               *BacklogReader
	client               NativeWriter
	fields               []apiv1.WorkbenchField
	mutationTargetDigest string
}

// NewBacklogWriter copies the source write allowlist. No policy or credential
// fallback occurs; a read-only source produces no enabled write capabilities.
func NewBacklogWriter(scope workbench.Scope, source workbench.BoundSource, client NativeWriter) (*BacklogWriter, error) {
	reader, err := NewBacklogReader(scope, source, client)
	if err != nil {
		return nil, err
	}
	target, err := workbench.BacklogMutationTargetDigest(scope, source)
	if err != nil {
		return nil, err
	}
	writer := &BacklogWriter{reader: reader, client: client, mutationTargetDigest: target}
	if source.Spec.Writes != nil {
		writer.fields = append([]apiv1.WorkbenchField(nil), source.Spec.Writes.Fields...)
	}
	return writer, nil
}

// Capabilities reports the configured intersection with this concrete adapter.
// Native relationship changes have no enabled capability in this field slice.
func (w *BacklogWriter) Capabilities() workbench.BacklogWriteCapabilities {
	return backlogCapabilities(w.reader.repository.Provider, w.fields)
}

// BacklogCapabilities reports configured native edit support without resolving
// a credential or contacting a provider. It grants no permission to mutate.
func BacklogCapabilities(source workbench.BoundSource) workbench.BacklogWriteCapabilities {
	var fields []apiv1.WorkbenchField
	if source.Spec.Writes != nil {
		fields = source.Spec.Writes.Fields
	}
	return backlogCapabilities(providers.ProviderKind(source.BacklogIdentity.Provider), fields)
}
func backlogCapabilities(provider providers.ProviderKind, fields []apiv1.WorkbenchField) workbench.BacklogWriteCapabilities {
	result := workbench.BacklogWriteCapabilities{Fields: []apiv1.WorkbenchField{}, Relationships: []apiv1.WorkbenchRelationship{}, RevisionSemantics: "timestamp-preflight", MaxAssignees: 10}
	if provider == providers.ProviderADO {
		result.RevisionSemantics = "atomic-revision-test"
		result.MaxAssignees = 1
	} else if provider != providers.ProviderGitHub {
		return result
	}
	for _, field := range []apiv1.WorkbenchField{"title", "description", "state", "labels", "assignees"} {
		if slices.Contains(fields, field) {
			result.Fields = append(result.Fields, field)
		}
	}
	return result
}

// BacklogOperationDigest validates exact command identity without minting auth
// or doing provider revision preflight. The host may compare durable replay
// receipts after current policy checks; only Patch performs remote effects.
func BacklogOperationDigest(scope workbench.Scope, source workbench.BoundSource, request workbench.BacklogPatchRequest) (string, error) {
	if scope.Validate() != nil || !scope.Bindings[source.Spec.Name] || source.Spec.Kind != "backlog" || source.Backlog.BaseURL != "" || !validBacklogTarget(source, source.BacklogIdentity) {
		return "", ErrInvalidSource
	}
	target, err := workbench.BacklogMutationTargetDigest(scope, source)
	if err != nil {
		return "", err
	}
	id := source.BacklogIdentity
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(id.Provider), Owner: id.Owner, Project: id.Project, Name: id.Name}
	native := nativePatchRequest(repo, request)
	return mutationOperationDigest(target, repo.Provider, BacklogCapabilities(source), native, request)
}

// OperationDigest computes the exact bounded source/target/request digest before
// any provider call, for the host's durable command acceptance and replay guard.
// This digest is not a provider idempotency key and is never sent remotely.
func (w *BacklogWriter) OperationDigest(request workbench.BacklogPatchRequest) (string, error) {
	return mutationOperationDigest(w.mutationTargetDigest, w.reader.repository.Provider, w.Capabilities(), w.nativeRequest(request), request)
}
func mutationOperationDigest(target string, provider providers.ProviderKind, capabilities workbench.BacklogWriteCapabilities, native providers.NativeWorkItemPatch, request workbench.BacklogPatchRequest) (string, error) {
	if !slices.Contains(capabilities.Fields, request.Field) {
		return "", ErrUnsupportedEdit
	}
	if err := providers.ValidateNativeWorkItemPatch(native, provider); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(struct {
		Version int
		Target  string
		Request workbench.BacklogPatchRequest
	}{2, target, request})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// Patch performs at most one native mutation attempt and one post-attempt read.
// ExpectedRevision is always required. An uncertain outcome is returned with its
// receipt even when an observation matches; the caller must not blindly retry.
func (w *BacklogWriter) Patch(ctx context.Context, request workbench.BacklogPatchRequest) (workbench.BacklogPatchReceipt, error) {
	request = copyPatchRequest(request)
	receipt := workbench.BacklogPatchReceipt{Outcome: "not-applied", RevisionSemantics: w.Capabilities().RevisionSemantics}
	digest, err := w.OperationDigest(request)
	if err != nil {
		return receipt, err
	}
	receipt.OperationDigest = digest
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	before, err := w.reader.Get(ctx, workbench.BacklogItemRequest{ID: request.ID, ExpectedSourceID: request.SourceID})
	if err != nil {
		return receipt, err
	}
	receipt.Observed = &before
	if before.Revision != request.ExpectedRevision {
		return receipt, &providers.RevisionConflictError{ItemID: request.ID, Expected: request.ExpectedRevision, Actual: before.Revision}
	}
	result, editErr := w.client.PatchNativeWorkItem(ctx, w.nativeRequest(request))
	if !result.MutationAttempted && !result.Acknowledged {
		if editErr == nil {
			editErr = ErrUnverifiedEdit
		}
		return receipt, editErr
	}
	receipt.Outcome = "unknown"
	receipt.Observed = nil
	receipt.ProviderAcknowledged = result.Acknowledged
	observed, readErr := w.reader.Get(ctx, workbench.BacklogItemRequest{ID: request.ID, ExpectedSourceID: request.SourceID})
	if readErr == nil {
		receipt.Observed = &observed
		receipt.ObservedMatches = patchMatches(request, observed)
	}
	if editErr == nil && readErr == nil && result.Acknowledged && receipt.ObservedMatches {
		receipt.Outcome = "confirmed"
		return receipt, nil
	}
	return receipt, errors.Join(ErrUnverifiedEdit, editErr, readErr)
}

func (w *BacklogWriter) nativeRequest(request workbench.BacklogPatchRequest) providers.NativeWorkItemPatch {
	return nativePatchRequest(w.reader.repository, request)
}
func nativePatchRequest(repository providers.RepositoryRef, request workbench.BacklogPatchRequest) providers.NativeWorkItemPatch {
	return providers.NativeWorkItemPatch{Repository: repository, ID: request.ID, StableID: request.SourceID, ExpectedRevision: request.ExpectedRevision, Field: string(request.Field), Value: request.Value, Values: append([]string(nil), request.Values...)}
}

func patchMatches(request workbench.BacklogPatchRequest, item workbench.BacklogItem) bool {
	switch request.Field {
	case "title":
		return item.Title == *request.Value
	case "description":
		return item.Description == *request.Value
	case "state":
		return item.State == *request.Value
	case "labels":
		return equalNativeSet(item.Labels, request.Values)
	case "assignees":
		return equalNativeSet(item.Assignees, request.Values)
	default:
		return false
	}
}

func equalNativeSet(left, right []string) bool {
	normalize := func(values []string) []string {
		result := make([]string, 0, len(values))
		for _, value := range values {
			result = append(result, strings.ToLower(value))
		}
		slices.Sort(result)
		return result
	}
	return slices.Equal(normalize(left), normalize(right))
}

func copyPatchRequest(request workbench.BacklogPatchRequest) workbench.BacklogPatchRequest {
	if request.Value != nil {
		value := *request.Value
		request.Value = &value
	}
	request.Values = append([]string(nil), request.Values...)
	return request
}
