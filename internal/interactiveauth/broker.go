package interactiveauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strconv"
	"time"
)

func newID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("system entropy unavailable")
	}
	return prefix + hex.EncodeToString(raw[:])
}

func authorityKey(ref AuthorityRef) string { return ref.ID + "@" + strconv.FormatUint(ref.Version, 10) }

// ConnectDemo is idempotent for a local owner/provider. A revoked connection is
// returned as revoked, never replaced with a fresh grant as a side effect of startup.
func (b *Broker) ConnectDemo(ctx context.Context, p Principal, provider Provider) (Connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return Connection{}, err
	}
	if err := contextOK(ctx); err != nil {
		return Connection{}, err
	}
	for _, c := range b.data.Connections {
		if c.Owner == p.identity && c.Provider == provider {
			return clone(c), nil
		}
	}
	var resource Resource
	var label string
	switch provider {
	case ADO:
		resource = DemoADOResource()
		label = "Controlled local ADO — no Microsoft connection"
	case AppInsights:
		resource = DemoInsightsResource()
		label = "Controlled local App Insights — no Azure connection"
	default:
		return Connection{}, fault("unsupported_provider", "only controlled-local ADO and App Insights are supported")
	}
	c := Connection{Ref: ConnectionRef{newID("conn_"), 1}, Owner: p.identity, Provider: provider, Resources: []Resource{resource}, Mode: ControlledLocal, Label: label, State: "ready", ExpiresAt: b.now().Add(5 * time.Minute), Epoch: 1}
	b.data.Connections[c.Ref.ID] = c
	b.availability[c.Ref.ID] = availability{refreshable: true}
	if err := b.persistLocked(); err != nil {
		return Connection{}, err
	}
	return clone(c), nil
}

func (b *Broker) connectionLocked(p Principal, ref ConnectionRef) (Connection, error) {
	c, ok := b.data.Connections[ref.ID]
	if !ok || c.Owner != p.identity {
		return Connection{}, fault("not_found", "connection unavailable")
	}
	if c.Ref != ref {
		return Connection{}, fault("stale_binding", "connection binding has changed")
	}
	return c, nil
}

// ListConnections returns connection metadata visible to the principal.
func (b *Broker) ListConnections(p Principal) ([]Connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return nil, err
	}
	result := []Connection{}
	for _, c := range b.data.Connections {
		if c.Owner == p.identity {
			result = append(result, clone(c))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Ref.ID < result[j].Ref.ID })
	return result, nil
}

// SetDemoAuthState changes a controlled fixture connection’s authorization state.
func (b *Broker) SetDemoAuthState(ctx context.Context, p Principal, ref ConnectionRef, state string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return err
	}
	if err := contextOK(ctx); err != nil {
		return err
	}
	c, err := b.connectionLocked(p, ref)
	if err != nil {
		return err
	}
	if c.State == "revoked" {
		return fault("connection_revoked", "revocation cannot be undone by simulated expiration")
	}
	switch state {
	case "expired":
		c.ExpiresAt = b.now().Add(-time.Second)
		b.availability[c.Ref.ID] = availability{refreshable: true}
	case "interaction_required", "consent_required":
		b.availability[c.Ref.ID] = availability{}
	default:
		return fault("invalid", "simulation state must be expired, interaction_required, or consent_required")
	}
	c.State = state
	b.data.Connections[c.Ref.ID] = c
	return b.persistLocked()
}

// Reauthenticate refreshes a controlled connection without reviving revoked grants.
func (b *Broker) Reauthenticate(ctx context.Context, p Principal, ref ConnectionRef) (Connection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return Connection{}, err
	}
	if err := contextOK(ctx); err != nil {
		return Connection{}, err
	}
	c, err := b.connectionLocked(p, ref)
	if err != nil {
		return Connection{}, err
	}
	if c.State == "revoked" {
		return Connection{}, fault("connection_revoked", "a revoked connection cannot be reauthenticated")
	}
	c.State = "ready"
	c.ExpiresAt = b.now().Add(5 * time.Minute)
	b.data.Connections[c.Ref.ID] = c
	b.availability[c.Ref.ID] = availability{refreshable: true}
	if err := b.persistLocked(); err != nil {
		return Connection{}, err
	}
	return clone(c), nil
}

