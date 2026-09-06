// Package journalclient is the run-journal READ primitive every
// journal-reading goobers CLI stage calls, behind one interface with two
// backends (decision 005 ruling R1 option 1; finding 002 "JOURNAL READ /
// CARRY-FORWARD", plan step C4):
//
//   - File: instance.Layout + journal.OpenRead — the type-1/type-2 same-host
//     path and the daemon's own readers, byte-for-byte the discipline
//     cmd/goobers used before this package existed.
//   - HTTP: the daemon's run-read routes (GET /runs/{run}/events,
//     /runs/{run}/artifacts/{digest}, /runs/{run}/stages/{stage}/attempts)
//     plus the gaggle-scoped cross-run journal plane
//     (POST /journal/run-phase, /journal/conflict-touches,
//     /journal/unpushed-work), selected when GOOBERS_JOURNAL_ENDPOINT and a
//     journal bearer are present in the stage's environment — the path a
//     stage POD takes, where the pod's filesystem holds no run directory at
//     all.
//
// The boundary decision 005 R1 records: a pod principal may read ITS OWN
// run's scrubbed journal (the daemon's existing read projection, which omits
// journal-relative paths and non-scalar outputs), and nothing else by that
// route. Every read that legitimately crosses runs is a PURPOSE-BUILT
// gaggle-scoped question answered by the daemon — "what phase did run X end
// in", "which runs touched conflicting files since T", "is there stranded
// unpushed work for the items this run claims" — so the daemon decides what
// is exposable rather than handing a pod a general cross-run journal reader.
//
// Fail closed everywhere: a configured endpoint with no bearer, or no run
// identity, is a refusal — never a silent fall-through to a run directory the
// pod does not have. Every backend answers an explicit error rather than an
// empty result: a read that cannot be served must stop its stage, not
// silently change its stage's decision.
package journalclient

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Environment variables that select the HTTP backend in a stage process.
const (
	// EnvEndpoint is the daemon API base URL the run-read routes are reached
	// at. Set by the dispatcher into goobers-CLI stage pods only.
	EnvEndpoint = "GOOBERS_JOURNAL_ENDPOINT"
	// EnvToken is the journal-scoped bearer presented to the read routes.
	EnvToken = "GOOBERS_JOURNAL_TOKEN"
	// EnvRunID is the run the stage acts for — the route's containment key.
	EnvRunID = "GOOBERS_RUN_ID"
	// EnvGaggle is the gaggle the run was admitted into, which the cross-run
	// routes are scoped to.
	EnvGaggle = "GOOBERS_GAGGLE"
)

// ErrEndpointWithoutToken is the fail-closed refusal Select answers when the
// endpoint is configured but no bearer is: a stage in a pod must never fall
// back to a run directory it does not have.
var ErrEndpointWithoutToken = errors.New("journalclient: " + EnvEndpoint + " is set but " + EnvToken + " is empty; refusing to fall back to an on-disk run journal")

// ErrEndpointWithoutRun is the sibling refusal for a missing run identity.
var ErrEndpointWithoutRun = errors.New("journalclient: " + EnvEndpoint + " is set but " + EnvRunID + " is empty; the run-read routes contain every call to the stage's own run")

// ErrCrossRunUnavailable reports a cross-run question asked of a backend that
// cannot answer it — the explicit refusal that replaces the silent zero the
// pre-route code degraded to.
var ErrCrossRunUnavailable = errors.New("journalclient: cross-run journal reads are not available from this backend")

// Reader is the same-run read surface: journal.Reader's shape, narrowed to
// what CLI stages actually call. Implemented by File over a run directory and
// by HTTP over the daemon's run-scoped read routes.
//
// Deliberately NOT journal.Reader itself: the plane serves the daemon's
// scrubbed projection, so a Ref returned here carries a digest and the
// canonical journal-relative path derived from it, never a writer-chosen one,
// and Events omits non-scalar stage outputs exactly as the portal's read of
// the same run does.
type Reader interface {
	// RunID is the run this reader is bound to.
	RunID() string
	// Events returns the run's durable events in seq order.
	Events() ([]journal.Event, error)
	// ArtifactBytes returns the artifact ref addresses, verified against
	// ref.Digest.
	ArtifactBytes(ref journal.Ref) ([]byte, error)
	// ArtifactBytesBounded is ArtifactBytes with an explicit ceiling: content
	// over maxBytes is refused rather than buffered, so a caller that will
	// only act on a bounded artifact cannot be made to hold an unbounded one.
	// maxBytes <= 0 is unbounded. It is on the interface rather than beside
	// it so a backend cannot be added that has no way to be bounded.
	ArtifactBytesBounded(ref journal.Ref, maxBytes int64) ([]byte, error)
	// ArtifactByDigest returns the artifact stored at digest, verified.
	ArtifactByDigest(digest string) ([]byte, error)
	// StageAttempts returns every durable traversal of stage.
	StageAttempts(stage string) ([]StageAttempt, error)
	// Phase reconstructs the run's phase from its events.
	Phase() (journal.RunPhase, error)
}

