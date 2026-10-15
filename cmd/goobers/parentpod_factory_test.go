package main

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestParentDispatchAdmissionUsesCurrentParentModelAuthority(t *testing.T) {
	f := containedParentFixture(t)
	run, env := configuredChildStage(t, f)
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	defer unregisterDaemonStageGrants(f.layout.Root, service)
	if err := service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	parent := parentStagePod{service: service, journal: run, identity: id, goober: env.Goober}
	release, err := parent.admission(env)(t.Context())
	if err != nil {
		t.Fatal("allowed parent refused", err)
	}
	release()
	// Child delegation can still include agent:model while the parent itself
	// loses model authority. The transport must refuse before creating a pod.
	for i := range f.applied.Goobers {
		f.applied.Goobers[i].Spec.Capabilities = nil
	}
	if err := service.children.ApplyDefinitions(t.Context(), f.applied, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if release, err := parent.admission(env)(t.Context()); err == nil {
		release()
		t.Fatal("revoked parent model authority dispatched")
	}
}
