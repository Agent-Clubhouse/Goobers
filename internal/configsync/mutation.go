package configsync

import (
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Phases of the generation barrier, reported by ApplyError so a caller knows
// whether the new generation ever became authoritative.
const (
	PhasePrepare  = "prepare"
	PhaseApply    = "apply"
	PhaseValidate = "validate"
	PhaseSwitch   = "switch"
	PhasePrune    = "prune"
)

// Cluster operations a mutation can name.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpDelete = "delete"
	// OpNone means a step failed before issuing any write (e.g. the read that
	// precedes an upsert failed), so there is no mutation to report.
	OpNone = ""
)

// MutationState says whether a cluster mutation is known to have landed.
type MutationState string

const (
	// MutationCommitted mutations definitely landed in the cluster.
	MutationCommitted MutationState = "committed"
	// MutationAmbiguous mutations may or may not have landed: the request failed
	// in a way that does not prove the server rejected it.
	MutationAmbiguous MutationState = "ambiguous"
	// MutationRejected mutations definitely did not land.
	MutationRejected MutationState = "rejected"
)

// Mutation is one attempted cluster write during an apply.
type Mutation struct {
	Op    string
	Key   string
	State MutationState
}

// String renders a mutation as "op object (state)".
func (m Mutation) String() string {
	return fmt.Sprintf("%s %s (%s)", m.Op, m.Key, m.State)
}

// ApplyError reports a failed direct apply, naming the generation involved, the
// generation left authoritative, and every mutation the attempt made.
type ApplyError struct {
	// Phase is the barrier phase that failed.
	Phase string
	// Generation is the generation being published.
	Generation string
	// Authoritative is the generation consumers still select; it is the previous
	// generation unless the authoritative switch already succeeded.
	Authoritative string
	// Mutations are every write attempted, in order.
	Mutations []Mutation
	// Err is the underlying failure.
	Err error
}

// Error reports the failure together with every committed or ambiguous mutation.
func (e *ApplyError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config-sync %s generation %s: %v", e.Phase, e.Generation, e.Err)
	authoritative := e.Authoritative
	if authoritative == "" {
		authoritative = "none"
	}
	fmt.Fprintf(&b, "; authoritative generation %s", authoritative)
	reported := append(e.Committed(), e.Ambiguous()...)
	if len(reported) == 0 {
		b.WriteString("; no committed or ambiguous mutations")
		return b.String()
	}
	parts := make([]string, 0, len(reported))
	for _, m := range reported {
		parts = append(parts, m.String())
	}
	fmt.Fprintf(&b, "; mutations: %s", strings.Join(parts, ", "))
	return b.String()
}

// Unwrap exposes the underlying failure.
func (e *ApplyError) Unwrap() error { return e.Err }

// Committed returns the mutations known to have landed.
func (e *ApplyError) Committed() []Mutation { return e.byState(MutationCommitted) }

// Ambiguous returns the mutations whose outcome is unknown.
func (e *ApplyError) Ambiguous() []Mutation { return e.byState(MutationAmbiguous) }

func (e *ApplyError) byState(state MutationState) []Mutation {
	var out []Mutation
	for _, m := range e.Mutations {
		if m.State == state {
			out = append(out, m)
		}
	}
	return out
}

// appendMutation records an attempted write. OpNone attempts are dropped: no
// write was issued, so there is nothing to report as committed or ambiguous.
func appendMutation(mutations []Mutation, op, key string, state MutationState) []Mutation {
	if op == OpNone {
		return mutations
	}
	return append(mutations, Mutation{Op: op, Key: key, State: state})
}

// mutationState classifies a failed write. Only errors that prove the server
// refused the request count as rejected; everything else (timeouts, transport
// failures, canceled contexts) is ambiguous and must be reported as such.
func mutationState(err error) MutationState {
	switch {
	case err == nil:
		return MutationCommitted
	case apierrors.IsAlreadyExists(err),
		apierrors.IsConflict(err),
		apierrors.IsInvalid(err),
		apierrors.IsForbidden(err),
		apierrors.IsUnauthorized(err),
		apierrors.IsNotFound(err),
		apierrors.IsBadRequest(err),
		apierrors.IsMethodNotSupported(err),
		apierrors.IsRequestEntityTooLargeError(err):
		return MutationRejected
	default:
		return MutationAmbiguous
	}
}
