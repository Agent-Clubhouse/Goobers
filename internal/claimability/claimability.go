// Package claimability classifies issue candidates already returned by a
// bounded scheduler poll against the authoritative claim sources actual
// admission consults: the local claim ledger (live, expired and legacy
// leases plus the provider-disagreement backoff), learned dependency blocks,
// and, for shared claim visibility, the provider-clocked shared lease.
//
// Observation is strictly read-only: it never acquires, renews or releases a
// claim, never writes blocked records, and never lists provider queues. Its
// result is evidence, not an authorization guarantee: admission runs later
// and may observe a different ledger, blocked record or shared revision.
// Every ambiguous case is Unknown rather than Available, so a verified
// Available count is a lower bound on what admission would have accepted at
// observation time.
package claimability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// Verdict is one candidate's classification.
type Verdict string

// Verdicts. Waiting is intentional deferral (a claim-disagreement backoff),
// distinct from a live lease held by a run.
const (
	Available Verdict = "available"
	Held      Verdict = "held"
	Waiting   Verdict = "waiting"
	Unknown   Verdict = "unknown"
)

// Candidate is one item from an actual scheduler poll. ProviderClaimed means
// the poll saw the provider's claim marker, which only the admission
// transaction's ownership confirmation can settle.
type Candidate struct {
	ID              string
	ProviderClaimed bool
}

// Policy is the admission identity: the claim ledger namespace (empty Gaggle
// addresses legacy unscoped keys, exactly as an ungaggled claim stage does)
// and whether the workflow's claim visibility requires shared admission.
type Policy struct {
	Gaggle   string
	Provider string
	Shared   bool
}

// LocalSnapshot is one read of the claim ledger: current entries and
// retained history.
type LocalSnapshot struct {
	Entries []localscheduler.ClaimEntry
	History []localscheduler.ClaimEntry
}

// BlockedSnapshot is one read of learned dependency blocks for the polled
// repository. Admission re-checks every block against live provider state
// each cycle and clears resolved ones, and migrates Unscoped (legacy) records
// before use, so a recorded block only proves ambiguity, never deferral.
type BlockedSnapshot struct {
	Scoped   map[string]struct{}
	Unscoped map[string]struct{}
}

// Sources are read-only accessors. Shared is opened lazily and only consulted
// under a shared Policy; a nil Shared under a shared Policy is Unknown.
type Sources struct {
	Local   func(context.Context) (LocalSnapshot, error)
	Blocked func(context.Context) (BlockedSnapshot, error)
	Shared  func(context.Context) (sharedclaim.Store, error)
}

// Limits bound one observation. DisagreementWindow is the shortest lease
// admission could use for its disagreement backoff and DisagreementMaxWindow
// the longest (defaulting to DisagreementWindow): a backoff inside the
// shortest is Waiting, one that only the longest still covers is Unknown, so
// an expiring backoff is reported neither Available early nor Waiting late.
type Limits struct {
	MaxCandidates         int
	MaxSharedReads        int
	Timeout               time.Duration
	DisagreementWindow    time.Duration
	DisagreementMaxWindow time.Duration
}

// Result counts verdicts. Observed is the number of candidates classified.
// Complete holds only when the poll's candidate list was not truncated and
// every observed candidate received a definitive verdict.
type Result struct {
	Observed  int
	Available int
	Held      int
	Waiting   int
	Unknown   int
	Complete  bool
}

var errSourceUnavailable = errors.New("claimability source unavailable")

// Observe classifies candidates. now is the worker clock used for local
// leases; shared leases are judged only by the provider clock in each read.
func Observe(ctx context.Context, now time.Time, policy Policy, candidates []Candidate, truncated bool, sources Sources, limits Limits) Result {
	if limits.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limits.Timeout)
		defer cancel()
	}
	if len(candidates) > max(limits.MaxCandidates, 0) {
		candidates, truncated = candidates[:max(limits.MaxCandidates, 0)], true
	}
	o := observer{ctx: ctx, now: now, policy: policy, sources: sources, limits: limits}
	o.local, o.localErr = readSource(ctx, sources.Local)
	o.blocked, o.blockedErr = readSource(ctx, sources.Blocked)
	result := Result{Observed: len(candidates)}
	for _, candidate := range candidates {
		switch o.classify(candidate) {
		case Available:
			result.Available++
		case Held:
			result.Held++
		case Waiting:
			result.Waiting++
		default:
			result.Unknown++
		}
	}
	result.Complete = !truncated && result.Unknown == 0 && ctx.Err() == nil
	return result
}

