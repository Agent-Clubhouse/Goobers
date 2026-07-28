package journal

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSnapshotAndArtifactIntegrityAreJournaled(t *testing.T) {
	run, err := Create(
		t.TempDir(),
		testIdentity(),
		map[string][]byte{"item": []byte(`{"id":"42"}`)},
		WithInputIntegrity(map[string]apiv1.Integrity{"item": apiv1.IntegrityUnapproved}),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = run.Close() }()

	artifact, err := run.RecordArtifact("review.json", []byte(`{"decision":"pass"}`))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if artifact.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("artifact integrity = %q, want derived", artifact.Integrity)
	}

	reader, err := OpenRead(run.Dir())
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if len(identity.Inputs) != 1 || identity.Inputs[0].Integrity != apiv1.IntegrityUnapproved ||
		identity.Inputs[0].Ref.Integrity != apiv1.IntegrityUnapproved {
		t.Fatalf("input refs = %+v", identity.Inputs)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawArtifact bool
	for _, event := range events {
		switch event.Type {
		case EventArtifactRecorded:
			sawArtifact = event.Integrity == apiv1.IntegrityDerived &&
				event.Ref != nil && event.Ref.Integrity == apiv1.IntegrityDerived
		}
	}
	if !sawArtifact {
		t.Fatalf("artifact integrity event missing: events=%+v", events)
	}
}
