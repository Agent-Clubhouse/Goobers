package configsync

import (
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// MutationStatus is how much a failed generation apply is known to have changed.
type MutationStatus string

const (
	// MutationCommitted is a mutation the API server accepted.
	MutationCommitted MutationStatus = "committed"
	// MutationAmbiguous is a mutation whose outcome is unknown — a timeout or a
	// transport failure may or may not have been applied server-side.
	MutationAmbiguous MutationStatus = "ambiguous"
)

// Mutation is one attempted cluster write, recorded so a failed apply can report
// exactly what the cluster may now hold.
type Mutation struct {
	Object    string
	Operation string
	Status    MutationStatus
}

// String renders a mutation for operator-facing output.
func (m Mutation) String() string {
	return fmt.Sprintf("%s %s (%s)", m.Operation, m.Object, m.Status)
}

// ApplyError reports a generation apply that did not complete. The previous
// generation stays authoritative; Mutations names every write that committed or
// whose outcome is ambiguous.
type ApplyError struct {
	Generation string
	Phase      string
	Mutations  []Mutation
	Err        error
}

// Error summarizes the failing phase and the mutations the cluster may hold.
func (e *ApplyError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config generation %s not published (%s phase): %v", e.Generation, e.Phase, e.Err)
	if len(e.Mutations) == 0 {
		return b.String() + "; no mutations committed"
	}
	parts := make([]string, 0, len(e.Mutations))
	for _, m := range e.Mutations {
		parts = append(parts, m.String())
	}
	fmt.Fprintf(&b, "; mutations: %s", strings.Join(parts, ", "))
	return b.String()
}

// Unwrap exposes the underlying cause for errors.Is/As.
func (e *ApplyError) Unwrap() error { return e.Err }

// mutationLedger accumulates the committed and ambiguous writes of one apply.
type mutationLedger struct {
	mutations []Mutation
}

// record notes a write attempt, skipping errors that prove nothing changed.
func (l *mutationLedger) record(object, operation string, err error) {
	status, ok := mutationStatus(err)
	if !ok {
		return
	}
	l.mutations = append(l.mutations, Mutation{Object: object, Operation: operation, Status: status})
}

// fail wraps a phase failure with the ledger so callers can report it.
func (l *mutationLedger) fail(generation, phase string, err error) error {
	return &ApplyError{Generation: generation, Phase: phase, Mutations: l.mutations, Err: err}
}

// mutationStatus classifies a write result. Errors the API server returns before
// admitting a change (conflict, invalid, forbidden, ...) prove nothing committed
// and are not reported; anything else — timeouts, transport failures, server
// errors — leaves the outcome ambiguous.
func mutationStatus(err error) (MutationStatus, bool) {
	if err == nil {
		return MutationCommitted, true
	}
	switch {
	case apierrors.IsConflict(err),
		apierrors.IsInvalid(err),
		apierrors.IsBadRequest(err),
		apierrors.IsForbidden(err),
		apierrors.IsUnauthorized(err),
		apierrors.IsNotFound(err),
		apierrors.IsAlreadyExists(err),
		apierrors.IsMethodNotSupported(err),
		apierrors.IsUnsupportedMediaType(err),
		apierrors.IsRequestEntityTooLargeError(err):
		return "", false
	}
	return MutationAmbiguous, true
}
