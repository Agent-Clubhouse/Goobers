package journal

import (
	"os"
	"path/filepath"
)

// On-disk names within a run directory (ARCHITECTURE.md §4). Centralized so the
// writer, reader, and content-address helpers agree on one layout.
const (
	fileRunYAML   = "run.yaml"
	fileState     = "state.json"
	fileEvents    = "events.jsonl"
	fileStateTemp = "state.json.tmp"
	fileLock      = ".lock"
	filePruning   = ".telemetry-pruning"

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

// Recorded reports whether dir holds a run journal's identity file, and so can
// be read or ingested at all. A run directory without one is not a journal: the
// span exporter creates spans/ before run.yaml is published, and a run that
// never publishes leaves the directory behind permanently. Such directories
// accumulate in the thousands on a long-lived instance, so callers use this as a
// cheap stat-only precondition to skip work — notably lock acquisition — that
// could only fail on them.
func Recorded(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, fileRunYAML))
	return err == nil && info.Mode().IsRegular()
}

// Schema identifiers. Each is a versioned URI; the leading path is stable and the
// trailing vN bumps on a breaking change. Readers use the version to apply
// forward-compat policy (see reader.go).
const (
	// EventSchema is the schema id stamped on every event envelope.
	EventSchema = "goobers.dev/journal/event/v1"
	// RunSchema is the schema id stamped on run.yaml.
	RunSchema = "goobers.dev/journal/run/v1"
	// StateSchema is the schema id stamped on state.json.
	StateSchema = "goobers.dev/journal/state/v1"
)
