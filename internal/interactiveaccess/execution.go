package interactiveaccess

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
)

// ErrExecutionNotJoined refuses policy publication while canceled execution may
// still hold credentials. Cancellation alone is not a termination receipt.
var ErrExecutionNotJoined = errors.New("interactive execution did not join before policy publication")

// ExecutionLease binds one trusted local execution to the applied gaggle policy.
// Close must run only after its work and all subprocesses have returned. Unlike
// bounded provider callbacks, this lease permits credentials to survive until
// execution returns; policy changes cancel and join it before publication.
type ExecutionLease struct {
	service   *Service
	gaggle    *apiv1.Gaggle
	principal httpapi.Principal
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	once      sync.Once
	failure   error
}

// BeginExecution rechecks a durable human identity before asynchronous work.
// It must be called outside WithAuthorization/WithRestartSources callbacks.
func (s *Service) BeginExecution(ctx context.Context, p httpapi.Principal, gaggle string) (*ExecutionLease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g := s.gaggles[gaggle]
	if err := authorize(p, g, "run.restartStage"); err != nil {
		return nil, err
	}
	p.Roles = append([]httpapi.Role(nil), p.Roles...)
	p.Groups = append([]string(nil), p.Groups...)
	child, cancel := context.WithCancel(ctx)
	lease := &ExecutionLease{service: s, gaggle: g.DeepCopy(), principal: p, ctx: child, cancel: cancel, done: make(chan struct{})}
	s.executionMu.Lock()
	if s.executions == nil {
		s.executions = make(map[*ExecutionLease]struct{})
	}
	s.executions[lease] = struct{}{}
	s.executionMu.Unlock()
	return lease, nil
}

// Context carries revocation even when ordinary runner drain contexts are detached.
func (l *ExecutionLease) Context() context.Context { return l.ctx }

// Close proves the caller's execution has joined and releases its registration.
func (l *ExecutionLease) Close() { _ = l.CloseAfter(nil) }

// CloseAfter releases only acknowledged execution. An uncertain writer keeps
// its lease revoked and unjoined so a later reload cannot publish past it.
func (l *ExecutionLease) CloseAfter(proof error) error {
	l.cancel()
	l.service.executionMu.Lock()
	defer l.service.executionMu.Unlock()
	l.failure = errors.Join(l.failure, proof)
	if l.failure != nil {
		return errors.Join(ErrExecutionNotJoined, l.failure)
	}
	l.once.Do(func() { delete(l.service.executions, l); close(l.done) })
	return nil
}

// Credential resolves an explicitly selected human credential under this live
// lease. It never consults an automation resolver or reacquires the policy lock.
func (l *ExecutionLease) Credential(ctx context.Context, action apiv1.InteractiveAction, target Target) (Credential, error) {
	if err := l.ctx.Err(); err != nil {
		return Credential{}, err
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if !actionTarget(action, target) {
		return Credential{}, ErrDenied
	}
	if err := authorize(l.principal, l.gaggle, action); err != nil {
		return Credential{}, err
	}
	source, err := selectSource(l.gaggle, l.service.sources, target)
	if err != nil {
		return Credential{}, err
	}
	resolver, scheme, err := l.service.resolver(source)
	if err != nil {
		return Credential{}, ErrCredentialUnavailable
	}
	scoped, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	defer func() { stop(); cancel() }()
	var value string
	var expiresAt time.Time
	if expiring, ok := resolver.(credentials.ExpiringResolver); ok {
		value, expiresAt, err = expiring.ResolveWithExpiry(scoped, source.Name)
	} else {
		value, err = resolver.Resolve(scoped, source.Name)
	}
	if err != nil {
		return Credential{}, ErrCredentialUnavailable
	}
	l.service.deps.Registrar.Register([]byte(value))
	if err := l.ctx.Err(); err != nil {
		return Credential{}, err
	}
	return Credential{Value: value, Scheme: scheme, ExpiresAt: expiresAt}, nil
}

// cancelChangedExecutions runs with the policy write lock held. Lease cleanup
// and credential resolution never acquire that lock, so a draining execution
// cannot deadlock against publication. Failed publication still leaves already
// canceled executions revoked; a later resume requires fresh authorization.
func (s *Service) cancelChangedExecutions(next map[string]*apiv1.Gaggle) error {
	s.executionMu.Lock()
	var pending []*ExecutionLease
	for lease := range s.executions {
		if !reflect.DeepEqual(lease.gaggle, next[lease.gaggle.Name]) {
			lease.cancel()
			pending = append(pending, lease)
		}
	}
	s.executionMu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	timeout := s.executionDrainTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, lease := range pending {
		select {
		case <-lease.done:
		case <-deadline.C:
			return ErrExecutionNotJoined
		}
	}
	return nil
}

// RequireSources prevents a retained workflow from receiving credentials for a
// newly configured repository or backlog after a topology change.
func (l *ExecutionLease) RequireSources(project apiv1.RepoRef, backlog apiv1.BacklogRef, additional []apiv1.RepoRef) error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if !reflect.DeepEqual(l.gaggle.Spec.Project, project) || !reflect.DeepEqual(l.gaggle.Spec.Backlog, backlog) || !reflect.DeepEqual(l.gaggle.Spec.AdditionalRepos, additional) {
		return ErrDenied
	}
	return nil
}
