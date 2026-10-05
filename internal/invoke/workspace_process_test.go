package invoke

import (
	"context"
	"errors"
	"testing"
	"time"
)

type processProofFixture struct {
	stopped, killed int
	failure         error
}

func (p *processProofFixture) StopAndWait(ctx context.Context) error {
	p.stopped++
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("missing join deadline")
	}
	return p.failure
}
func (p *processProofFixture) Kill() error { p.killed++; return nil }

func TestWorkspaceProcessUsesObservedCleanupOnNormalReturn(t *testing.T) {
	ctx, proof := WithWorkspaceQuiescence(t.Context())
	p := &processProofFixture{}
	stop, joined := TrackWorkspaceProcess(ctx, p, time.Second)
	if err := proof.Verify(); err == nil {
		t.Fatal("registration alone granted custody")
	}
	joined()
	joined()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if err := proof.Verify(); err != nil || p.stopped != 1 || p.killed != 0 {
		t.Fatalf("proof=%v process=%+v", err, p)
	}
}

func TestWorkspaceProcessNeverConfusesFallbackKillWithObservedTermination(t *testing.T) {
	ctx, proof := WithWorkspaceQuiescence(t.Context())
	p := &processProofFixture{failure: errors.New("process census unavailable")}
	_, joined := TrackWorkspaceProcess(ctx, p, time.Second)
	joined()
	if !errors.Is(proof.Verify(), ErrWorkspaceNotQuiescent) || p.killed != 1 {
		t.Fatalf("unverified fallback=%+v", p)
	}
	ordinary := &processProofFixture{}
	stop, done := TrackWorkspaceProcess(t.Context(), ordinary, time.Second)
	done()
	if err := stop(); err != nil || ordinary.stopped != 0 || ordinary.killed != 1 {
		t.Fatalf("ordinary behavior changed: %+v %v", ordinary, err)
	}
}