// RevokeConnection revokes a connection and fences authorities that depend on it.
func (b *Broker) RevokeConnection(ctx context.Context, p Principal, ref ConnectionRef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return err
	}
	if err := contextOK(ctx); err != nil {
		return err
	}
	c, err := b.connectionLocked(p, ref)
	if err != nil {
		return err
	}
	if c.State == "revoked" {
		return nil
	}
	c.State = "revoked"
	c.Epoch++
	b.data.Connections[c.Ref.ID] = c
	delete(b.availability, c.Ref.ID)
	// Acknowledgment happens only after the canonical admission fence is durable.
	return b.persistLocked()
}

// IssueAuthority creates an immutable bounded authority for the principal.
func (b *Broker) IssueAuthority(ctx context.Context, p Principal, req AuthorityRequest) (Authority, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return Authority{}, err
	}
	if err := contextOK(ctx); err != nil {
		return Authority{}, err
	}
	if err := b.validateGrantLocked(p, req); err != nil {
		return Authority{}, err
	}
	a := b.makeAuthority(p, req)
	a.SharedBudgetID = newID("budget_")
	b.data.Budgets[a.SharedBudgetID] = Budget{Limit: req.Budget}
	b.data.Authorities[authorityKey(a.Ref)] = a
	b.data.Fences[authorityKey(a.Ref)] = authorityFence{Epoch: 1, State: "active"}
	if err := b.persistLocked(); err != nil {
		return Authority{}, err
	}
	return clone(a), nil
}

func (b *Broker) validateGrantLocked(p Principal, r AuthorityRequest) error {
	if err := requiredID(r.SessionID, "sessionId"); err != nil {
		return err
	}
	if err := requiredID(r.ExecutionID, "executionId"); err != nil {
		return err
	}
	if r.ExecutionFence == 0 {
		return fault("invalid", "a server-bound execution fence is required")
	}
	if !r.NotAfter.After(b.now()) || r.NotAfter.After(b.now().Add(24*time.Hour)) {
		return fault("invalid", "prototype authority deadline must be future and within 24 hours")
	}
	if r.Budget < 1 || r.Budget > 10000 || r.MaxChildren < 0 || r.MaxChildren > 16 || r.MaxDepth < 0 || r.MaxDepth > 4 {
		return fault("invalid", "invalid bounded operation or child budget")
	}
	if len(r.Bindings) == 0 || len(r.AllowedActions) == 0 {
		return fault("invalid", "explicit connection bindings and actions are required")
	}
	seen := map[string]bool{}
	for _, binding := range r.Bindings {
		c, err := b.connectionLocked(p, binding.Connection)
		if err != nil {
			return err
		}
		if c.State == "revoked" {
			return fault("connection_revoked", "connection is revoked")
		}
		if seen[c.Ref.ID] || len(binding.Resources) == 0 {
			return fault("invalid", "duplicate or empty connection binding")
		}
		seen[c.Ref.ID] = true
		for _, r := range binding.Resources {
			if !containsResource(c.Resources, r) {
				return fault("forbidden_resource", "resource is outside the exact connection binding")
			}
		}
	}
	for _, action := range r.AllowedActions {
		provider, ok := actionProvider(action)
		if !ok {
			return fault("forbidden_action", "unsupported mediated action")
		}
		found := false
		for _, binding := range r.Bindings {
			for _, r := range binding.Resources {
				if r.Provider == provider {
					found = true
				}
			}
		}
		if !found {
			return fault("forbidden_action", "action requires a matching resource binding")
		}
	}
	return nil
}

func (b *Broker) makeAuthority(p Principal, r AuthorityRequest) Authority {
	return Authority{Ref: AuthorityRef{newID("auth_"), 1}, Actor: p.identity, SessionID: r.SessionID, ExecutionID: r.ExecutionID, ExecutionFence: r.ExecutionFence, Bindings: clone(r.Bindings), AllowedActions: clone(r.AllowedActions), IssuedAt: b.now(), NotAfter: r.NotAfter, MaxChildren: r.MaxChildren, MaxDepth: r.MaxDepth, Mode: "delegated-user-demo"}
}

