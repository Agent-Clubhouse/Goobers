package readservice

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrChildHistoryUnavailable means this reader has no live custody source.
var ErrChildHistoryUnavailable = errors.New("child history unavailable")

// ChildHistorySource is a bounded read-only seam; readers cannot accept,
// reconcile, cancel, acknowledge or prune children through it.
type ChildHistorySource func(context.Context, triggerqueue.ChildParent, string, int) ([]triggerqueue.ChildRecord, error)

// ChildHistoryPage contains durable acceptance history, not live worker health.
// Pages are ordered by stable child ID, not by acceptance time. New acceptances
// may precede a cursor; refresh from the first page to include them.
type ChildHistoryPage struct {
	ReadStateEnvelope
	RunID      string             `json:"runId"`
	Gaggle     string             `json:"gaggle"`
	Status     string             `json:"status"`
	ObservedAt time.Time          `json:"observedAt"`
	Items      []ChildHistoryItem `json:"items"`
	NextCursor string             `json:"nextCursor"`
}

// ChildHistoryItem intentionally omits artifact references, proposal bytes and
// credentials. CancellationRequested alone is never proof of stopped work.
type ChildHistoryItem struct {
	ChildID               string                   `json:"childId"`
	RunID                 string                   `json:"runId"`
	StageOccurrence       string                   `json:"stageOccurrence"`
	InvocationKey         string                   `json:"invocationKey"`
	State                 triggerqueue.ChildState  `json:"state"`
	AcceptedAt            time.Time                `json:"acceptedAt"`
	UpdatedAt             time.Time                `json:"updatedAt"`
	TerminalAt            *time.Time               `json:"terminalAt,omitempty"`
	AcknowledgedAt        *time.Time               `json:"acknowledgedAt,omitempty"`
	ExpiredAt             *time.Time               `json:"expiredAt,omitempty"`
	CancellationRequested bool                     `json:"cancellationRequested"`
	Publication           *ChildPublicationHistory `json:"publication,omitempty"`
}

const childHistoryPageSize = 50

// RunChildren binds the queue read to the parent's on-disk identity. Callers
// cannot choose a gaggle. Reading never opens or creates a queue database.
func (s *Local) RunChildren(ctx context.Context, runID, cursor string) (ChildHistoryPage, error) {
	if err := ctx.Err(); err != nil {
		return ChildHistoryPage{}, err
	}
	run, err := s.openRun(runID)
	if err != nil {
		return ChildHistoryPage{}, err
	}
	parent := triggerqueue.ChildParent{Gaggle: run.identity.Gaggle, ParentRunID: run.identity.RunID}
	scope := parent.Gaggle + "/" + parent.ParentRunID
	after, err := childHistoryCursor(cursor, scope)
	if err != nil {
		return ChildHistoryPage{}, err
	}
	out := ChildHistoryPage{RunID: parent.ParentRunID, Gaggle: parent.Gaggle, Status: "unavailable", ObservedAt: s.now().UTC(), Items: []ChildHistoryItem{}}
	if s.sources.ChildHistory == nil {
		return annotated[ChildHistoryPage](ctx, s, out), nil
	}
	records, err := s.sources.ChildHistory(ctx, parent, after, childHistoryPageSize+1)
	if errors.Is(err, ErrChildHistoryUnavailable) {
		return annotated[ChildHistoryPage](ctx, s, out), nil
	}
	if err != nil {
		return ChildHistoryPage{}, err
	}
	if len(records) > childHistoryPageSize+1 {
		return ChildHistoryPage{}, errors.New("unbounded child history source")
	}
	out.Status = "recorded"
	for i, record := range records {
		if record.Identity.ChildParent != parent || record.ChildID <= after {
			return ChildHistoryPage{}, errors.New("invalid child history source")
		}
		after = record.ChildID
		if i == childHistoryPageSize {
			out.NextCursor = encodeCursor(pageCursor{Collection: "run-children", Scope: scope, After: records[i-1].ChildID})
			break
		}
		publication, err := s.childPublications(ctx, record)
		if err != nil {
			return ChildHistoryPage{}, err
		}
		out.Items = append(out.Items, ChildHistoryItem{ChildID: record.ChildID, RunID: record.RunID, StageOccurrence: record.Identity.StageOccurrence, InvocationKey: record.Identity.InvocationKey, State: record.State, AcceptedAt: record.AcceptedAt, UpdatedAt: record.UpdatedAt, TerminalAt: childHistoryTime(record.TerminalAt), AcknowledgedAt: childHistoryTime(record.AcknowledgedAt), ExpiredAt: childHistoryTime(record.TombstonedAt), CancellationRequested: record.CancellationRequested, Publication: publication})
	}
	return annotated[ChildHistoryPage](ctx, s, out), nil
}

func childHistoryCursor(cursor, scope string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > 2048 {
		return "", ErrInvalidCursor
	}
	decoded, err := decodeCursor(cursor)
	if err != nil || decoded.Collection != "run-children" || decoded.Scope != scope || len(decoded.After) > 128 {
		return "", ErrInvalidCursor
	}
	return decoded.After, nil
}

func childHistoryTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