// StageAttempt is one durable stage traversal, restated from the daemon's
// read projection (readservice.StageAttempt) with the fields a stage-side
// consumer can act on. The server's tests pin the two shapes together.
type StageAttempt struct {
	ID             string               `json:"id"`
	Visit          int                  `json:"visit"`
	Number         int                  `json:"number"`
	Class          string               `json:"class"`
	Status         string               `json:"status"`
	StartedSeq     uint64               `json:"startedSeq,omitempty"`
	FinishedSeq    uint64               `json:"finishedSeq,omitempty"`
	StartedAt      *time.Time           `json:"startedAt,omitempty"`
	FinishedAt     *time.Time           `json:"finishedAt,omitempty"`
	DurationMillis int64                `json:"durationMillis"`
	Outputs        map[string]any       `json:"outputs,omitempty"`
	Artifacts      []ArtifactMetadata   `json:"artifacts"`
	Error          *journal.ErrorDetail `json:"error,omitempty"`
}

// ArtifactMetadata addresses one artifact by digest. It carries no
// journal-relative path by design — the same omission the daemon's read
// projection makes — so a stage cannot steer a read with a path it chose.
type ArtifactMetadata struct {
	Name         string `json:"name,omitempty"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
	MediaType    string `json:"mediaType"`
	Stage        string `json:"stage,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	AttemptClass string `json:"attemptClass,omitempty"`
	RecordedSeq  uint64 `json:"recordedSeq,omitempty"`
}

// Ref is the journal.Ref that addresses metadata's content: the canonical
// content-addressed path derived from the digest, never a path the journal
// carried. An unparseable digest yields a zero Ref and false.
func (m ArtifactMetadata) Ref() (journal.Ref, bool) {
	path, err := journal.ArtifactPath(m.Digest)
	if err != nil {
		return journal.Ref{}, false
	}
	return journal.Ref{Path: path, Digest: m.Digest, Size: m.Size, MediaType: m.MediaType}, true
}

// CrossRun is the gaggle-scoped cross-run read surface. Each method is one
// question the daemon knows how to answer safely, not a general reader:
// callers get derived facts about other runs in their own gaggle, never those
// runs' journals.
type CrossRun interface {
	// RunPhase returns the phase run targetRunID ended in.
	// backlog-query --claim's terminalFailureStreak is the caller: it walks an
	// item's released claim history and counts consecutive terminal failures.
	RunPhase(ctx context.Context, targetRunID string) (journal.RunPhase, error)
	// ConflictTouches returns, per run, the files a base-sync conflict
	// artifact recorded since the request's cutoff — gather-implement-context's
	// hot-file history.
	ConflictTouches(ctx context.Context, req ConflictTouchRequest) ([]ConflictTouch, error)
	// UnpushedWork returns the newest committed-but-never-published diff a
	// prior run recorded for one of the requested items, or nil when there is
	// none (#3366).
	UnpushedWork(ctx context.Context, req UnpushedWorkRequest) (*UnpushedWork, error)
	// EscalationCandidates returns gaggle's outstanding decomposition
	// escalation candidates — decomposition.FindEscalationCandidates's own
	// scan, run where the runs tree actually lives (this instance for File,
	// the daemon for HTTP) instead of exposed to a pod as raw run-directory
	// traversal (#4342).
	EscalationCandidates(ctx context.Context, req EscalationCandidatesRequest) ([]EscalationCandidate, error)
	// BranchOwnership answers whether TargetRunID's journal actually owns
	// Branch, and if so its identity and terminal/ref facts (#4344).
	BranchOwnership(ctx context.Context, req BranchOwnershipRequest) (BranchOwnershipResponse, error)
}