func (b *Broker) authorityLocked(p Principal, ref AuthorityRef, checkActive bool) (Authority, error) {
	a, ok := b.data.Authorities[authorityKey(ref)]
	if !ok || a.Actor != p.identity {
		for _, existing := range b.data.Authorities {
			if existing.Ref.ID == ref.ID && existing.Actor == p.identity {
				return Authority{}, fault("stale_authority", "immutable authority revision does not match")
			}
		}
		return Authority{}, fault("not_found", "authority unavailable")
	}
	if a.Ref != ref {
		return Authority{}, fault("stale_authority", "immutable authority revision does not match")
	}
	if checkActive {
		current := a
		for depth := 0; ; depth++ {
			if depth > 4 {
				return Authority{}, fault("invalid_authority", "authority lineage exceeds the prototype bound")
			}
			f, ok := b.data.Fences[authorityKey(current.Ref)]
			if !ok || f.State != "active" {
				return Authority{}, fault("authority_stopped", "authority or its owning parent has stopped")
			}
			if !b.now().Before(current.NotAfter) {
				return Authority{}, fault("authority_expired", "authority deadline has expired")
			}
			if current.Parent == nil {
				break
			}
			parent, ok := b.data.Authorities[authorityKey(*current.Parent)]
			if !ok || parent.Ref != *current.Parent || parent.Actor != p.identity {
				return Authority{}, fault("invalid_authority", "authority lineage is unavailable")
			}
			current = parent
		}
	}
	return a, nil
}

// DeriveChild is a live attenuation edge. It keeps its parent's session, deadline,
// shared budget, exact bindings, and cancellation ancestry. It cannot mint credit.
func (b *Broker) DeriveChild(ctx context.Context, p Principal, parentRef AuthorityRef, req AuthorityRequest) (Authority, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return Authority{}, err
	}
	if err := contextOK(ctx); err != nil {
		return Authority{}, err
	}
	parent, err := b.authorityLocked(p, parentRef, true)
	if err != nil {
		return Authority{}, err
	}
	if req.SessionID != parent.SessionID || req.ExecutionID == parent.ExecutionID || req.NotAfter.After(parent.NotAfter) {
		return Authority{}, fault("attenuation_denied", "child must keep parent session, a distinct execution, and an equal or shorter deadline")
	}
	if parent.Depth >= parent.MaxDepth {
		return Authority{}, fault("child_limit", "maximum authority depth reached")
	}
	count := 0
	for _, a := range b.data.Authorities {
		if a.Parent != nil && a.Parent.ID == parent.Ref.ID {
			count++
		}
	}
	if count >= parent.MaxChildren {
		return Authority{}, fault("child_limit", "parent child admission budget exhausted")
	}
	if req.MaxChildren > parent.MaxChildren || req.MaxDepth > parent.MaxDepth {
		return Authority{}, fault("attenuation_denied", "child cannot widen child limits")
	}
	budget := b.data.Budgets[parent.SharedBudgetID]
	req.Budget = budget.Limit // No new budget is created or reset.
	if err = b.validateGrantLocked(p, req); err != nil {
		return Authority{}, err
	}
	for _, action := range req.AllowedActions {
		if !containsAction(parent.AllowedActions, action) {
			return Authority{}, fault("attenuation_denied", "child cannot add an action")
		}
	}
	for _, binding := range req.Bindings {
		var ceiling *Binding
		for i := range parent.Bindings {
			if parent.Bindings[i].Connection == binding.Connection {
				ceiling = &parent.Bindings[i]
				break
			}
		}
		if ceiling == nil {
			return Authority{}, fault("attenuation_denied", "child cannot acquire a new connection")
		}
		for _, r := range binding.Resources {
			if !containsResource(ceiling.Resources, r) {
				return Authority{}, fault("attenuation_denied", "child cannot widen exact resources")
			}
		}
	}
	a := b.makeAuthority(p, req)
	a.Parent = &parent.Ref
	a.SharedBudgetID = parent.SharedBudgetID
	a.Depth = parent.Depth + 1
	b.data.Authorities[authorityKey(a.Ref)] = a
	b.data.Fences[authorityKey(a.Ref)] = authorityFence{Epoch: 1, State: "active"}
	if err = b.persistLocked(); err != nil {
		return Authority{}, err
	}
	return clone(a), nil
}

