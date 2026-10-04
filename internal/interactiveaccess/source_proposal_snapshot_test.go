package interactiveaccess

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSourceProposalSnapshotChecksEveryTargetWithoutMinting(t *testing.T) {
	g := testGaggle()
	s, registrar := testService(t, g, testSources())
	target := Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}
	err := s.WithSourceProposalSnapshot(t.Context(), testPrincipal(), g.Name, func(ctx context.Context, copy *apiv1.Gaggle, access ProposalSnapshotAccess) error {
		copy.Spec.Project.Name = "not-applied"
		for _, action := range []apiv1.InteractiveAction{"repository.read", "source.proposeChange"} {
			if _, err := access(action, target); err != nil {
				return err
			}
		}
		foreign := target
		foreign.Repository.Name = "foreign"
		if _, err := access("repository.read", foreign); err == nil {
			t.Fatal("foreign source permitted")
		}
		if _, err := access("pr.repair", target); !errors.Is(err, ErrDenied) {
			t.Fatal("unrelated action", err)
		}
		return ctx.Err()
	})
	if err != nil || len(registrar.values) != 0 {
		t.Fatal(err, registrar.values)
	}
}

func TestSourceProposalSnapshotHoldsOneAppliedRevisionAndRevokes(t *testing.T) {
	g := testGaggle()
	s, _ := testService(t, g, testSources())
	started, release, published := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done, applied := make(chan error, 1), make(chan error, 1)
	target := Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}
	go func() {
		done <- s.WithSourceProposalSnapshot(t.Context(), testPrincipal(), g.Name, func(ctx context.Context, _ *apiv1.Gaggle, access ProposalSnapshotAccess) error {
			if _, err := access("repository.read", target); err != nil {
				return err
			}
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := access("source.proposeChange", target)
			return err
		})
	}()
	<-started
	changed := g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	go func() { applied <- s.Apply([]apiv1.Gaggle{*changed}, func() error { close(published); return nil }) }()
	select {
	case <-published:
		t.Fatal("policy changed during reviewed write")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	err := s.WithSourceProposalSnapshot(t.Context(), testPrincipal(), g.Name, func(_ context.Context, _ *apiv1.Gaggle, access ProposalSnapshotAccess) error {
		_, err := access("source.proposeChange", target)
		return err
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatal("revoked proposal grant accepted", err)
	}
}
