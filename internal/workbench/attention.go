package workbench

import "time"

// NeedsHumanEvidenceRef identifies observed evidence. Inline model prose cannot
// supply the body or human identity of a referenced message/comment/command.
type NeedsHumanEvidenceRef struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

// NeedsHumanComment is bounded source text, never authority to clear a marker.
type NeedsHumanComment struct {
	ID        string     `json:"id"`
	Author    string     `json:"author,omitempty"`
	Text      string     `json:"text"`
	Digest    string     `json:"digest"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// NeedsHumanDependency includes only authorized exact-target native state.
// Unknown or unverified entries keep resolution unavailable.
type NeedsHumanDependency struct {
	ID       string `json:"id"`
	SourceID string `json:"sourceId,omitempty"`
	Revision string `json:"revision,omitempty"`
	Open     bool   `json:"open"`
	Verified bool   `json:"verified"`
}

// NeedsHumanObservation combines provider evidence and the exact learned block
// record. Digest binds all fields except itself. Coverage never implies semantic
// completion; that assessment belongs to the authorized agent.
type NeedsHumanObservation struct {
	Digest               string                 `json:"digest"`
	Item                 BacklogItem            `json:"item"`
	MarkerPresent        bool                   `json:"markerPresent"`
	Comments             []NeedsHumanComment    `json:"comments"`
	Dependencies         []NeedsHumanDependency `json:"dependencies"`
	CommentsComplete     bool                   `json:"commentsComplete"`
	DependenciesComplete bool                   `json:"dependenciesComplete"`
	LearnedRecordDigest  string                 `json:"learnedRecordDigest"`
	LearnedReason        string                 `json:"learnedReason,omitempty"`
	LearnedDependencies  []NeedsHumanDependency `json:"learnedDependencies"`
	LearnedComplete      bool                   `json:"learnedComplete"`
	WaitReasons          []string               `json:"waitReasons"`
}

// NeedsHumanResolutionRequest records an agent's semantic assessment. The host
// checks current source/evidence custody, exact revision, authority and known
// dependency coverage. A current-human-message basis must name the initiating
// human message of this actual turn; a learned-record basis names host evidence.
type NeedsHumanResolutionRequest struct {
	ID                string                  `json:"id"`
	SourceID          string                  `json:"sourceId"`
	ExpectedRevision  string                  `json:"expectedRevision"`
	ObservationDigest string                  `json:"observationDigest"`
	Basis             NeedsHumanEvidenceRef   `json:"basis"`
	Rationale         string                  `json:"rationale"`
	Evidence          []NeedsHumanEvidenceRef `json:"evidence"`
}

// NeedsHumanResolutionReceipt distinguishes a narrow marker effect from broader
// work eligibility. It never represents gate approval, restart or PR publication.
type NeedsHumanResolutionReceipt struct {
	OperationDigest      string       `json:"operationDigest"`
	Outcome              string       `json:"outcome"`
	RevisionSemantics    string       `json:"revisionSemantics"`
	ProviderAcknowledged bool         `json:"providerAcknowledged"`
	ObservedClear        bool         `json:"observedClear"`
	Observed             *BacklogItem `json:"observed,omitempty"`
	WaitReasons          []string     `json:"waitReasons,omitempty"`
}

// NeedsHumanResolutionOrigin is supplied by the trusted session launcher, never
// by an operation request. It retains the actual assessor and initiating human.
type NeedsHumanResolutionOrigin struct {
	RunID         string `json:"runId"`
	SessionID     string `json:"sessionId"`
	TurnID        string `json:"turnId"`
	MessageID     string `json:"messageId"`
	MessageDigest string `json:"messageDigest"`
	GooberDigest  string `json:"gooberDigest"`
}
