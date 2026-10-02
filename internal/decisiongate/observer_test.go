package decisiongate

import (
	"errors"
	"sync"
	"testing"
)

func TestObserverRecordsAndSamples(t *testing.T) {
	g, _ := New(&fake{yes: 0.99}, cfg(), nil)
	var mu sync.Mutex
	var got []ShadowRecord
	o := NewObserver(g, 0, 2, func(r ShadowRecord) { mu.Lock(); got = append(got, r); mu.Unlock() })
	o.Observe("run-1", "the JSON is corrupted")
	o.Observe("run-1", "")
	o.Wait()
	if len(got) != 1 || got[0].Verdict != ClaimSpurious || !got[0].AgentClaimedBad || got[0].InputKnown {
		t.Fatalf("%+v", got)
	}
	var nilObs *Observer
	nilObs.Observe("x", "y")
}

func TestObserverDropsWhenBusyAndSurvivesErrors(t *testing.T) {
	g, _ := New(&fake{err: errTest}, cfg(), nil)
	n := 0
	o := NewObserver(g, 1, 1, func(r ShadowRecord) {
		n++
		if r.Err == nil || r.Verdict != ClaimUnsure {
			t.Error("error must be recorded as unsure")
		}
	})
	o.Observe("r", "corrupted")
	o.Wait()
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
}

func TestClaimsBadInput(t *testing.T) {
	for s, want := range map[string]bool{"The JSON appears corrupted": true, "input was truncated": true, "all tests passed": false, "": false} {
		if ClaimsBadInput(s) != want {
			t.Errorf("%q", s)
		}
	}
}

var errTest = errors.New("down")