// ConflictTouchRequest asks for base-sync conflict history in one gaggle.
type ConflictTouchRequest struct {
	// RunID is the asking run — the plane's containment key.
	RunID string `json:"runId"`
	// Gaggle scopes the walk. Empty means the instance's unscoped runs root,
	// which only a same-host File backend can serve; the plane requires one.
	Gaggle string `json:"gaggle,omitempty"`
	// Since bounds the walk: conflict artifacts recorded before it are ignored.
	Since time.Time `json:"since"`
}

// ConflictTouch is one prior run's conflicting-file set.
type ConflictTouch struct {
	RunID string   `json:"runId"`
	Files []string `json:"files"`
}

// ConflictTouchResponse is the plane's answer.
type ConflictTouchResponse struct {
	Touches []ConflictTouch `json:"touches"`
}

// UnpushedWorkRequest asks for a prior run's stranded diff.
type UnpushedWorkRequest struct {
	// RunID is the asking run: the containment key, and the run excluded from
	// the search (its own in-flight work is not "prior").
	RunID string `json:"runId"`
	// Gaggle scopes the walk, as for ConflictTouchRequest.
	Gaggle string `json:"gaggle,omitempty"`
	// Since bounds which recorded diffs are still considered.
	Since time.Time `json:"since"`
	// ItemIDs are the backlog items the asking run holds. The File backend
	// treats this list as authoritative — its caller read the ledger. The
	// daemon DELIBERATELY ignores it and derives the asking run's claimed
	// items from its own ledger instead: a pod must not be able to ask about
	// an item it does not hold, and the daemon is the only party that can
	// tell. It is still sent so a plane refusal names the same question the
	// stage asked.
	ItemIDs []string `json:"itemIds,omitempty"`
	// MaxInlineDiffBytes bounds the diff carried inline in the answer. Zero
	// uses DefaultMaxInlineDiffBytes.
	MaxInlineDiffBytes int `json:"maxInlineDiffBytes,omitempty"`
}

// UnpushedWork is a prior run's committed-but-never-published diff.
type UnpushedWork struct {
	RunID         string    `json:"runId"`
	Stage         string    `json:"stage"`
	Attempt       int       `json:"attempt"`
	RecordedAt    time.Time `json:"recordedAt"`
	Branch        string    `json:"branch,omitempty"`
	BaseRef       string    `json:"baseRef,omitempty"`
	ItemIDs       []string  `json:"itemIds,omitempty"`
	DiffBytes     int       `json:"diffBytes"`
	DiffDigest    string    `json:"diffDigest,omitempty"`
	Diff          string    `json:"diff,omitempty"`
	DiffTruncated bool      `json:"diffTruncated,omitempty"`
}

// UnpushedWorkResponse is the plane's answer; Work is nil when none exists.
type UnpushedWorkResponse struct {
	Work *UnpushedWork `json:"work,omitempty"`
}

// EscalationCandidatesRequest asks for gaggle's outstanding decomposition
// escalation candidates (#4342).
type EscalationCandidatesRequest struct {
	// RunID is the asking run — the plane's containment key.
	RunID string `json:"runId"`
	// Gaggle scopes the walk, as for ConflictTouchRequest.
	Gaggle string `json:"gaggle,omitempty"`
}

