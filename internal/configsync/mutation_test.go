package configsync

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestMutationState(t *testing.T) {
	gk := schema.GroupKind{Group: "goobers.dev", Kind: "Gaggle"}
	cases := []struct {
		name string
		err  error
		want MutationState
	}{
		{"nil", nil, MutationCommitted},
		{"timeout", apierrors.NewTimeoutError("timeout", 1), MutationAmbiguous},
		{"transport", errors.New("connection reset"), MutationAmbiguous},
		{"conflict", apierrors.NewConflict(schema.GroupResource{Resource: "gaggles"}, "web", errors.New("x")), MutationRejected},
		{"invalid", apierrors.NewInvalid(gk, "web", field.ErrorList{}), MutationRejected},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "gaggles"}, "web", errors.New("x")), MutationRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mutationState(tc.err); got != tc.want {
				t.Errorf("mutationState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyError_ReportsCommittedAndAmbiguous(t *testing.T) {
	cause := apierrors.NewTimeoutError("timeout", 1)
	ae := &ApplyError{
		Phase:         PhaseApply,
		Generation:    "abc123",
		Authoritative: "prev456",
		Mutations: []Mutation{
			{Op: OpCreate, Key: "Gaggle/goobers-system/web", State: MutationCommitted},
			{Op: OpUpdate, Key: "Gaggle/goobers-system/api", State: MutationAmbiguous},
			{Op: OpUpdate, Key: "Gaggle/goobers-system/db", State: MutationRejected},
		},
		Err: cause,
	}
	msg := ae.Error()
	for _, want := range []string{"apply", "abc123", "prev456", "web (committed)", "api (ambiguous)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "db") {
		t.Errorf("error %q should not report rejected mutations as mutations", msg)
	}
	if len(ae.Committed()) != 1 || len(ae.Ambiguous()) != 1 {
		t.Errorf("committed = %v, ambiguous = %v", ae.Committed(), ae.Ambiguous())
	}
	if !errors.Is(ae, cause) {
		t.Error("ApplyError must unwrap to its cause")
	}
}

func TestApplyError_NoMutations(t *testing.T) {
	ae := &ApplyError{Phase: PhasePrepare, Generation: "abc123", Err: errors.New("boom")}
	msg := ae.Error()
	if !strings.Contains(msg, "no committed or ambiguous mutations") {
		t.Errorf("error %q should state that nothing was mutated", msg)
	}
	if !strings.Contains(msg, "authoritative generation none") {
		t.Errorf("error %q should state that no generation is authoritative", msg)
	}
}
