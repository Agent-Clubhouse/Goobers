// Package sessionops exposes bounded host operations to a single live session
// invocation. It carries no provider implementation or provider credential.
package sessionops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

// BacklogReader is installed by the host for an exact live ExecutionLease.
// Implementations authorize source bindings and resolve provider credentials
// inside that lease; they must never reacquire the global policy read lock.
// Calls are synchronous and must honor cancellation. Source text is untrusted.
type BacklogReader interface {
	Get(context.Context, string, workbench.BacklogItemRequest) (workbench.BacklogItem, error)
	Page(context.Context, string, workbench.BacklogPageRequest) (workbench.BacklogPage, error)
}

// Recorder retains command evidence in the real session journal.
type Recorder interface {
	Append(journal.Event) error
	RecordArtifactWithIntegrity(string, []byte, apiv1.Integrity) (journal.Ref, error)
}

// Invocation is trusted host binding, never accepted from a tool request.
// StageSequence names the actual committed stage.started event.
type Invocation struct {
	Identity        journal.RunIdentity
	Actor           sessioning.Actor
	StageSequence   uint64
	Attempt         int
	Lease           *interactiveaccess.ExecutionLease
	Reader          BacklogReader
	Writer          BacklogWriter
	Resolver        BacklogResolver
	ResolveBindings []string
	WriteBindings   []string
	SourceBindings  []string
	Recorder        Recorder
}

// Bridge keeps only live native invocation grants. A restart destroys them;
// resuming an uncertain process cannot recreate authority from the old token.
type Bridge struct {
	Endpoint string
	Scrubber journal.Scrubber
	Secrets  interface{ RegisterUntil([]byte, time.Time) }
	Now      func() time.Time
	mu       sync.Mutex
	grants   map[string]*grant
}
type grant struct {
	mu           sync.Mutex
	invocation   Invocation
	expires      time.Time
	revoked      bool
	calls, bytes int
}

const maxGrants = 256
const grantLifetime = time.Hour

// ErrDenied reports an absent, revoked or mismatched invocation grant.
var ErrDenied = errors.New("session operation access denied")

// ErrLimit reports the bounded per-turn operation allowance.
var ErrLimit = errors.New("session operation limit reached")

// Open binds a fresh opaque credential to verified live host custody. close
// first revokes it and then joins any bounded operation before returning.
func (b *Bridge) Open(inv Invocation) (*mcpio.SessionOperationAccess, func(), error) {
	if b == nil || b.Scrubber == nil || b.Secrets == nil || b.Now == nil || inv.Lease == nil || (inv.Reader == nil && inv.Writer == nil && inv.Resolver == nil) || inv.Recorder == nil || inv.Identity.ValidateSessionLineage() != nil || inv.Identity.Session == nil || inv.Actor.Issuer == "" || inv.Actor.Subject == "" || inv.StageSequence == 0 || inv.Attempt < 1 || inv.Lease.Context().Err() != nil {
		return nil, nil, ErrDenied
	}
	if err := inv.Lease.RequireSessionScope(inv.Identity.Gaggle, inv.Actor.Issuer, inv.Actor.Subject); err != nil {
		return nil, nil, ErrDenied
	}
	lineage := *inv.Identity.Session
	inv.Identity.Session = &lineage
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return nil, nil, err
	}
	token := sessioning.OperationTokenPrefix + hex.EncodeToString(entropy)
	access := &mcpio.SessionOperationAccess{Endpoint: b.Endpoint, BearerToken: token, BacklogSources: append([]string(nil), inv.SourceBindings...), BacklogWriteSources: append([]string(nil), inv.WriteBindings...), BacklogResolveSources: append([]string(nil), inv.ResolveBindings...), BacklogReadDisabled: inv.Reader == nil}
	if err := access.Validate(inv.Identity.RunID); err != nil {
		return nil, nil, err
	}
	g := &grant{invocation: inv, expires: b.Now().Add(grantLifetime)}
	b.mu.Lock()
	if b.grants == nil {
		b.grants = map[string]*grant{}
	}
	if len(b.grants) >= maxGrants {
		b.mu.Unlock()
		return nil, nil, ErrLimit
	}
	b.grants[token] = g
	b.mu.Unlock()
	b.Secrets.RegisterUntil([]byte(token), g.expires)
	close := func() {
		b.mu.Lock()
		delete(b.grants, token)
		b.mu.Unlock()
		g.mu.Lock()
		g.revoked = true
		g.mu.Unlock()
	}
	return access, close, nil
}
func (b *Bridge) lookup(token string) (*grant, error) {
	if b == nil || b.Now == nil {
		return nil, ErrDenied
	}
	b.mu.Lock()
	g := b.grants[token]
	b.mu.Unlock()
	if g == nil {
		return nil, ErrDenied
	}
	return g, nil
}
func (b *Bridge) active(g *grant, run string) error {
	if g.revoked || !b.Now().Before(g.expires) || g.invocation.Identity.RunID != run || g.invocation.Lease.Context().Err() != nil {
		return ErrDenied
	}
	return nil
}

// AuthenticateSessionOperation returns only the actual run binding, never human roles. Every
// operation repeats liveness checks while holding its exclusive invocation lock.
func (b *Bridge) AuthenticateSessionOperation(token string) (string, error) {
	g, err := b.lookup(token)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	run := g.invocation.Identity.RunID
	if err = b.active(g, run); err != nil {
		return "", err
	}
	return run, nil
}
