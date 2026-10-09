package childpod

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestRetainedAttemptPreservesTypedJoinInputsAndLargeIntegers(t *testing.T) {
	r := retainedFixture()
	r.Input.Attempt.Envelope = &apiv1.InvocationEnvelope{Inputs: map[string]any{
		"completeness": []journal.BranchOutcome{{Branch: 2, Name: "inspect", Status: journal.BranchSucceeded}},
		"exact":        uint64(9007199254740993),
	}}
	var err error
	r.Input.Attempt, err = normalizeAttempt(r.Input.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "join-inputs"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	ref, err := RecordRetainedAttempt(run, r)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadRetainedAttempt(reader, ref)
	if err != nil {
		t.Fatal(err)
	}
	if read.Input.BindingDigest() != r.Input.BindingDigest() {
		t.Fatal("retention changed transport binding")
	}
	if got, ok := read.Input.Attempt.Envelope.Inputs["exact"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatal("integer precision lost", got)
	}
	if _, err = normalizeAttempt(r.Input.Attempt); err != nil {
		t.Fatal("normalized replay refused", err)
	}
}
