package interactiveauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/secretpattern"
)

func requestDigest(req OperationRequest) string {
	req.DecisionID = ""
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateOperation(req OperationRequest) error {
	for name, value := range map[string]string{"operationId": req.OperationID, "actionId": req.ActionID, "idempotencyKey": req.IdempotencyKey, "sessionId": req.SessionID, "executionId": req.ExecutionID} {
		if err := requiredID(value, name); err != nil {
			return err
		}
	}
	if _, ok := actionProvider(req.Action); !ok {
		return fault("forbidden_action", "unsupported mediated action")
	}
	if len(req.Payload.Comment) > 4096 || len(req.Payload.Query) > 4096 {
		return fault("invalid", "provider payload exceeds the prototype limit")
	}
	raw, _ := json.Marshal(req.Payload)
	scrubbed := secretpattern.NewScrubber().Scrub(raw)
	if string(raw) != string(scrubbed) {
		return fault("secret_rejected", "provider payload appears to contain a credential; remove it before submitting")
	}
	switch req.Action {
	case ReadWorkItem:
		if req.Payload.WorkItemID <= 0 || req.Payload.Comment != "" || req.Payload.Query != "" {
			return fault("invalid", "ADO read requires only a positive work item ID")
		}
	case AddComment:
		if req.Payload.WorkItemID <= 0 || strings.TrimSpace(req.Payload.Comment) == "" || req.Payload.Query != "" {
			return fault("invalid", "ADO comment requires a positive work item ID and comment text")
		}
	case QueryTelemetry:
		// The local connector demonstrates a useful native telemetry query without
		// pretending to implement arbitrary KQL or authorizing cross-resource union.
		if req.Payload.Query != "requests | summarize count() by resultCode" || req.Payload.WorkItemID != 0 || req.Payload.Comment != "" {
			return fault("unsupported_query", "controlled App Insights supports: requests | summarize count() by resultCode")
		}
	}
	return nil
}

func (b *Broker) operationScopeLocked(p Principal, req OperationRequest) (Authority, Connection, error) {
	a, err := b.authorityLocked(p, req.Authority, true)
	if err != nil {
		return Authority{}, Connection{}, err
	}
	if a.SessionID != req.SessionID || a.ExecutionID != req.ExecutionID {
		return Authority{}, Connection{}, fault("execution_mismatch", "authority is bound to a different session or execution")
	}
	if a.ExecutionFence != req.ExecutionFence {
		return Authority{}, Connection{}, fault("stale_fence", "authority belongs to a different execution attempt fence")
	}
	if !containsAction(a.AllowedActions, req.Action) {
		return Authority{}, Connection{}, fault("forbidden_action", "action is outside this authority")
	}
	provider, _ := actionProvider(req.Action)
	if provider != req.Resource.Provider {
		return Authority{}, Connection{}, fault("forbidden_resource", "action and resource provider do not match")
	}
	var selected *Connection
	for _, binding := range a.Bindings {
		if !containsResource(binding.Resources, req.Resource) {
			continue
		}
		c, err := b.connectionLocked(p, binding.Connection)
		if err != nil {
			return Authority{}, Connection{}, err
		}
		if !containsResource(c.Resources, req.Resource) {
			return Authority{}, Connection{}, fault("forbidden_resource", "resource no longer belongs to the verified connection")
		}
		if selected != nil {
			return Authority{}, Connection{}, fault("invalid_authority", "ambiguous connection binding")
		}
		selected = &c
	}
	if selected == nil {
		return Authority{}, Connection{}, fault("forbidden_resource", "exact resource is outside this authority")
	}
	if selected.State == "revoked" {
		return Authority{}, Connection{}, fault("connection_revoked", "connection was revoked; no new operation is admitted")
	}
	return a, *selected, nil
}

// Authorize records an exact owner decision for a controlled ADO mutation.
// The HTTP layer must call this only for an authenticated explicit approval.
// Reads are admitted by the authority's bounded policy without a second decision.
func (b *Broker) Authorize(ctx context.Context, p Principal, req OperationRequest, notAfter time.Time) (Decision, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return Decision{}, err
	}
	if err := contextOK(ctx); err != nil {
		return Decision{}, err
	}
	if err := validateOperation(req); err != nil {
		return Decision{}, err
	}
	a, _, err := b.operationScopeLocked(p, req)
	if err != nil {
		return Decision{}, err
	}
	if req.Action != AddComment {
		return Decision{}, fault("invalid", "exact decisions are required only for the controlled ADO comment mutation")
	}
	if !notAfter.After(b.now()) || notAfter.After(a.NotAfter) {
		return Decision{}, fault("invalid", "decision must expire within its authority deadline")
	}
	digest := requestDigest(req)
	for _, decision := range b.data.Decisions {
		if decision.Authority == req.Authority && decision.OperationID == req.OperationID {
			if decision.PayloadDigest != digest {
				return Decision{}, fault("idempotency_conflict", "operation approval intent changed")
			}
			return clone(decision), nil
		}
	}
	d := Decision{ID: newID("decision_"), Authority: req.Authority, OperationID: req.OperationID, PayloadDigest: digest, Approver: p.identity, NotAfter: notAfter, CreatedAt: b.now()}
	b.data.Decisions[d.ID] = d
	if err = b.persistLocked(); err != nil {
		return Decision{}, err
	}
	return clone(d), nil
}

