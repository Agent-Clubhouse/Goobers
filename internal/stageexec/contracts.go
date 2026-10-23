// Package stageexec defines the per-run executor construction contracts shared
// by the local runner and remote worker. It contains no workflow driver.
package stageexec

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// ArtifactRecorder persists stage output bytes into the run journal by content
// digest. Mirrors internal/executor.ArtifactRecorder and internal/gate's
// Journal interface — *journal.Run satisfies all three structurally (same
// RecordArtifact method), so the runner hands its run's own journal.Run
// straight through with no adapter.
type ArtifactRecorder interface {
	RecordArtifact(name string, data []byte) (journal.Ref, error)
}

// SecretRegistrar receives every secret a run's executors resolve, so the
// same run's journal scrubber (registered from the identical instance — see
// Start) can redact it from anything written to the journal at rest. This is
// defense-in-depth alongside each executor's own credential-issuing
// redaction (#14, #66): an executor already scrubs its own artifact/result
// content before it reaches the journal, but a runner-authored event (e.g. an
// executor_error message that happens to echo a credential) only ever passes
// through the run's own journal.Scrubber — which is a pattern-net-only
// fallback unless it is chained with the exact-value registry this interface
// feeds. Satisfied directly by journal.DefaultScrubber()'s
// *journal.RegistryScrubber, and by internal/credentials.SecretRegistrar
// callers (identical method shape) — no adapter needed either direction.
type SecretRegistrar interface {
	Register(secret []byte)
}

// NewDeterministicFunc constructs the deterministic-task executor for one run,
// bound to rec so its captured output/result-file artifacts land in that
// run's journal (e.g. internal/executor.NewShellExecutor(injector, rec)), and
// to reg so every credential its internal *credentials.Injector resolves is
// also registered with this run's journal scrubber (see SecretRegistrar).
// Required if the workflow has any deterministic task.
type NewDeterministicFunc func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error)

// NewAgenticFunc constructs the agentic executor for one named goober, bound
// to rec and reg (see NewDeterministicFunc for both). Keyed by goober name
// since a single Runner/run can target more than one distinct goober (e.g.
// "coder" for a task, "reviewer" for its gate); the caller's closure resolves
// that name to its instructions/model/harness config. Required if the
// workflow has any agentic task or agentic gate. The same constructed
// invoke.Goober serves both a Task.Goober's Invoke and a paired AgenticGate's
// Review — one instance, two methods.
type NewAgenticFunc func(gooberName string, rec ArtifactRecorder, reg SecretRegistrar) (invoke.Goober, error)

// NewAgenticWithMCPServersFunc receives the compiler-materialized MCP
// declarations for this run's pinned workflow definition.
type NewAgenticWithMCPServersFunc func(gooberName string, servers []apiv1.MCPServer, rec ArtifactRecorder, reg SecretRegistrar) (invoke.Goober, error)