// StopAuthority fences an authority and its descendants.
func (b *Broker) StopAuthority(ctx context.Context, p Principal, ref AuthorityRef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return err
	}
	if err := contextOK(ctx); err != nil {
		return err
	}
	a, err := b.authorityLocked(p, ref, false)
	if err != nil {
		return err
	}
	f := b.data.Fences[authorityKey(a.Ref)]
	if f.State == "stopped" {
		return nil
	}
	f.State = "stopped"
	f.Epoch++
	b.data.Fences[authorityKey(a.Ref)] = f
	return b.persistLocked()
}

// ListAuthorities returns the principal’s authority metadata and budgets.
func (b *Broker) ListAuthorities(p Principal) ([]AuthorityStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return nil, err
	}
	out := []AuthorityStatus{}
	for _, a := range b.data.Authorities {
		if a.Actor != p.identity {
			continue
		}
		f := b.data.Fences[authorityKey(a.Ref)]
		state := f.State
		if _, err := b.authorityLocked(p, a.Ref, true); err != nil {
			state = ErrorCode(err)
		}
		out = append(out, AuthorityStatus{Authority: clone(a), State: state, Budget: b.data.Budgets[a.SharedBudgetID], Epoch: f.Epoch})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Authority.Ref.ID < out[j].Authority.Ref.ID })
	return out, nil
}

// RebindExecution issues an immutable revision after trusted execution recovery.
// The host must verify prior writer custody before calling it. It retains the
// exact deadline, shared budget and scope, supersedes the old revision, and fences
// children still attached to that revision. It cannot revive stopped authority.
func (b *Broker) RebindExecution(ctx context.Context, p Principal, oldRef AuthorityRef, newFence uint64) (Authority, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return Authority{}, err
	}
	if err := contextOK(ctx); err != nil {
		return Authority{}, err
	}
	old, err := b.authorityLocked(p, oldRef, false)
	if err != nil {
		return Authority{}, err
	}
	if old.Parent != nil || newFence <= old.ExecutionFence {
		return Authority{}, fault("stale_fence", "only a recovered root execution with a strictly newer fence may rebind")
	}
	nextRef := AuthorityRef{ID: oldRef.ID, Version: oldRef.Version + 1}
	if existing, ok := b.data.Authorities[authorityKey(nextRef)]; ok {
		if existing.ExecutionFence != newFence {
			return Authority{}, fault("idempotency_conflict", "authority already rebound to another execution fence")
		}
		if _, err = b.authorityLocked(p, nextRef, true); err != nil {
			return Authority{}, err
		}
		return clone(existing), nil
	}
	if _, err = b.authorityLocked(p, oldRef, true); err != nil {
		return Authority{}, err
	}
	for _, binding := range old.Bindings {
		c, err := b.connectionLocked(p, binding.Connection)
		if err != nil {
			return Authority{}, err
		}
		if c.State == "revoked" {
			return Authority{}, fault("connection_revoked", "recovery cannot revive a revoked connection")
		}
	}
	next := clone(old)
	next.Ref = nextRef
	next.ExecutionFence = newFence
	fence := b.data.Fences[authorityKey(oldRef)]
	fence.State = "superseded"
	fence.Epoch++
	b.data.Fences[authorityKey(oldRef)] = fence
	b.data.Authorities[authorityKey(nextRef)] = next
	b.data.Fences[authorityKey(nextRef)] = authorityFence{Epoch: fence.Epoch, State: "active"}
	if err = b.persistLocked(); err != nil {
		return Authority{}, err
	}
	return clone(next), nil
}

func actionProvider(action Action) (Provider, bool) {
	switch action {
	case ReadWorkItem, AddComment:
		return ADO, true
	case QueryTelemetry:
		return AppInsights, true
	default:
		return "", false
	}
}
func containsResource(list []Resource, r Resource) bool {
	for _, candidate := range list {
		if candidate == r {
			return true
		}
	}
	return false
}
func containsAction(list []Action, a Action) bool {
	for _, candidate := range list {
		if candidate == a {
			return true
		}
	}
	return false
}
