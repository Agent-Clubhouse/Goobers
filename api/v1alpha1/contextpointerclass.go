package v1alpha1

import "strings"

// ContextPointerClass is the naming CONTRACT a context pointer was stamped
// with by whatever produced it. It exists so consumers that have to reason
// about a pointer's origin — contextFrom selection, above all — switch on a
// declared class instead of re-deriving one from ad-hoc string prefixes at
// each call site.
//
// Pointer names are stamped by the RUNNERS, never by an agent: a stage's own
// artifacts are named "<stage>.artifact[<i>]" by the runner as it records
// them (internal/runner/run.go, internal/engine/engine.go), a gate's verdict
// "<gate>.verdict" by the gate arm, and an injected learning episode
// "learning.episode[<seq>]" by the repass arm. Classification is therefore a
// statement about which runner code path produced a pointer, not a guess about
// untrusted content.
//
// The classes divide on the question contextFrom actually asks — "which
// upstream PRODUCER does this pointer come from?":
//
//   - SOURCE-SCOPED pointers name their producing workflow state in the
//     pointer name itself, so contextFrom can select them by that name.
//   - SYSTEM-GENERATED pointers have no producing workflow state. They are
//     minted by the run about the run and addressed to one specific stage.
//     No source name a workflow author could write would ever select them, so
//     a source filter has no jurisdiction over them.
//
// It is a named string rather than an enum so that the zero value is the
// fail-closed one and every diagnostic that prints a class is readable without
// a String method the production paths would never call.
//
// +kubebuilder:object:generate=false
type ContextPointerClass string

const (
	// ContextPointerUnclassified is a pointer whose name matches no declared
	// naming contract — including a MALFORMED name that resembles one. It is
	// the zero value and the fail-closed default: an unclassified pointer is
	// selected by nothing, so a name that merely looks like a system-generated
	// pointer does not inherit a system-generated pointer's exemption.
	ContextPointerUnclassified ContextPointerClass = ""
	// ContextPointerStageArtifact is "<stage>.artifact[<i>]": one artifact a
	// completed stage recorded. Source-scoped on <stage>.
	ContextPointerStageArtifact ContextPointerClass = "stage-artifact"
	// ContextPointerGateVerdict is "<gate>.verdict": the verdict artifact a
	// gate produced. Source-scoped on <gate>.
	ContextPointerGateVerdict ContextPointerClass = "gate-verdict"
	// ContextPointerLearningEpisode is "learning.episode[<seq>]": the
	// correction feedback a repassing gate injects into the stage it re-enters
	// (#3843/#3913), named after the JOURNAL SEQUENCE of the event it corrects
	// rather than after any workflow state. System-generated.
	ContextPointerLearningEpisode ContextPointerClass = "learning-episode"
)

// SourceScoped reports whether the class names a producing workflow state that
// contextFrom selects it by.
func (c ContextPointerClass) SourceScoped() bool {
	switch c {
	case ContextPointerStageArtifact, ContextPointerGateVerdict:
		return true
	default:
		return false
	}
}

// SystemGenerated reports whether the class is minted by the run itself rather
// than by a workflow state, and therefore has no source for contextFrom to
// scope it by.
//
// This is deliberately NOT "everything that is not source-scoped":
// ContextPointerUnclassified is neither, so a malformed or unrecognized name
// gets neither a source match nor an exemption.
func (c ContextPointerClass) SystemGenerated() bool {
	return c == ContextPointerLearningEpisode
}

const (
	gateVerdictPointerSuffix     = ".verdict"
	stageArtifactPointerInfix    = ".artifact["
	learningEpisodePointerPrefix = "learning.episode["
	learningEpisodePointerSuffix = "]"
)

// ClassifyContextPointer reports a pointer name's naming contract and, for a
// source-scoped class, the producing workflow state its name carries. The
// source is empty for every other class.
//
// The stage-artifact arm splits on the FIRST ".artifact[" and the gate-verdict
// arm on the LAST ".verdict", which reproduces exactly the prefix/equality
// tests SelectContextPointers used before classification existed — this
// function must not quietly widen or narrow what contextFrom selects.
func ClassifyContextPointer(name string) (class ContextPointerClass, source string) {
	if isLearningEpisodePointerName(name) {
		return ContextPointerLearningEpisode, ""
	}
	if rest, ok := strings.CutSuffix(name, gateVerdictPointerSuffix); ok {
		return ContextPointerGateVerdict, rest
	}
	if i := strings.Index(name, stageArtifactPointerInfix); i >= 0 {
		return ContextPointerStageArtifact, name[:i]
	}
	return ContextPointerUnclassified, ""
}

// isLearningEpisodePointerName reports whether name is a WELL-FORMED injected
// episode pointer: the exact prefix, a non-empty run of ASCII digits (the
// corrected event's journal sequence, a uint64), and the closing bracket.
//
// Strictness is the point. "learning.episode[abc]", "learning.episode[]",
// "learning.episode[3" and "learning.episodes[3]" all resemble the contract
// without honouring it; each classifies unclassified and is dropped by a
// contextFrom filter exactly as it is today. Only the shape
// LearningEpisodePointerName actually emits is exempt from source scoping.
func isLearningEpisodePointerName(name string) bool {
	seq, ok := strings.CutPrefix(name, learningEpisodePointerPrefix)
	if !ok {
		return false
	}
	seq, ok = strings.CutSuffix(seq, learningEpisodePointerSuffix)
	if !ok || seq == "" {
		return false
	}
	for i := 0; i < len(seq); i++ {
		if seq[i] < '0' || seq[i] > '9' {
			return false
		}
	}
	return true
}
