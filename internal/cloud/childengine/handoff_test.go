package childengine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type handoffTestQueue struct {
	*triggerqueue.Store
	pins    acceptance.RunPins
	corrupt bool
	receipt []byte
}

func (q handoffTestQueue) RunPinsOf(context.Context, string) (acceptance.RunPins, error) {
	return q.pins, nil
}
func (q handoffTestQueue) VerifiedChildStart(ctx context.Context, id triggerqueue.ChildIdentity, actor string) (triggerqueue.Record, error) {
	if q.corrupt {
		return triggerqueue.Record{}, childworkflow.ErrSubmissionInvalid
	}
	return q.Store.VerifiedChildStart(ctx, id, actor)
}

const handoffChildSource = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: generated-check
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: check
  tasks:
    - name: check
      type: deterministic
      goal: Run the local check
      run:
        command: ["true"]
      next: ""
`

func TestObserveCloudChildHandoffFromAcceptedCustody(t *testing.T) {
	for _, scenario := range []string{"accepted", "no-child", "wrong-grant", "wrong-origin", "untrusted-receipt", "revoked", "final-read"} {
		t.Run(scenario, func(t *testing.T) {
			f := newParentFixture(t)
			authority, err := f.authority().PrepareOrigin(t.Context(), f.pins.RunID, f.origin)
			if err != nil {
				t.Fatal(err)
			}
			authority.Origin.GrantID = "bound-grant"
			store, err := triggerqueue.Open(filepath.Join(t.TempDir(), "children.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			now := time.Now()
			bound := authority.Origin.Binding(now.Add(time.Hour))
			if err := store.BindChildAuthority(t.Context(), bound, "", now); err != nil {
				t.Fatal(err)
			}
			service := childworkflow.SubmissionService{Queue: store, Authority: childworkflow.AuthorityResolverFunc(func(context.Context, childworkflow.Origin) (childworkflow.Authority, error) { return authority, nil })}
			var accepted childworkflow.Submission
			if scenario != "no-child" {
				accepted, err = service.Submit(t.Context(), authority.Origin, childworkflow.SubmissionRequest{InvocationKey: "first", Source: []byte(handoffChildSource)})
				if err != nil {
					t.Fatal(err)
				}
			}
			request := engine.ChildHandoffObservation{ChildAuthorityRelease: engine.ChildAuthorityRelease{RunID: f.pins.RunID, Origin: f.origin, GrantID: bound.GrantID}}
			queue := handoffTestQueue{Store: store, pins: f.pins}
			switch scenario {
			case "wrong-grant":
				request.GrantID = "forged-grant"
			case "wrong-origin":
				request.Origin.AttemptID = "forged-attempt"
			case "untrusted-receipt":
				queue.corrupt = true
			case "revoked":
				if err := store.RevokeChildAuthority(t.Context(), bound); err != nil {
					t.Fatal(err)
				}
			case "final-read":
				request.Finish, request.Stopped = true, true
			}
			owner := Authorities{Queue: &queue, Parents: f.authority()}
			got, err := owner.ObserveChildHandoff(t.Context(), request)
			switch scenario {
			case "no-child":
				if err != nil || got != nil {
					t.Fatalf("no child: %+v %v", got, err)
				}
			case "accepted", "final-read":
				if err != nil || got == nil {
					t.Fatalf("accepted: %+v %v", got, err)
				}
				if got.ChildRunID != accepted.Child.RunID || got.AcceptanceID != accepted.Child.AcceptanceID || got.SourceDigest != journal.Digest([]byte(handoffChildSource)) || got.Origin != f.origin || got.RequestID != engine.ChildHandoffDigest(*got) {
					t.Fatalf("wrong handoff: %+v", got)
				}
				request.Finish = true
				if _, err := owner.ObserveChildHandoff(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				if err := store.CheckChildAuthority(t.Context(), bound, now); !errors.Is(err, triggerqueue.ErrChildAuthorityChanged) {
					t.Fatalf("final read did not revoke: %v", err)
				}
			default:
				if got != nil || !errors.Is(err, engine.ErrChildCustodyRefused) {
					t.Fatalf("forged custody accepted: %+v %v", got, err)
				}
			}
		})
	}
}

func (q *handoffTestQueue) RecoverChildHandoff(context.Context, triggerqueue.ChildAuthority) ([]byte, error) {
	return q.receipt, nil
}
func (q *handoffTestQueue) ConfirmChildHandoff(ctx context.Context, grant triggerqueue.ChildAuthority, _ *triggerqueue.ChildRecord, receipt []byte, finish, stopped bool) error {
	if len(q.receipt) != 0 {
		return nil
	}
	if err := q.CheckChildAuthority(ctx, grant, time.Now()); err != nil {
		return err
	}
	if !finish {
		return nil
	}
	if err := q.RevokeChildAuthority(ctx, grant); err != nil {
		return err
	}
	if stopped {
		q.receipt = receipt
	}
	return nil
}
