package childpublication

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

// Action identifies one of the two immutable publication intents per child.
type Action string

// Publication actions cannot select an arbitrary provider operation.
const (
	ActionBranch Action = "branch"
	ActionPR     Action = "pr"
)

// Valid reports whether an action is a supported immutable publication slot.
func (a Action) Valid() bool { return a == ActionBranch || a == ActionPR }

// Status is safe for the authorized child monitor: no credentials, authored
// title/body, full intent, workspace paths or retained source are exposed.
// CheckedAt/Observation describe this check; the command ledger owns its audit.
type Status struct {
	SourceRunID       string
	ExecutionEpoch    int
	Action            Action
	IntentDigest      string
	State             string
	Head              string
	Base              string
	Commit            string
	PullRequestURL    string
	PullRequestNumber int
	NeedsHuman        bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CheckedAt         time.Time
	Observation       string
}

// ObservationTarget is host-only, verified effect custody. The authorization
// bridge uses its exact repository to construct a currently permitted read-only
// observer; it must not accept a repository or provider credential from a body.
type ObservationTarget struct {
	SourceRunID    string
	ExecutionEpoch int
	Repository     providers.RepositoryRef
	Action         Action
	IntentDigest   string
	Head           string
	Base           string
	Commit         string
	ChildRunID     string
	ParentRunID    string
}

// EffectObserver contains no mutation method. A terminated/cancelled child may
// observe its previous provider effect under current human read authority; this
// capability cannot restart it, publish again, or change its immutable result.
type EffectObserver interface {
	GetBranch(context.Context, providers.RepositoryRef, string) (providers.BranchSummary, bool, error)
	FindPullRequestByBranch(context.Context, providers.RepositoryRef, string, string) (providers.PullRequestResult, bool, error)
}

// Reconciler confirms only already-admitted publication effects. The caller must
// hold the current gaggle run.intervene+repository.read lease across Check and
// use its existing idempotent command ledger for human attribution.
type Reconciler struct {
	Queue    *triggerqueue.Store
	Observer EffectObserver
}

