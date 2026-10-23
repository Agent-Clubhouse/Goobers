// Package terminalreport retains terminal provider reports until publication
// succeeds. Replays publish the captured projection, never re-charge a run's
// failure streak or depend on claims that terminal finalization has released.
package terminalreport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/providers"
)

// Kind identifies the idempotent provider projection to publish.
type Kind string

// Supported terminal projections.
const (
	WorkFailure  Kind = "work-failure"
	InfraFailure Kind = "infra-failure"
	AbortLabel   Kind = "abort-label"
)

// ErrSuperseded discards a report whose originating run has since resumed.
var ErrSuperseded = errors.New("terminal report superseded")

// Effect is a credential-free publication intent. Reset uses the same key as
// the corresponding failure projection, so a later success replaces a pending
// failure comment instead of allowing it to resurrect an obsolete streak.
type Effect struct {
	Kind        Kind                    `json:"kind"`
	Attribution providers.Attribution   `json:"attribution"`
	Repository  providers.RepositoryRef `json:"repository"`
	ItemID      string                  `json:"itemId"`
	RunID       string                  `json:"runId"`
	Gaggle      string                  `json:"gaggle,omitempty"`
	Stage       string                  `json:"stage,omitempty"`
	RunURL      string                  `json:"runUrl,omitempty"`
	Count       int                     `json:"count,omitempty"`
	Threshold   int                     `json:"threshold,omitempty"`
	Labels      []string                `json:"labels,omitempty"`
	Reset       bool                    `json:"reset,omitempty"`
	RetryAt     time.Time               `json:"retryAt,omitempty"`
	Attempts    int                     `json:"attempts,omitempty"`
}

// Apply publishes the intent through the correctly scoped provider supplied
// by the composition root. Comment markers and additive labels make replay
// safe if a process dies after publication but before acknowledging it.
func (e Effect) Apply(ctx context.Context, poster gate.Commenter) error {
	switch e.Kind {
	case WorkFailure:
		if e.Reset {
			return gate.ResetFailureComment(ctx, poster, e.Repository, e.ItemID, e.RunID, e.RunURL)
		}
		return gate.UpsertFailureComment(ctx, poster, e.Repository, e.ItemID, e.Count, e.Stage, e.RunID, e.RunURL, e.Labels)
	case InfraFailure:
		if e.Reset {
			return gate.ResetInfraFailureComment(ctx, poster, e.Repository, e.ItemID, e.RunID, e.RunURL)
		}
		return gate.UpsertInfraFailureComment(ctx, poster, e.Repository, e.ItemID, e.Count, e.Threshold, e.Stage, e.RunID, e.RunURL, e.Labels)
	default:
		return fmt.Errorf("terminal report: unsupported comment kind %q", e.Kind)
	}
}

// Key returns the outbox key every backend uses for this projection.
func (e Effect) Key() string { return effectKey(e) }

// Identity distinguishes report contents that must keep a frozen body on
// re-admission from a new report that needs a fresh rendering.
func (e Effect) Identity() string { return reportIdentity(e) }
