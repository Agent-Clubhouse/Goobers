package workbenchservice

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// WriterService combines current human/source policy with durable command custody.
// ReadService supplies exact-target provider construction; no automation identity
// or arbitrary provider endpoint can be supplied by a patch request.
type WriterService struct {
	ReadService *Service
	Queue       *triggerqueue.Store
	Now         func() time.Time
}

// Capabilities returns the actual configured native intersection only after
// checking current operator and credential-binding authority. No token is minted.
func (s *WriterService) Capabilities(ctx context.Context, p httpapi.Principal, gaggle, binding string) (workbench.BacklogWriteCapabilities, error) {
	var result workbench.BacklogWriteCapabilities
	err := s.withWrite(ctx, p, gaggle, binding, func(_ context.Context, bound ReadBinding, _ interactiveaccess.SourceCredentialLoader) error {
		result = workbenchprovider.BacklogCapabilities(bound.Source)
		return nil
	})
	return result, writeError(err)
}

// Patch commits acceptance and claims one attempt before resolving a provider
// credential or invoking its mutation. Exact retries return retained evidence;
// they never recheck the old provider revision or repeat an uncertain effect.
func (s *WriterService) Patch(ctx context.Context, p httpapi.Principal, gaggle, binding, key string, request workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
	request = copyWriteRequest(request)
	var view workbench.BacklogEditCommand
	err := s.withWrite(ctx, p, gaggle, binding, func(ctx context.Context, bound ReadBinding, load interactiveaccess.SourceCredentialLoader) error {
		var err error
		view, err = s.patch(ctx, writeScope(p, gaggle, binding), bound, load, key, request)
		return err
	})
	return view, writeError(err)
}

// Command reads actor-scoped evidence after current target and field checks.
// A compact tombstone has no remaining field/body and returns only generic 410.
func (s *WriterService) Command(ctx context.Context, p httpapi.Principal, gaggle, binding, id string) (workbench.BacklogEditCommand, error) {
	var view workbench.BacklogEditCommand
	err := s.withWrite(ctx, p, gaggle, binding, func(ctx context.Context, bound ReadBinding, _ interactiveaccess.SourceCredentialLoader) error {
		var err error
		view, err = s.command(ctx, writeScope(p, gaggle, binding), bound, id)
		return err
	})
	return view, writeError(err)
}

// patch and command receive authority only from a held human callback or a
// live session lease. They never acquire the policy lock themselves.
func (s *WriterService) patch(ctx context.Context, scope triggerqueue.WorkbenchCommandScope, bound ReadBinding, load interactiveaccess.SourceCredentialLoader, key string, request workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
	operation, err := workbenchprovider.BacklogOperationDigest(bound.Scope, bound.Source, request)
	if err != nil {
		return workbench.BacklogEditCommand{}, err
	}
	target, err := workbench.BacklogMutationTargetDigest(bound.Scope, bound.Source)
	if err != nil {
		return workbench.BacklogEditCommand{}, err
	}
	input := triggerqueue.WorkbenchCommandInput{Scope: scope, RequestID: key, TargetDigest: target, OperationDigest: operation, Request: request}
	record, duplicate, err := s.Queue.AcceptWorkbenchCommand(ctx, input, s.now())
	if err != nil {
		return workbench.BacklogEditCommand{}, err
	}
	record, claimed, err := s.Queue.ClaimWorkbenchCommand(ctx, input.Scope, record.ID, record.RequestDigest, s.now())
	if err != nil {
		return workbench.BacklogEditCommand{}, err
	}
	if claimed {
		record, err = s.execute(ctx, bound, load, record)
	}
	return commandView(record, duplicate), err
}
func (s *WriterService) command(ctx context.Context, scope triggerqueue.WorkbenchCommandScope, bound ReadBinding, id string) (workbench.BacklogEditCommand, error) {
	record, err := s.Queue.WorkbenchCommand(ctx, scope, id)
	if err != nil && !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
		return workbench.BacklogEditCommand{}, err
	}
	target, targetErr := workbench.BacklogMutationTargetDigest(bound.Scope, bound.Source)
	if targetErr != nil || target != record.Input.TargetDigest {
		return workbench.BacklogEditCommand{}, interactiveaccess.ErrDenied
	}
	if err != nil {
		return workbench.BacklogEditCommand{}, err
	}
	if !slices.Contains(workbenchprovider.BacklogCapabilities(bound.Source).Fields, record.Input.Request.Field) {
		return workbench.BacklogEditCommand{}, interactiveaccess.ErrDenied
	}
	return commandView(record, false), nil
}
func (s *WriterService) withWrite(ctx context.Context, p httpapi.Principal, gaggle, binding string, use func(context.Context, ReadBinding, interactiveaccess.SourceCredentialLoader) error) error {
	if s == nil || s.ReadService == nil || s.ReadService.Permissions == nil || s.ReadService.Backlog == nil || s.Queue == nil {
		return readError(http.StatusServiceUnavailable, "workbench_write_unavailable", "Backlog editing is unavailable.")
	}
	// Reserve three seconds of the eight-second mutation budget for detached
	// receipt persistence after the provider phase stops.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.ReadService.Permissions.WithSourceWrite(ctx, p, gaggle, func(ctx context.Context, g *apiv1.Gaggle, load interactiveaccess.SourceCredentialLoader) error {
		bound, err := selectBacklog(g, binding)
		if err != nil {
			return err
		}
		return use(ctx, bound, load)
	})
}
func (s *WriterService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func writeScope(p httpapi.Principal, gaggle, binding string) triggerqueue.WorkbenchCommandScope {
	return triggerqueue.WorkbenchCommandScope{Gaggle: gaggle, SourceBindingID: binding, Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}}
}
func copyWriteRequest(request workbench.BacklogPatchRequest) workbench.BacklogPatchRequest {
	if request.Value != nil {
		value := *request.Value
		request.Value = &value
	}
	request.Values = append([]string(nil), request.Values...)
	return request
}
