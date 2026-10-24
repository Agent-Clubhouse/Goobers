package terminalwriter

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// AppliedSet returns the applied configuration's gaggles and the digest of the
// generation they came from; ok is false before any is applied.
type AppliedSet func() (set *instance.ConfigSet, digest string, ok bool)

// InstanceWriter is the one credentialed write path that is not bound to a run. The
// controller's sweep uses it to escalate a dependency cycle it had deferred, after
// the recording run's pin was swept, so there is no run to bind credentials from.
// It is locked down to exactly that:
//
//   - the repository must be the backlog repository of a configured gaggle
//     (InstanceBinding); anything else is refused as not pinned;
//   - the only writes are adding goobers:needs-human, removing goobers:ready and
//     goobers:claimed, and a comment that carries an idempotency key;
//   - nothing else can be expressed: no title, body, assignee, milestone, state (so
//     an issue is never opened or closed), revision, and no pull-request operation
//     at all;
//   - every write is journaled before it is made, and a write whose journal entry
//     cannot be appended (a failed or fenced append: a deposed leader's included) is
//     refused.
//
// Credentials are materialized per call for issues:write on that repository alone,
// exactly as for the run-bound Writer, and every failure is scrubbed.
type InstanceWriter struct {
	applied  AppliedSet
	backends Backends
	scrubber journal.Scrubber
	// Journal appends one event per write, before it is attempted; its failure
	// refuses the write. Required, and it should be the epoch's fenced append, so
	// that a deposed leader cannot make a credentialed write.
	Journal func(journal.Event) error
}

// NewInstanceWriter returns an InstanceWriter over the applied configuration.
func NewInstanceWriter(applied AppliedSet, backends Backends, scrubber journal.Scrubber, record func(journal.Event) error) (*InstanceWriter, error) {
	if applied == nil || backends == nil || scrubber == nil || record == nil {
		return nil, errors.New("instance writer requires an applied configuration, backends, a scrubber and a journal")
	}
	return &InstanceWriter{applied: applied, backends: backends, scrubber: scrubber, Journal: record}, nil
}

// InstanceWriteOperation names the journal events the writer emits.
const InstanceWriteOperation = "instance_escalation_write"

// UpdateWorkItem applies req if it is an escalation write the writer allows.
func (w *InstanceWriter) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if err := validateEscalation(req); err != nil {
		return providers.WorkItem{}, failWith(w.scrubber, "instance escalation", err)
	}
	set, digest, ok := w.applied()
	if !ok {
		// Carries no provider text, so it needs no scrubbing, and callers retry on it.
		return providers.WorkItem{}, fmt.Errorf("instance escalation: %w", ErrNoAppliedConfig)
	}
	bound, ok := InstanceBinding(set, digest, req.Repository)
	if !ok {
		return providers.WorkItem{}, failWith(w.scrubber, "instance escalation",
			fmt.Errorf("%s is not a configured gaggle's backlog repository: %w", req.Repository.CanonicalKey(), providerwriter.ErrRepositoryNotPinned))
	}
	backend, err := w.backends.For(ctx, bound, req.Repository, string(capability.GitHubIssuesWrite))
	if err != nil {
		return providers.WorkItem{}, failWith(w.scrubber, "instance escalation", err)
	}
	if req.Comment != "" && backend.Kind() == providers.ProviderADO {
		// Azure Boards ignores IdempotencyKey: a retried post would duplicate.
		return providers.WorkItem{}, failWith(w.scrubber, "instance escalation", fmt.Errorf("comment on %s: %w", backend.Kind(), ErrUnsupported))
	}
	if err := w.Journal(journal.Event{
		Type: journal.EventRunnerAnnotation, Reason: "instance-level escalation write",
		Runner: map[string]any{
			"kind": InstanceWriteOperation, "repository": req.Repository.CanonicalKey(), "itemId": req.ID,
			"addLabels": req.AddLabels, "removeLabels": req.RemoveLabels, "idempotencyKey": req.IdempotencyKey,
			"commented": req.Comment != "", "driver": string(journal.DriverEngine),
		},
	}); err != nil {
		// Not journaled, not written. The failure is the fenced append's own, so a
		// deposed leader's ErrStale passes through for the caller to end its epoch.
		return providers.WorkItem{}, fmt.Errorf("journal the escalation write: %w", err)
	}
	item, err := backend.UpdateWorkItem(ctx, req)
	return item, failWith(w.scrubber, "instance escalation", err)
}

// Allowed escalation labels. Anything else is refused.
var (
	escalationAdd    = []string{providers.LabelNeedsHuman}
	escalationRemove = []string{providers.LabelReady, providers.LabelClaimed}
)

// validateEscalation allows only what escalating a cycle needs.
func validateEscalation(req providers.UpdateWorkItemRequest) error {
	switch {
	case req.ID == "":
		return fmt.Errorf("an escalation names an item: %w", providerwriter.ErrPermanent)
	case req.Title != nil || req.Body != nil || req.Assignee != nil || req.Milestone != nil:
		return fmt.Errorf("an escalation may not edit the item: %w", providerwriter.ErrPermanent)
	case req.State != "":
		return fmt.Errorf("an escalation may not open or close an item: %w", providerwriter.ErrPermanent)
	case req.ExpectedRevision != "":
		return fmt.Errorf("an escalation carries no revision: %w", providerwriter.ErrPermanent)
	}
	for _, label := range req.AddLabels {
		if !slices.Contains(escalationAdd, label) {
			return fmt.Errorf("an escalation may add only %v, not %q: %w", escalationAdd, label, providerwriter.ErrPermanent)
		}
	}
	for _, label := range req.RemoveLabels {
		if !slices.Contains(escalationRemove, label) {
			return fmt.Errorf("an escalation may remove only %v, not %q: %w", escalationRemove, label, providerwriter.ErrPermanent)
		}
	}
	if len(req.AddLabels) == 0 && len(req.RemoveLabels) == 0 && req.Comment == "" {
		return fmt.Errorf("an empty escalation: %w", providerwriter.ErrPermanent)
	}
	if req.Comment != "" && req.IdempotencyKey == "" {
		return fmt.Errorf("a comment needs an idempotency key: %w", providerwriter.ErrPermanent)
	}
	return nil
}
