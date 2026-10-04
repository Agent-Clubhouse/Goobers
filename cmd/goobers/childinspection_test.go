package main

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestChildInspectionRetainsPinsWithoutCurrentExecutionAuthority(t *testing.T) {
	service, pinned, _ := humanChildCredentialFixture(t)
	ref, err := retainedChildExecutionRef(t.Context(), service.childQueue, pinned.identity, true)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := service.layout.FindRunDir(ref.Child.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = retainedChildExecutionRef(t.Context(), service.childQueue, id, true); err == nil {
		t.Fatal("old source is still current")
	}
	inspected, err := service.inspectChildExecution(t.Context(), id)
	if err != nil || inspected.Runner != nil || inspected.Machine == nil || inspected.Machine.Digest() != id.WorkflowDigest {
		t.Fatal(inspected, err)
	}
	id.Child.SourceDigest = pinned.identity.Child.EnvelopeDigest
	if _, err = service.inspectChildExecution(t.Context(), id); err == nil {
		t.Fatal("tampered source inspected")
	}
}