func (b *Broker) refreshLocked(c Connection) (Connection, error) {
	if c.State == "interaction_required" || c.State == "consent_required" {
		return c, fault(c.State, "controlled connection requires its owner to reauthenticate")
	}
	if c.State == "revoked" {
		return c, fault("connection_revoked", "connection was revoked")
	}
	if c.State == "expired" || !b.now().Before(c.ExpiresAt) {
		if !b.availability[c.Ref.ID].refreshable {
			c.State = "interaction_required"
			b.data.Connections[c.Ref.ID] = c
			if err := b.persistLocked(); err != nil {
				return c, err
			}
			return c, fault("interaction_required", "controlled renewal context is unavailable")
		}
		c.State = "ready"
		c.ExpiresAt = b.now().Add(5 * time.Minute)
		c.RefreshCount++
		b.data.Connections[c.Ref.ID] = c
		if err := b.persistLocked(); err != nil {
			return c, err
		}
	}
	return c, nil
}

// Execute serializes final admission with authority/connection revocation and
// durably marks dispatch before crossing the provider boundary. A later revoke
// fences NEW admission; an already admitted request can still finish afterward.
func (b *Broker) Execute(ctx context.Context, p Principal, req OperationRequest) (Operation, error) {
	b.mu.Lock()
	if err := b.readyLocked(p); err != nil {
		b.mu.Unlock()
		return Operation{}, err
	}
	if err := contextOK(ctx); err != nil {
		b.mu.Unlock()
		return Operation{}, err
	}
	if err := validateOperation(req); err != nil {
		b.mu.Unlock()
		return Operation{}, err
	}
	if _, err := b.authorityLocked(p, req.Authority, false); err != nil {
		b.mu.Unlock()
		return Operation{}, err
	}
	digest := requestDigest(req)
	if prior, ok := b.data.Operations[req.OperationID]; ok {
		if prior.Authority != req.Authority || prior.PayloadDigest != digest {
			b.mu.Unlock()
			return Operation{}, fault("idempotency_conflict", "stable operation ID cannot be reused for different intent")
		}
		out := clone(prior)
		b.mu.Unlock()
		if prior.State == "outcome_unknown" {
			return out, fault("outcome_unknown", "reconcile this operation; it will not be replayed")
		}
		return out, nil
	}
	for _, op := range b.data.Operations {
		if op.Authority.ID == req.Authority.ID && op.IdempotencyKey == req.IdempotencyKey {
			b.mu.Unlock()
			return Operation{}, fault("idempotency_conflict", "idempotency key is already bound to another operation")
		}
	}
	a, c, err := b.operationScopeLocked(p, req)
	if err != nil {
		b.mu.Unlock()
		return Operation{}, err
	}
	if req.Action == AddComment {
		d, ok := b.data.Decisions[req.DecisionID]
		if !ok || d.Authority != a.Ref || d.OperationID != req.OperationID || d.PayloadDigest != digest || d.Approver != p.identity {
			b.mu.Unlock()
			return Operation{}, fault("decision_required", "an exact owner decision is required before this provider mutation")
		}
		if !b.now().Before(d.NotAfter) {
			b.mu.Unlock()
			return Operation{}, fault("decision_expired", "the exact decision expired; reauthentication does not extend it")
		}
	}
	c, err = b.refreshLocked(c)
	if err != nil {
		b.mu.Unlock()
		return Operation{}, err
	}
	budget := b.data.Budgets[a.SharedBudgetID]
	if budget.Used >= budget.Limit {
		b.mu.Unlock()
		return Operation{}, fault("budget_exhausted", "shared parent/child operation budget is exhausted")
	}
	if b.checkExecution != nil {
		if req.TurnID == "" || req.LeaseID == "" {
			b.mu.Unlock()
			return Operation{}, fault("execution_unavailable", "a current server turn and writer lease are required")
		}
		if err = b.checkExecution(ctx, a.Actor, req); err != nil {
			b.mu.Unlock()
			return Operation{}, fault("execution_unavailable", "server execution lease or current fence could not be verified")
		}
	}
	budget.Used++
	b.data.Budgets[a.SharedBudgetID] = budget
	op := Operation{ID: req.OperationID, Authority: a.Ref, SessionID: a.SessionID, ExecutionID: a.ExecutionID, ExecutionFence: a.ExecutionFence, ActionID: req.ActionID, Action: req.Action, Resource: req.Resource, PayloadDigest: digest, IdempotencyKey: req.IdempotencyKey, State: "dispatched", Mode: ControlledLocal, Connection: c.Ref, AuthorityEpoch: b.data.Fences[authorityKey(a.Ref)].Epoch, ConnectionEpoch: c.Epoch, DispatchedAt: b.now()}
	b.data.Operations[op.ID] = op
	if err = b.persistLocked(); err != nil {
		b.mu.Unlock()
		return Operation{}, err
	}
	hook := b.afterAdmission
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return b.dispatchControlled(ctx, req)
}

