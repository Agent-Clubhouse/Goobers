package invoke

import (
	"errors"
	"testing"
)

func TestWorkspaceQuiescenceRequiresEveryWriterAcknowledgement(t *testing.T) {
	ctx, proof := WithWorkspaceQuiescence(t.Context())
	if !errors.Is(proof.Verify(), ErrWorkspaceNotQuiescent) {
		t.Fatal("runtime with no process ownership was trusted")
	}
	first, second := RegisterWorkspaceWriter(ctx), RegisterWorkspaceWriter(ctx)
	first(nil)
	first(errors.New("duplicate ignored"))
	if !errors.Is(proof.Verify(), ErrWorkspaceNotQuiescent) {
		t.Fatal("one still-active writer was ignored")
	}
	second(nil)
	if err := proof.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceQuiescenceRefusesUnverifiedTermination(t *testing.T) {
	ctx, proof := WithWorkspaceQuiescence(t.Context())
	RegisterWorkspaceWriter(ctx)(errors.New("writer survived join deadline"))
	if !errors.Is(proof.Verify(), ErrWorkspaceNotQuiescent) {
		t.Fatal("unverified termination became snapshot permission")
	}
	if RegisterWorkspaceWriter(t.Context()) != nil {
		t.Fatal("ordinary invocations changed behavior")
	}
}