// readSource runs one accessor but returns no later than ctx's deadline, so an
// accessor that ignores its context cannot stretch the observation bound; its
// late result is discarded and the source is Unknown.
func readSource[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	var zero T
	if read == nil {
		return zero, errSourceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := read(ctx)
		done <- outcome{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case got := <-done:
		if got.err == nil {
			got.err = ctx.Err()
		}
		return got.value, got.err
	}
}

// ErrSourceTooLarge reports a local source file over its byte cap.
var ErrSourceTooLarge = errors.New("claimability source exceeds its size limit")

const readChunkBytes = 64 << 10

// ReadFile reads at most maxBytes from path, checking ctx between chunks. A
// missing file is empty; a larger file is ErrSourceTooLarge, so an oversized
// or slow source makes the observation Unknown instead of unbounded.
func ReadFile(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var buf bytes.Buffer
	limited := io.LimitReader(file, maxBytes+1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, err := io.CopyN(&buf, limited, readChunkBytes)
		if int64(buf.Len()) > maxBytes {
			return nil, fmt.Errorf("%w: %s", ErrSourceTooLarge, path)
		}
		if errors.Is(err, io.EOF) {
			return buf.Bytes(), ctx.Err()
		}
		if err != nil {
			return nil, err
		}
	}
}

type observer struct {
	ctx         context.Context
	now         time.Time
	policy      Policy
	sources     Sources
	limits      Limits
	local       LocalSnapshot
	localErr    error
	blocked     BlockedSnapshot
	blockedErr  error
	store       sharedclaim.Store
	storeOpened bool
	sharedReads int
}

func (o *observer) classify(candidate Candidate) Verdict {
	if candidate.ID == "" || o.ctx.Err() != nil || o.localErr != nil || o.blockedErr != nil {
		return Unknown
	}
	if verdict, decided := o.localLease(candidate.ID); decided {
		return verdict
	}
	if verdict := o.recentDisagreement(candidate.ID); verdict != "" {
		return verdict
	}
	if _, ok := o.blocked.Unscoped[candidate.ID]; ok {
		return Unknown
	}
	if _, ok := o.blocked.Scoped[candidate.ID]; ok {
		return Unknown
	}
	if candidate.ProviderClaimed {
		return Unknown
	}
	if !o.policy.Shared {
		return Available
	}
	return o.sharedLease(candidate.ID)
}

// localLease mirrors ClaimLedger.claim's refusal: a live scoped lease or a
// live legacy item-only lease holds the item. An expired lease is claimable
// to admission but awaits recovery and may be renewed by its live owner, so
// it stays Unknown rather than Available.
func (o *observer) localLease(id string) (Verdict, bool) {
	for _, entry := range o.local.Entries {
		legacy := entry.Gaggle == "" && entry.Provider == ""
		matches := legacy && entry.ItemID == id
		if !legacy && o.policy.Gaggle != "" {
			matches = entry.Gaggle == o.policy.Gaggle && entry.Provider == o.policy.Provider && entry.ExternalID == id
		}
		if !matches {
			continue
		}
		if entry.ExpiresAt.After(o.now) {
			return Held, true
		}
		return Unknown, true
	}
	return "", false
}

// recentDisagreement mirrors the claim stage's provider-disagreement backoff
// (scoped claims only) over the same retained namespace history. It returns
// Waiting inside the shortest admissible window, Unknown inside only the
// longest, and "" when no backoff applies.
func (o *observer) recentDisagreement(id string) Verdict {
	if o.policy.Gaggle == "" {
		return ""
	}
	longest := max(o.limits.DisagreementMaxWindow, o.limits.DisagreementWindow)
	var verdict Verdict
	for _, entry := range o.local.History {
		if entry.ItemID != id && entry.ExternalID != id {
			continue
		}
		legacy := entry.Gaggle == "" && entry.Provider == ""
		if !legacy && (entry.Gaggle != o.policy.Gaggle || entry.Provider != o.policy.Provider) {
			continue
		}
		state := entry.Verification.State
		if state != "contended" && state != "ownership-mismatch" {
			continue
		}
		if entry.Verification.ProviderRunID == "" || entry.Verification.ObservedAt.IsZero() {
			continue
		}
		if o.now.Before(entry.Verification.ObservedAt.Add(o.limits.DisagreementWindow)) {
			return Waiting
		}
		if o.now.Before(entry.Verification.ObservedAt.Add(longest)) {
			verdict = Unknown
		}
	}
	return verdict
}

// sharedLease applies Acquire's ownership check to one provider-clocked read.
// A lease that has expired by the provider clock is Unknown: its owner may
// still renew before a later admission reads the record.
func (o *observer) sharedLease(id string) Verdict {
	if o.sharedReads >= o.limits.MaxSharedReads {
		return Unknown
	}
	if !o.storeOpened {
		o.storeOpened = true
		if o.sources.Shared != nil {
			o.store, _ = o.sources.Shared(o.ctx)
		}
	}
	if o.store == nil {
		return Unknown
	}
	o.sharedReads++
	observed, err := sharedclaim.Inspect(o.ctx, o.store, id)
	if err != nil || o.ctx.Err() != nil {
		return Unknown
	}
	if observed.Held() {
		return Held
	}
	if observed.Record.Owner != (sharedclaim.Owner{}) {
		return Unknown
	}
	return Available
}