func (b *Broker) dispatchControlled(ctx context.Context, req OperationRequest) (Operation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	op := b.data.Operations[req.OperationID]
	if b.closed || b.storageFailed {
		return clone(op), fault("outcome_unknown", "broker became unavailable after dispatch admission")
	}
	if ctx.Err() != nil {
		op.State = "failed"
		op.ErrorCode = "cancelled"
		op.Message = "Controlled provider was not called: execution cancelled after admission."
		now := b.now()
		op.FinishedAt = &now
		b.data.Operations[op.ID] = op
		if err := b.persistLocked(); err != nil {
			return Operation{}, err
		}
		return clone(op), fault("cancelled", op.Message)
	}
	result, err := b.controlledResultLocked(req)
	if b.storageFailed {
		op.State = "outcome_unknown"
		op.ErrorCode = "outcome_unknown"
		op.Message = "Provider persistence is uncertain after dispatch; broker is fenced until recovery."
		return clone(op), fault("outcome_unknown", op.Message)
	}
	if err != nil {
		op.State = "failed"
		op.ErrorCode = ErrorCode(err)
		op.Message = "Controlled provider rejected the request without applying an effect."
	} else if req.SimulateLostResponse {
		op.State = "outcome_unknown"
		op.ErrorCode = "outcome_unknown"
		op.Message = "Controlled provider response was deliberately lost; reconcile the original operation."
	} else {
		op.State = "confirmed"
		op.Result = result
		op.ProviderRequestID = "controlled/" + op.ID
	}
	now := b.now()
	op.FinishedAt = &now
	b.data.Operations[op.ID] = op
	if persistErr := b.persistLocked(); persistErr != nil {
		return Operation{}, persistErr
	}
	if op.State == "outcome_unknown" {
		return clone(op), fault("outcome_unknown", op.Message)
	}
	return clone(op), err
}

