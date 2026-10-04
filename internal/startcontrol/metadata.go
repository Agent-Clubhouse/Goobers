package startcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/enginestartintent"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/restartintent"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrLegacyUnscoped denotes old requests that lack immutable gaggle/generation
// provenance. Existing dispatch remains compatible, but new controls refuse it.
var ErrLegacyUnscoped = errors.New("start controls: legacy request lacks pinned scope")

// Metadata retains typed source selectors for archive qualification. Child and
// session display identities do not masquerade as configured parent workflows.
type Metadata struct {
	Scope                          triggerqueue.StartScope
	ArchiveWorkflow, ArchiveGoober string
	ExplicitDeadline               time.Time
}

// Describe validates source-specific receipts before deriving display scope.
// Caller-submitted control bodies can never populate these fields.
func Describe(ctx context.Context, q *triggerqueue.Store, r triggerqueue.Record) (Metadata, error) {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(r.Payload, &header); err != nil {
		return Metadata{}, err
	}
	var m Metadata
	var err error
	switch header.Kind {
	case startintent.Kind:
		m, err = ordinaryMetadata(r)
	case eventing.StartKind:
		m, err = eventMetadata(ctx, q, r)
	case childworkflow.ChildStartKind:
		m, err = childMetadata(ctx, q, r)
	case sessioning.StartKind:
		m, err = sessionMetadata(ctx, q, r)
	case restartintent.Kind:
		m, err = restartMetadata(ctx, q, r)
	case enginestartintent.Kind:
		m, err = engineMetadata(ctx, q, r)
	case "":
		return m, ErrLegacyUnscoped
	default:
		return m, errors.New("start controls: unsupported accepted source")
	}
	if err != nil {
		return m, err
	}
	m.Scope.Kind = header.Kind
	m.Scope.PayloadDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(r.Payload))
	if m.Scope.ReservedRunID == "" {
		m.Scope.ReservedRunID = strings.TrimPrefix(r.ID, "trigger-")
	}
	return m, nil
}
func ordinaryMetadata(r triggerqueue.Record) (Metadata, error) {
	e, err := startintent.Parse(r.Payload)
	if err != nil {
		return Metadata{}, err
	}
	source := "manual"
	if e.Source != nil {
		source = "signal"
		if e.Source.WorkerKind != "" {
			source = "backlog"
		}
		if !e.Source.ScheduledAt.IsZero() {
			source = "schedule"
		}
	}
	return Metadata{Scope: triggerqueue.StartScope{Gaggle: e.Target.Gaggle, Workflow: e.Target.Workflow, Generation: e.Target.ConfigGeneration, Source: source}, ArchiveWorkflow: e.Target.Workflow, ExplicitDeadline: e.Deadline}, nil
}
func eventMetadata(ctx context.Context, q *triggerqueue.Store, r triggerqueue.Record) (Metadata, error) {
	e, err := eventing.ParseStart(r.Payload)
	if err != nil {
		return Metadata{}, err
	}
	verified, _, err := q.VerifiedEventStart(ctx, e.Gaggle, e.GroupID)
	if err != nil {
		return Metadata{}, err
	}
	if verified.ID != r.ID || string(verified.Payload) != string(r.Payload) {
		return Metadata{}, triggerqueue.ErrConflict
	}
	return Metadata{Scope: triggerqueue.StartScope{Gaggle: e.Gaggle, Workflow: e.Workflow, Generation: e.ConfigGeneration, Source: "event"}, ArchiveWorkflow: e.Workflow}, nil
}
func childMetadata(ctx context.Context, q *triggerqueue.Store, r triggerqueue.Record) (Metadata, error) {
	e, err := childworkflow.DecodeStartEnvelope(r.Payload)
	if err != nil {
		return Metadata{}, err
	}
	verified, err := q.ChildStart(ctx, e.Identity())
	if err != nil {
		return Metadata{}, err
	}
	if verified.ID != r.ID || string(verified.Payload) != string(r.Payload) {
		return Metadata{}, triggerqueue.ErrConflict
	}
	return Metadata{Scope: triggerqueue.StartScope{Gaggle: e.Gaggle, Workflow: e.Workflow, Generation: e.ConfigGeneration, Source: "child"}, ArchiveWorkflow: e.ParentWorkflow}, nil
}
func sessionMetadata(ctx context.Context, q *triggerqueue.Store, r triggerqueue.Record) (Metadata, error) {
	turn, err := q.SessionTurn(ctx, r.ID)
	if err != nil {
		return Metadata{}, err
	}
	if string(turn.Record.Payload) != string(r.Payload) {
		return Metadata{}, triggerqueue.ErrConflict
	}
	return Metadata{Scope: triggerqueue.StartScope{Gaggle: turn.Session.Gaggle, Workflow: "session/" + turn.Session.ID, Generation: turn.Session.ConfigGeneration, Source: "session"}, ArchiveGoober: turn.Session.Goober}, nil
}
func restartMetadata(ctx context.Context, q *triggerqueue.Store, r triggerqueue.Record) (Metadata, error) {
	plan, err := (&restartintent.Service{Queue: q}).Load(ctx, r)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Scope: triggerqueue.StartScope{Gaggle: plan.Source.Gaggle, Workflow: plan.Source.Workflow, Generation: plan.Source.ConfigGeneration, Source: "human-restart", ReservedRunID: plan.Continuation.RunID}, ArchiveWorkflow: plan.Source.Workflow}, nil
}
func engineMetadata(ctx context.Context, q *triggerqueue.Store, r triggerqueue.Record) (Metadata, error) {
	e, err := enginestartintent.Parse(r.Payload)
	if err != nil {
		return Metadata{}, err
	}
	raw, err := q.DirectEngineInput(ctx, r.ID)
	if err != nil {
		return Metadata{}, err
	}
	if _, err = e.Input(raw); err != nil {
		return Metadata{}, err
	}
	return Metadata{Scope: triggerqueue.StartScope{Gaggle: e.Request.Gaggle, Workflow: e.Request.Workflow, Generation: e.ConfigGeneration, Source: "direct-engine", ReservedRunID: e.RunID}, ArchiveWorkflow: e.Request.Workflow}, nil
}