// EscalationCandidate is decomposition.EscalationCandidate carried over the
// wire — the same fields, JSON-tagged for transport. Kept as a distinct type
// rather than adding tags to decomposition.EscalationCandidate directly:
// internal/decomposition has no wire-format concerns of its own, and this
// package is the one boundary that does.
type EscalationCandidate struct {
	SourceRunID    string    `json:"sourceRunId"`
	SourceWorkflow string    `json:"sourceWorkflow"`
	SourceStage    string    `json:"sourceStage"`
	ErrorCode      string    `json:"errorCode"`
	ErrorMessage   string    `json:"errorMessage,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	ParentProvider string    `json:"parentProvider"`
	ParentID       string    `json:"parentId"`
}

// EscalationCandidatesResponse is the plane's answer.
type EscalationCandidatesResponse struct {
	Candidates []EscalationCandidate `json:"candidates"`
}

// RunPhaseRequest asks for one prior run's terminal phase.
type RunPhaseRequest struct {
	// RunID is the asking run — the plane's containment key.
	RunID string `json:"runId"`
	// TargetRunID is the run whose phase is being asked for. It must belong to
	// the same gaggle as RunID.
	TargetRunID string `json:"targetRunId"`
	// Gaggle scopes the lookup.
	Gaggle string `json:"gaggle,omitempty"`
}

// RunPhaseResponse carries only the phase — nothing else about the target run
// crosses the boundary.
type RunPhaseResponse struct {
	RunID string `json:"runId"`
	Phase string `json:"phase"`
}

// BranchOwnershipRequest asks whether TargetRunID's journal actually owns
// Branch — reconcile-branches's own check before a candidate branch is
// preserved or deleted (#4344).
type BranchOwnershipRequest struct {
	// RunID is the asking run — the plane's containment key.
	RunID string `json:"runId"`
	// Gaggle scopes the lookup, as for RunPhaseRequest.
	Gaggle string `json:"gaggle,omitempty"`
	// TargetRunID is the run id parsed out of the branch name
	// (<prefix><workflow>/<run-id>). It must belong to the same gaggle as
	// RunID.
	TargetRunID string `json:"targetRunId"`
	// Workflow is the workflow name parsed out of the branch name; it must
	// match TargetRunID's own recorded workflow, or ownership is refused.
	Workflow string `json:"workflow"`
	// Branch is the full branch name TargetRunID must have journaled a
	// ref.touched event naming, or ownership is refused.
	Branch string `json:"branch"`
}

// BranchOwnership is the owning run's identity and terminal/ref facts —
// nothing else about the run crosses the boundary.
type BranchOwnership struct {
	Workflow   string    `json:"workflow"`
	RunID      string    `json:"runId"`
	StartedAt  time.Time `json:"startedAt"`
	TerminalAt time.Time `json:"terminalAt,omitempty"`
	Phase      string    `json:"phase"`
}

// BranchOwnershipResponse is the plane's answer. Owner is nil when ownership
// could not be established; Reason then names why
// ("ambiguous-ownership"/"run-journal-unreadable"), matching
// reconcile-branches's own pre-existing outcome vocabulary so callers keep
// reporting the same reasons regardless of backend. Detail is the
// human-readable explanation reconcile-branches journals alongside the
// reason; it is data, not a Go error — an unresolved ownership question is
// reconcile-branches's ordinary "preserve this branch" outcome, not a
// request failure, so it must reach both backends as a 200 answer rather
// than surface as an HTTP error status.
type BranchOwnershipResponse struct {
	Owner  *BranchOwnership `json:"owner,omitempty"`
	Reason string           `json:"reason,omitempty"`
	Detail string           `json:"detail,omitempty"`
}

// DefaultMaxInlineDiffBytes bounds an inline stranded diff when a caller
// names no bound. It mirrors gather-implement-context's own ceiling; the full
// diff always stays addressable by the digest the answer names.
const DefaultMaxInlineDiffBytes = 200_000

// Selection reports which backend a stage should use, and why. Endpoint is
// empty when the stage is on the file path.
type Selection struct {
	Endpoint string
	Token    string
	RunID    string
	Gaggle   string
}

// OnPlane reports whether the HTTP backend was selected.
func (s Selection) OnPlane() bool { return s.Endpoint != "" }

// Select resolves the backend from a stage's environment, failing closed.
//
// It returns a zero Selection when no endpoint is configured (the file path),
// a populated one when the plane is fully configured, and an error when the
// endpoint is set but the bearer or the run identity is not — never a silent
// fall-through to a run directory the pod does not have.
func Select(getenv func(string) string) (Selection, error) {
	endpoint := strings.TrimSpace(getenv(EnvEndpoint))
	if endpoint == "" {
		return Selection{}, nil
	}
	token := strings.TrimSpace(getenv(EnvToken))
	if token == "" {
		return Selection{}, ErrEndpointWithoutToken
	}
	runID := strings.TrimSpace(getenv(EnvRunID))
	if runID == "" {
		return Selection{}, ErrEndpointWithoutRun
	}
	return Selection{
		Endpoint: endpoint,
		Token:    token,
		RunID:    runID,
		Gaggle:   strings.TrimSpace(getenv(EnvGaggle)),
	}, nil
}
