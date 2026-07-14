package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/goobers/goobers/providers"
)

// mutationsSidecarFile is the well-known, worktree-relative file a
// provider-chain subcommand (backlog-query/open-pr/issue-close-out) records
// its mutation facts to, for the runner to project into ref.touched journal
// events once the stage finishes (issue #228). These subcommands run as
// separate short-lived processes with no legal journal access — only the
// parent runner process holds the run journal under its single-writer,
// monotonic-seq, fsync-per-record contract — so a sidecar in the stage
// worktree (the one thing the subcommand and the runner both touch) is the
// only legal handoff; a MutationRecorder wired directly into the subprocess
// would have nowhere legal to write.
const mutationsSidecarFile = "mutations.jsonl"

// mutationFact is one line of mutationsSidecarFile — just enough to build a
// journal.ExternalRef (Provider/Kind/ID/URL) plus an operation annotation,
// not the richer providers.ExternalRef shape (Fields digests, RunID) that
// exists for a different purpose (provider_mutations' own telemetry
// richness) and isn't needed for this projection.
type mutationFact struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Operation string `json:"operation,omitempty"`
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
// the sidecar is provenance, not the mutation itself.
func (r sidecarMutationRecorder) RecordExternalRef(_ context.Context, ref providers.ExternalRef) {
	fact := mutationFact{
		Provider:  string(ref.Provider),
		Kind:      r.kind,
		ID:        externalRefID(ref.Ref),
		URL:       ref.URL,
		Operation: ref.Operation,
	}
	data, err := json.Marshal(fact)
	if err != nil {
		return
	}
	f, err := os.OpenFile(mutationsSidecarFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(data, '\n'))
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
