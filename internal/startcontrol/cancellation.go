package startcontrol

// CancellationState distinguishes request custody from observed execution end.
type CancellationState string

const (
	// CancellationRequested carries no proof of termination or absence.
	CancellationRequested CancellationState = "requested"
	// CancellationConfirmed requires joined or terminal cancellation evidence.
	CancellationConfirmed CancellationState = "confirmed"
	// CancellationAlreadyTerminal proves an earlier terminal result, not a stop.
	CancellationAlreadyTerminal CancellationState = "already-terminal"
)

// CancellationObservation is supplied only by a qualified host source adapter.
type CancellationObservation struct{ State CancellationState }
