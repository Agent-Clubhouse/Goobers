package childadmissionclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
)

// Until the stage blob plane is shown to serve control's store, every acquire
// is a retryable refusal that never reaches control; probes are paced while
// failing; once verified, acquires pass through for good. Release always does.
func TestProbedAuthorityWithholdsUntilTheBlobPlaneIsVerified(t *testing.T) {
	inner := &recordingAuthority{grant: engine.ChildAuthorityGrant{GrantID: "g"}}
	clock := time.Unix(1_800_000_000, 0)
	probes, reports := 0, 0
	missing := errors.New("blob not found")
	probeErr := missing
	probed := &ProbedAuthority{Inner: inner, Now: func() time.Time { return clock },
		Probe:  func(context.Context) error { probes++; return probeErr },
		Report: func(error) { reports++ }}

	if err := probed.Verify(t.Context()); !errors.Is(err, childresult.ErrBlobPlaneUnverified) || !errors.Is(err, missing) {
		t.Fatalf("boot verify against a plane without the canary = %v", err)
	}
	_, err := probed.AcquireChildAuthority(t.Context(), authorityRequest())
	if !errors.Is(err, engine.ErrChildCustodyUnavailable) || !errors.Is(err, childresult.ErrBlobPlaneUnverified) || len(inner.acquired) != 0 {
		t.Fatalf("acquire while unverified = %v (control reached %d times), want withheld and retryable", err, len(inner.acquired))
	}
	if probes != 1 {
		t.Fatalf("probes inside the interval = %d, want only the boot probe", probes)
	}
	clock = clock.Add(BlobPlaneProbeInterval)
	if _, err := probed.AcquireChildAuthority(t.Context(), authorityRequest()); err == nil || probes != 2 || reports != 2 {
		t.Fatalf("acquire after the interval = %v (probes %d, reports %d), want a re-probe that still fails", err, probes, reports)
	}
	if err := probed.ReleaseChildAuthority(t.Context(), engine.ChildAuthorityRelease{}); err != nil || len(inner.released) != 1 {
		t.Fatalf("release while unverified = %v (%d), want it passed through", err, len(inner.released))
	}

	probeErr = nil // the plane now serves control's store
	clock = clock.Add(BlobPlaneProbeInterval)
	if grant, err := probed.AcquireChildAuthority(t.Context(), authorityRequest()); err != nil || grant.GrantID != "g" || len(inner.acquired) != 1 {
		t.Fatalf("acquire once verified = %+v, %v", grant, err)
	}
	probeErr = missing // verification is sticky: no further probes, ever
	clock = clock.Add(10 * BlobPlaneProbeInterval)
	if _, err := probed.AcquireChildAuthority(t.Context(), authorityRequest()); err != nil || probes != 3 {
		t.Fatalf("acquire after verification = %v (probes %d), want no re-probe", err, probes)
	}
}

// A slow probe (a network call) does not hold the lock: while one acquire is
// probing, another returns at once with the last outcome, still withheld.
func TestProbedAuthorityDoesNotBlockAcquiresBehindASlowProbe(t *testing.T) {
	inner := &recordingAuthority{}
	clock := time.Unix(1_800_000_000, 0)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	slow := false
	probed := &ProbedAuthority{Inner: inner, Now: func() time.Time { return clock },
		Probe: func(context.Context) error {
			if slow {
				started <- struct{}{}
				<-release
			}
			return errors.New("blob not found")
		}}
	_ = probed.Verify(t.Context())
	clock = clock.Add(BlobPlaneProbeInterval)
	slow = true
	done := make(chan error, 1)
	go func() {
		_, err := probed.AcquireChildAuthority(context.Background(), authorityRequest())
		done <- err
	}()
	<-started // the first acquire is inside its probe
	result := make(chan error, 1)
	go func() {
		_, err := probed.AcquireChildAuthority(context.Background(), authorityRequest())
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, childresult.ErrBlobPlaneUnverified) {
			t.Fatalf("concurrent acquire = %v, want the last outcome (withheld)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a concurrent acquire blocked behind the slow probe")
	}
	close(release)
	if err := <-done; !errors.Is(err, childresult.ErrBlobPlaneUnverified) {
		t.Fatalf("probing acquire = %v", err)
	}
}
