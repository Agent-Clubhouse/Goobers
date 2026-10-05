package invoke

import (
	"errors"
	"sync"
	"testing"
)

func TestNestedWorkspaceWritersRemainVisibleToRuntimeLease(t *testing.T) {
	outerCtx, outer := WithWorkspaceQuiescence(t.Context())
	oneCtx, one := WithWorkspaceQuiescence(outerCtx)
	twoCtx, two := WithWorkspaceQuiescence(outerCtx)
	oneAck, twoAck := RegisterWorkspaceWriter(oneCtx), RegisterWorkspaceWriter(twoCtx)
	if outer.VerifyIdle() == nil || one.Verify() == nil || two.Verify() == nil {
		t.Fatal("live nested writers disappeared")
	}
	oneAck(nil)
	if one.Verify() != nil || outer.VerifyIdle() == nil || two.Verify() == nil {
		t.Fatal("one stage acknowledgement closed its sibling or runtime")
	}
	twoAck(nil)
	if outer.Verify() != nil || two.Verify() != nil {
		t.Fatal("joined nested writers did not settle")
	}
}

func TestNestedWorkspaceFailurePinsEveryEnclosingOwner(t *testing.T) {
	outerCtx, outer := WithWorkspaceQuiescence(t.Context())
	middleCtx, middle := WithWorkspaceQuiescence(outerCtx)
	innerCtx, inner := WithWorkspaceQuiescence(middleCtx)
	ack := RegisterWorkspaceWriter(innerCtx)
	lost := errors.New("exact container termination was not observed")
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { ack(lost) })
	}
	wg.Wait()
	for _, proof := range []*WorkspaceQuiescence{inner, middle, outer} {
		if err := proof.VerifyIdle(); !errors.Is(err, lost) || !errors.Is(err, ErrWorkspaceNotQuiescent) {
			t.Fatal("nested failure released authority", err)
		}
	}
}