// Inspect returns at most two verified publication slots in stable action order.
// The caller authorizes the qualified child before inspecting its custody.
func Inspect(ctx context.Context, q *triggerqueue.Store, id triggerqueue.ChildIdentity) ([]Status, error) {
	if q == nil {
		return nil, triggerqueue.ErrChildPublicationUnavailable
	}
	out := make([]Status, 0, 2)
	for _, action := range []Action{ActionBranch, ActionPR} {
		record, err := q.ChildPublication(ctx, id, string(action))
		if errors.Is(err, triggerqueue.ErrChildPublicationPending) {
			continue
		}
		if err != nil {
			return nil, err
		}
		target, err := inspectPublicationTarget(ctx, q, id, action, record)
		if err != nil {
			return nil, err
		}
		status, err := publicationStatus(record, target)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, nil
}

// InspectTarget verifies the exact immutable effect destination and digest. It
// exposes no raw intent text and grants no authority to call that provider.
func InspectTarget(ctx context.Context, q *triggerqueue.Store, id triggerqueue.ChildIdentity, action Action, expectedDigest string) (ObservationTarget, error) {
	if q == nil || !action.Valid() || !blobstore.ValidDigest(expectedDigest) {
		return ObservationTarget{}, triggerqueue.ErrChildPublicationUnavailable
	}
	record, err := q.ChildPublication(ctx, id, string(action))
	if err != nil {
		return ObservationTarget{}, err
	}
	if record.Digest != expectedDigest {
		return ObservationTarget{}, triggerqueue.ErrConflict
	}
	return inspectPublicationTarget(ctx, q, id, action, record)
}

func inspectPublicationTarget(ctx context.Context, q *triggerqueue.Store, id triggerqueue.ChildIdentity, action Action, record triggerqueue.ChildPublication) (ObservationTarget, error) {
	child, err := q.GetChild(ctx, id)
	if err != nil {
		return ObservationTarget{}, err
	}
	branch := record
	if action == ActionPR {
		branch, err = q.ChildPublication(ctx, id, string(ActionBranch))
		if err != nil {
			return ObservationTarget{}, err
		}
	}
	var source BranchIntent
	if err = json.Unmarshal(branch.Intent, &source); err != nil {
		return ObservationTarget{}, triggerqueue.ErrChildPublicationUnavailable
	}
	if source.RunID != branch.ExecutionRunID || record.ExecutionRunID != branch.ExecutionRunID || !matchesPublicationChild(source, child) || !matchesPublicationExecution(ctx, q, source, child) {
		return ObservationTarget{}, triggerqueue.ErrChildPublicationUnavailable
	}
	if action == ActionPR {
		var pr PRIntent
		if err = json.Unmarshal(record.Intent, &pr); err != nil {
			return ObservationTarget{}, triggerqueue.ErrChildPublicationUnavailable
		}
		if branch.State != "confirmed" || pr.Version != 1 || pr.BranchDigest != branch.Digest || pr.Request.Repository != source.Repository || pr.Request.Head != source.Head || pr.Request.Base != source.Base || pr.Request.RunID != source.RunID {
			return ObservationTarget{}, triggerqueue.ErrChildPublicationUnavailable
		}
	}
	return ObservationTarget{Repository: source.Repository, Action: action, IntentDigest: record.Digest, Head: source.Head, Base: source.Base, Commit: source.Commit, ChildRunID: child.RunID, SourceRunID: source.RunID, ExecutionEpoch: source.Lineage.ExecutionEpoch, ParentRunID: child.Identity.ParentRunID}, nil
}

func matchesPublicationChild(source BranchIntent, c triggerqueue.ChildRecord) bool {
	lineage := source.Lineage
	owner := lineage.Gaggle == c.Identity.Gaggle && lineage.ParentRunID == c.Identity.ParentRunID && lineage.StageOccurrence == c.Identity.StageOccurrence && lineage.InvocationKey == c.Identity.InvocationKey && lineage.AcceptanceID == c.AcceptanceID
	pins := blobstore.ValidDigest(lineage.SourceDigest) && blobstore.ValidDigest(lineage.EnvelopeDigest) && (c.ProposalDigest == "" || c.ProposalDigest == lineage.SourceDigest)
	target := source.Repository.Owner != "" && source.Repository.Name != "" && source.Base != "" && source.Base != source.Head && strings.HasSuffix(source.Head, "/children/"+source.RunID) && commitID.MatchString(source.Commit)
	return source.Version == 1 && owner && pins && target
}

// Observation selects the immutable emitting epoch, which may no longer be
// active. This metadata check authorizes no new execution or provider effect.
func matchesPublicationExecution(ctx context.Context, q *triggerqueue.Store, source BranchIntent, child triggerqueue.ChildRecord) bool {
	e, err := q.ChildExecutionMetadata(ctx, child.Identity, source.RunID)
	if err != nil || e.Epoch != source.Lineage.ExecutionEpoch {
		return false
	}
	if e.Epoch == 0 {
		return source.RunID == child.RunID && source.Lineage.PriorResultRef == "" && source.Lineage.RestartDigest == ""
	}
	return source.Lineage.PriorResultRef == e.SourceResultRef && source.Lineage.RestartDigest == e.RequestDigest
}

func publicationStatus(record triggerqueue.ChildPublication, target ObservationTarget) (Status, error) {
	status := Status{SourceRunID: target.SourceRunID, ExecutionEpoch: target.ExecutionEpoch, Action: target.Action, IntentDigest: record.Digest, State: record.State, Head: target.Head, Base: target.Base, Commit: target.Commit, NeedsHuman: record.State == "effect_pending", CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
	switch record.State {
	case "prepared":
		status.Observation = "no_effect_begun"
	case "effect_pending":
		status.Observation = "pending"
	case "confirmed":
		status.Observation = "confirmed"
		if target.Action == ActionBranch {
			var receipt BranchReceipt
			if json.Unmarshal(record.Receipt, &receipt) != nil || receipt.Head != target.Head || receipt.SHA != target.Commit {
				return Status{}, triggerqueue.ErrChildPublicationUnavailable
			}
		} else {
			var receipt providers.PullRequestResult
			if json.Unmarshal(record.Receipt, &receipt) != nil || !validObservedPR(receipt) {
				return Status{}, triggerqueue.ErrChildPublicationUnavailable
			}
			status.PullRequestURL, status.PullRequestNumber = receipt.URL, receipt.Number
		}
	default:
		return Status{}, triggerqueue.ErrChildPublicationUnavailable
	}
	return status, nil
}

// Check performs bounded reads against an exact previously admitted target. A
// prepared slot reports no effect begun; an unobserved or changed remote effect
// stays pending. Successful confirmation never changes child state or ResultRef.
func (r Reconciler) Check(ctx context.Context, id triggerqueue.ChildIdentity, action Action, expectedDigest string) (Status, error) {
	if r.Observer == nil {
		return Status{}, triggerqueue.ErrChildPublicationUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, EffectTimeout)
	defer cancel()
	target, err := InspectTarget(ctx, r.Queue, id, action, expectedDigest)
	if err != nil {
		return Status{}, err
	}
	record, err := r.Queue.ChildPublication(ctx, id, string(action))
	if err != nil {
		return Status{}, err
	}
	status, err := publicationStatus(record, target)
	if err != nil {
		return Status{}, err
	}
	if record.State != "effect_pending" {
		return status, nil
	}
	receipt, observation, err := r.observe(ctx, target)
	if err != nil {
		return Status{}, err
	}
	status.CheckedAt = time.Now().UTC()
	status.Observation = observation
	if receipt == nil {
		return status, nil
	}
	if err = r.Queue.ConfirmChildPublication(ctx, record, receipt); err != nil {
		return Status{}, err
	}
	confirmed, err := r.Queue.ChildPublication(ctx, id, string(action))
	if err != nil {
		return Status{}, err
	}
	final, err := publicationStatus(confirmed, target)
	final.CheckedAt = status.CheckedAt
	return final, err
}

func (r Reconciler) observe(ctx context.Context, target ObservationTarget) ([]byte, string, error) {
	branch, found, err := r.Observer.GetBranch(ctx, target.Repository, target.Head)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "not_observed", nil
	}
	if branch.Name != target.Head || branch.SHA != target.Commit {
		return nil, "branch_changed", nil
	}
	if target.Action == ActionBranch {
		raw, err := json.Marshal(BranchReceipt{Head: target.Head, SHA: target.Commit})
		return raw, "confirmed", err
	}
	pr, found, err := r.Observer.FindPullRequestByBranch(ctx, target.Repository, target.Head, target.Base)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "not_observed", nil
	}
	if !validObservedPR(pr) {
		return nil, "", triggerqueue.ErrChildPublicationUnavailable
	}
	raw, err := json.Marshal(pr)
	return raw, "confirmed", err
}

func validObservedPR(pr providers.PullRequestResult) bool {
	if pr.ID == "" || len(pr.ID) > 1024 || len(pr.URL) > 4096 || pr.Number < 1 {
		return false
	}
	url, err := url.Parse(pr.URL)
	return err == nil && url.User == nil && url.Host != "" && (url.Scheme == "https" || url.Scheme == "http")
}