func (b *Broker) controlledResultLocked(req OperationRequest) (map[string]any, error) {
	switch req.Action {
	case ReadWorkItem:
		if req.Payload.WorkItemID != 427 {
			return nil, fault("provider_not_found", "controlled ADO fixture contains work item 427 only")
		}
		return map[string]any{"mode": ControlledLocal, "id": 427, "title": "Invoice total becomes negative when the discount exceeds the subtotal", "state": "Active", "description": "Investigate invoice.mjs and invoice.test.mjs. An excessive discount can produce a negative invoice total. Add a regression test and correlate the controlled App Insights failures. This is local fixture data, not a live ADO work item.", "repository": "sample-app", "url": "https://dev.azure.com/goobers-demo/interactive/_workitems/edit/427", "expectedBehavior": "Invoice total is clamped to zero when the discount exceeds the subtotal; ordinary discounts still work."}, nil
	case QueryTelemetry:
		return map[string]any{"mode": ControlledLocal, "query": req.Payload.Query, "resource": DemoInsightsResource(), "columns": []string{"resultCode", "count_"}, "rows": [][]any{{"200", 118}, {"422", 17}}, "note": "Controlled local telemetry: 17 invoice requests rejected with HTTP 422 after an excessive discount yielded a negative total. No Azure query was sent."}, nil
	case AddComment:
		if req.Payload.WorkItemID != 427 {
			return nil, fault("provider_not_found", "controlled ADO fixture contains work item 427 only")
		}
		result := map[string]any{"mode": ControlledLocal, "workItemId": 427, "commentId": "controlled/" + req.OperationID, "text": req.Payload.Comment, "note": "Written only to the controlled local provider ledger; no ADO service was called."}
		b.data.DemoComments[req.OperationID] = result
		// The provider commits independently of receiving its response. This first
		// durable write supports a crash between effect and response persistence.
		if err := b.persistLocked(); err != nil {
			return nil, err
		}
		return result, nil
	default:
		return nil, fault("forbidden_action", "unsupported provider action")
	}
}

// Reconcile performs only a controlled-provider receipt lookup. Missing evidence
// remains unknown; it never submits the original write again. Reconciliation of
// already-sent operations is allowed after revocation and cannot grant new effects.
func (b *Broker) Reconcile(ctx context.Context, p Principal, operationID string) (Operation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return Operation{}, err
	}
	if err := contextOK(ctx); err != nil {
		return Operation{}, err
	}
	op, ok := b.data.Operations[operationID]
	if !ok {
		return Operation{}, fault("not_found", "operation unavailable")
	}
	if _, err := b.authorityLocked(p, op.Authority, false); err != nil {
		return Operation{}, err
	}
	if op.State != "outcome_unknown" {
		return clone(op), nil
	}
	if receipt, ok := b.data.DemoComments[op.ID]; ok {
		op.State = "confirmed"
		op.Result = clone(receipt)
		op.ErrorCode = ""
		op.Message = "Reconciled from the controlled provider's durable receipt without replay."
		op.ProviderRequestID = "controlled/" + op.ID
		now := b.now()
		op.FinishedAt = &now
		b.data.Operations[op.ID] = op
		if err := b.persistLocked(); err != nil {
			return Operation{}, err
		}
		return clone(op), nil
	}
	return clone(op), fault("outcome_unknown", "no committed provider receipt exists; automatic replay remains prohibited")
}

// ListOperations returns the principal’s operation receipts for a session.
func (b *Broker) ListOperations(p Principal, sessionID string) ([]Operation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readyLocked(p); err != nil {
		return nil, err
	}
	out := []Operation{}
	for _, op := range b.data.Operations {
		a, ok := b.data.Authorities[authorityKey(op.Authority)]
		if ok && a.Actor == p.identity && (sessionID == "" || op.SessionID == sessionID) {
			out = append(out, clone(op))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DispatchedAt.Before(out[j].DispatchedAt) })
	return out, nil
}
