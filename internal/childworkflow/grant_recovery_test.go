package childworkflow

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestContainedGrantLostDeliveryKeepsExactGrantAndRevocation(t *testing.T) {
	r, _, env := runtimeFixture(t)
	secrets := journal.NewRegistryScrubber()
	first, revoke, err := r.AcquireForContainedPod(t.Context(), env, secrets)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			access, _, err := r.AcquireForContainedPod(t.Context(), env, secrets)
			if err != nil || access == nil || access.BearerToken != first.BearerToken {
				t.Error("lost delivery changed authority", err)
			}
		})
	}
	wg.Wait()
	if bytes.Contains(secrets.Scrub([]byte(first.BearerToken)), []byte(first.BearerToken)) {
		t.Fatal("unregistered credential")
	}
	if _, _, err := r.Acquire(t.Context(), env, secrets); err == nil {
		t.Fatal("ordinary local acquisition stole live grant")
	}
	if err := revoke(); err != nil {
		t.Fatal(err)
	}
	if access, _, err := r.AcquireForContainedPod(t.Context(), env, secrets); err == nil || access != nil {
		t.Fatal("recovered revoked grant")
	}
}

func TestContainedGrantRecoveryRefusesPolicyAndAttemptReplacement(t *testing.T) {
	for _, mode := range []string{"policy", "attempt"} {
		t.Run(mode, func(t *testing.T) {
			r, f, env := runtimeFixture(t)
			secrets := journal.NewRegistryScrubber()
			_, oldRevoke, err := r.AcquireForContainedPod(t.Context(), env, secrets)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "policy" {
				defs := copyRuntimeDefinitions(r.definitions)
				defs.Gaggles[0].Annotations = map[string]string{"revision": "changed"}
				if err := r.ApplyDefinitions(t.Context(), defs, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			} else {
				next := f.start(t, 1, 2, true)
				access, revoke, err := r.AcquireForContainedPod(t.Context(), next, secrets)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = revoke() }()
				if err := oldRevoke(); err != nil {
					t.Fatal(err)
				}
				again, _, err := r.AcquireForContainedPod(t.Context(), next, secrets)
				if err != nil || again.BearerToken != access.BearerToken {
					t.Fatal("old cleanup revoked replacement", err)
				}
			}
			if access, _, err := r.AcquireForContainedPod(t.Context(), env, secrets); err == nil || access != nil {
				t.Fatal("stale grant recovered")
			}
		})
	}
}

func TestContainedGrantRecoveryRefusesCancelledOrSettledParent(t *testing.T) {
	for _, settle := range []bool{false, true} {
		r, _, env := runtimeFixture(t)
		secrets := journal.NewRegistryScrubber()
		if _, _, err := r.AcquireForContainedPod(t.Context(), env, secrets); err != nil {
			t.Fatal(err)
		}
		parent := triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}
		var err error
		if settle {
			err = r.queue.MarkChildParentSettled(t.Context(), parent, time.Now())
		} else {
			err = r.queue.FenceChildParent(t.Context(), parent, "operator", time.Now())
		}
		if err != nil {
			t.Fatal(err)
		}
		if access, _, err := r.AcquireForContainedPod(t.Context(), env, secrets); err == nil || access != nil {
			t.Fatal("closed parent recovered grant")
		}
	}
}
