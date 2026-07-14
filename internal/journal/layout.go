package journal

// On-disk names within a run directory (ARCHITECTURE.md §4). Centralized so the
// writer, reader, and content-address helpers agree on one layout.
const (
	fileRunYAML   = "run.yaml"
	fileState     = "state.json"
	fileEvents    = "events.jsonl"
	fileStateTemp = "state.json.tmp"
	fileLock      = ".lock"

	dirInputs    = "inputs"
	dirArtifacts = "artifacts"
	dirSpans     = "spans"
)

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
