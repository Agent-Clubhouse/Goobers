package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/goobers/goobers/providers"
)

// mutationsSidecarFile is the well-known, worktree-relative file a
// provider-chain subcommand (backlog-query/open-pr/issue-close-out) records
// its mutation facts to, for the runner to project into ref.touched or error
// journal events once the stage finishes (issue #228). These subcommands run as
// separate short-lived processes with no legal journal access — only the
// parent runner process holds the run journal under its single-writer,
// monotonic-seq, fsync-per-record contract — so a sidecar in the stage
// worktree (the one thing the subcommand and the runner both touch) is the
// only legal handoff; a MutationRecorder wired directly into the subprocess
// would have nowhere legal to write.
const mutationsSidecarFile = "mutations.jsonl"

// mutationFact is one line of mutationsSidecarFile — just enough to build a
// journal.ExternalRef (Provider/Kind/ID/URL), operation, and claim outcome.
// RunID identifies the claim owner, which can differ from the stage's run
// during reconciliation. Provider Fields digests are not part of this handoff.
type mutationFact struct {
	QueueAdmission    *providers.QueueAdmission    `json:"queueAdmission,omitempty"`
	MergeConfirmation *providers.MergeConfirmation `json:"mergeConfirmation,omitempty"`
	Provider          string                       `json:"provider"`
	Kind              string                       `json:"kind"`
	ID                string                       `json:"id"`
	URL               string                       `json:"url,omitempty"`
	Operation         string                       `json:"operation,omitempty"`
	RunID             string                       `json:"runId,omitempty"`
	Outcome           string                       `json:"outcome,omitempty"`
	ErrorCode         string                       `json:"errorCode,omitempty"`
	ProviderRunID     string                       `json:"providerRunId,omitempty"`
}

// sidecarMutationRecorder implements providers.MutationRecorder by appending
// each recorded mutation as one JSON line to mutationsSidecarFile in the
// process's current directory — the stage worktree, per providercmd.go's own
// doc on how these subcommands are invoked. kind is fixed per subcommand
// (constructed once, at provider-construction time) rather than parsed from
// providers.ExternalRef, since GitHub's REST API treats issues and PRs as the
// same underlying entity — nothing in ExternalRef itself says which one this
// mutation touched, but each subcommand unambiguously knows: open-pr only
// ever mutates PRs, backlog-query/issue-close-out only ever mutate issues.
type sidecarMutationRecorder struct {
	kind string
}

// RecordExternalRef appends fact best-effort: a malformed record or a failed
// write must never fail the mutation the provider already made for real —
// the sidecar is provenance, not the mutation itself. A failure is still
// surfaced via a log line (#2029): this subcommand runs as a short-lived
// subprocess with no legal journal access (see mutationsSidecarFile's doc),
// so stderr — captured by the parent runner process — is the only
// observability channel available here. The runner's own read side
// (internal/runner/run.go's readMutationSidecar) separately distinguishes a
// present-but-corrupt sidecar from the overwhelmingly common no-mutations
// case and emits its own journal-level signal for that.
func (r sidecarMutationRecorder) RecordExternalRef(_ context.Context, ref providers.ExternalRef) {
	fact := mutationFact{
		QueueAdmission:    ref.QueueAdmission,
		MergeConfirmation: ref.MergeConfirmation,
		Provider:          string(ref.Provider),
		Kind:              r.kind,
		ID:                externalRefID(ref.Ref),
		URL:               ref.URL,
		Operation:         ref.Operation,
		RunID:             ref.RunID, Outcome: ref.Outcome, ErrorCode: ref.ErrorCode,
		ProviderRunID: ref.ProviderRunID,
	}
	data, err := json.Marshal(fact)
	if err != nil {
		log.Printf("mutation sidecar: marshal %s %s %s: %v", r.kind, fact.Provider, fact.ID, err)
		return
	}
	f, err := os.OpenFile(mutationsSidecarFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("mutation sidecar: open %s: %v", mutationsSidecarFile, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("mutation sidecar: write %s: %v", mutationsSidecarFile, err)
	}
}

// externalRefID extracts the bare identifier from a providers.ExternalRef.Ref
// string ("owner/name#123" for an issue/PR, "owner/name@branch" for a
// branch), or returns it unchanged if neither separator is present.
func externalRefID(ref string) string {
	if i := strings.LastIndexAny(ref, "#@"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
