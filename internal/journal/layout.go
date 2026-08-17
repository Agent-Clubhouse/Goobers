package journal

import (
	"os"
	"path/filepath"
)

// On-disk names within a run directory (ARCHITECTURE.md §4). Centralized so the
// writer, reader, and content-address helpers agree on one layout.
const (
	fileRunYAML    = "run.yaml"
	fileState      = "state.json"
	fileEvents     = "events.jsonl"
	fileSchema     = "schema.json"
	fileStateTemp  = "state.json.tmp"
	fileLock       = ".lock"
	fileSchemaLock = ".schema.lock"
	filePruning    = ".telemetry-pruning"

	// fileEventsPointer names the instance journal's generation pointer (see
	// instancegen.go). Run journals have no pointer — a run's events.jsonl
	// never rotates — this is instance-log-only.
	fileEventsPointer = fileEvents + ".current"

	dirInputs    = "inputs"
	dirArtifacts = "artifacts"
	dirSpans     = "spans"
	// dirOutbox is the path-preserving (not content-addressed) export
	// namespace nested under artifacts/ (#1552): runs/<id>/artifacts/outbox/
	// <stage>/attempt-<N>/<relative path>. Unlike the rest of artifacts/,
	// paths under here are not digest-bucketed, so a declared export stays
	// browsable by name.
	dirOutbox = "outbox"
)

// CurrentSchemaVersion is the run-directory schema version written by this
// build. It is separate from the event/run/state envelope versions so the
// directory can evolve without rewriting append-only history.
const CurrentSchemaVersion = 1

// Recorded reports whether dir holds either the current schema marker or the
// legacy identity marker, and so must be admitted or rejected as a journal.
// Directories with neither marker are unpublished residue; callers use this as
// a cheap stat-only precondition to skip work on them.
func Recorded(dir string) bool {
	for _, name := range []string{fileSchema, fileRunYAML} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// Schema identifiers. Each is a versioned URI; the leading path is stable and the
// trailing vN bumps on a breaking change.
const (
	// EventSchema is the schema id stamped on every event envelope.
	EventSchema = "goobers.dev/journal/event/v1"
	// RunSchema is the schema id stamped on run.yaml.
	RunSchema = "goobers.dev/journal/run/v1"
	// StateSchema is the schema id stamped on state.json.
	StateSchema = "goobers.dev/journal/state/v1"
)
