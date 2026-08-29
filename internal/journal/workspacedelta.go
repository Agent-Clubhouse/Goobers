package journal

// WorkspaceDelta payload keys under Event.Runner for runner.workspace.delta.
// The runner.* map is the only sanctioned runner-specific divergence
// (event.go), so the payload rides it as scalars, exactly as Placement does.
const (
	workspaceDeltaKeyAction          = "action"
	workspaceDeltaKeyProducer        = "producer"
	workspaceDeltaKeyProducerAttempt = "producerAttempt"
	workspaceDeltaKeyDigest          = "digest"
	workspaceDeltaKeyBaseSHA         = "baseSha"
	workspaceDeltaKeyTipSHA          = "tipSha"
)

// WorkspaceDeltaAction names what a runner.workspace.delta event records.
type WorkspaceDeltaAction string

const (
	// WorkspaceDeltaPublished records that the event's stage/attempt bundled base..tip
	// and put it in the blob plane under Digest.
	WorkspaceDeltaPublished WorkspaceDeltaAction = "published"
	// WorkspaceDeltaSelected records that the event's stage (or gate) was handed
	// Producer's Digest to continue from — the continuity selector's choice
	// for this dispatch.
	WorkspaceDeltaSelected WorkspaceDeltaAction = "selected"
	// WorkspaceDeltaUnchanged records that a writable-repo stage succeeded without
	// moving its branch, so nothing was published; the record is unchanged.
	WorkspaceDeltaUnchanged WorkspaceDeltaAction = "unchanged"
)

// WorkspaceDelta is the typed payload of a runner.workspace.delta event
// (#3803/#3767): one movement of the engine's workspace continuity record.
// Digest is the blob-plane content address of the git bundle; BaseSHA/TipSHA
// are the commits it was cut between, which is what a far-side reader
// (worker mirror rev-parse, pod stderr, blob store listing) compares against.
type WorkspaceDelta struct {
	Action WorkspaceDeltaAction `json:"action"`
	// Producer is the stage whose commits the digest carries — the event's
	// own stage on publish, the selected producer on select.
	Producer string `json:"producer,omitempty"`
	// ProducerAttempt is the producer's winning attempt number.
	ProducerAttempt int `json:"producerAttempt,omitempty"`
	// Digest is the bundle's blob-plane address; empty on unchanged.
	Digest  string `json:"digest,omitempty"`
	BaseSHA string `json:"baseSha,omitempty"`
	TipSHA  string `json:"tipSha,omitempty"`
}

// WorkspaceDeltaEvent builds the runner.workspace.delta event for one stage
// (task) or gate. Exactly one of stage/gate is non-empty. attempt is the
// consuming/publishing attempt where known (0 on a selection made before
// the attempt loop begins).
func WorkspaceDeltaEvent(stage, gate string, attempt int, class AttemptClass, d WorkspaceDelta) Event {
	runner := map[string]any{workspaceDeltaKeyAction: string(d.Action)}
	if d.Producer != "" {
		runner[workspaceDeltaKeyProducer] = d.Producer
	}
	if d.ProducerAttempt > 0 {
		runner[workspaceDeltaKeyProducerAttempt] = d.ProducerAttempt
	}
	if d.Digest != "" {
		runner[workspaceDeltaKeyDigest] = d.Digest
	}
	if d.BaseSHA != "" {
		runner[workspaceDeltaKeyBaseSHA] = d.BaseSHA
	}
	if d.TipSHA != "" {
		runner[workspaceDeltaKeyTipSHA] = d.TipSHA
	}
	return Event{
		Type:         EventRunnerWorkspaceDelta,
		Stage:        stage,
		Gate:         gate,
		Attempt:      attempt,
		AttemptClass: class,
		Runner:       runner,
	}
}
