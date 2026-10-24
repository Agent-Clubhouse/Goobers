//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

const generatedChild = `apiVersion: goobers.dev/v1alpha1
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

type recordedPins struct{ pinned []string }

func (r *recordedPins) PinGeneration(_ context.Context, generation, owner string) error {
	r.pinned = append(r.pinned, generation+"|"+owner)
	return nil
}

// childWorkflowFamily is a parent whose stage holds a bound grant, served by
// the same composition FencedChildWorkflows builds per request.
type childWorkflowFamily struct {
	*authorityFamily
	pins  *recordedPins
	grant engine.ChildAuthorityGrant
}

func newChildWorkflowFamily(t *testing.T) *childWorkflowFamily {
	t.Helper()
	f := &childWorkflowFamily{authorityFamily: newAuthorityFamily(t), pins: &recordedPins{}}
	grant, err := f.authorities().AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{RunID: f.parent.pins.RunID, Origin: f.parent.origin, Endpoint: podEndpoint})
	if err != nil {
		t.Fatal(err)
	}
	f.grant = grant
	return f
}

func (f *childWorkflowFamily) service() *childworkflow.HTTPService {
	parents := f.parent.authority()
	parents.Pins = f.controller.RunPinsOf
	return childWorkflowService(f.controller, f.pins, parents, f.grants, time.Now)
}

// operations are every child-workflow route, each with its own credential.
func (f *childWorkflowFamily) operations(key string) map[string]func(token string) error {
	run := f.parent.pins.RunID
	return map[string]func(string) error{
		"validate": func(token string) error {
			_, err := f.service().ValidateChildWorkflow(context.Background(), token, run, []byte(generatedChild))
			return err
		},
		"start": func(token string) error {
			_, err := f.service().StartChildWorkflow(context.Background(), token, run, "another-call", []byte(generatedChild))
			return err
		},
		"status": func(token string) error {
			_, err := f.service().ChildWorkflowStatus(context.Background(), token, run, key)
			return err
		},
		"resolve": func(token string) error {
			_, err := f.service().ResolveChildWorkflow(context.Background(), token, run, apicontract.ChildWorkflowResolveRequest{InvocationKey: key, Action: "discard", ResultRef: "sha256:" + strings.Repeat("c", 64)})
			return err
		},
	}
}

func statusOf(t *testing.T, err error) (int, string) {
	t.Helper()
	var intervention *httpapi.InterventionError
	if !errors.As(err, &intervention) {
		t.Fatalf("error %v carries no HTTP status", err)
	}
	return intervention.Status, intervention.Code
}

func TestChildWorkflowsServeABoundGrantAndPinTheChildGeneration(t *testing.T) {
	f := newChildWorkflowFamily(t)
	run := f.parent.pins.RunID
	validated, err := f.service().ValidateChildWorkflow(t.Context(), f.grant.BearerToken, run, []byte(generatedChild))
	if err != nil || !validated.Valid {
		t.Fatalf("validate = %+v, %v", validated, err)
	}
	started, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, run, "call-1", []byte(generatedChild))
	if err != nil || started.AcceptanceID == "" {
		t.Fatalf("start = %+v, %v", started, err)
	}
	if want := fixtureGeneration + "|" + started.AcceptanceID; len(f.pins.pinned) != 1 || f.pins.pinned[0] != want {
		t.Fatalf("pins = %v, want the parent's generation owned by the acceptance (%s)", f.pins.pinned, want)
	}
	status, err := f.service().ChildWorkflowStatus(t.Context(), f.grant.BearerToken, run, "call-1")
	if err != nil || status.AcceptanceID != started.AcceptanceID {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if again, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, run, "call-1", []byte(generatedChild)); err != nil || !again.Duplicate || len(f.pins.pinned) != 2 {
		t.Fatalf("repeated start = %+v, %v (pins %v); a duplicate re-pins idempotently", again, err, f.pins.pinned)
	}
}

// HARD CONDITION (#8085 c): every child-workflow request re-resolves the
// parent's authority and checks the bound grant, so a grant whose attempt
// ended, whose parent stopped, that was revoked, or that control never bound
// authorizes nothing on any route, however valid its signature.
func TestEveryChildWorkflowRequestReResolvesAuthorityAndRechecksTheGrant(t *testing.T) {
	for name, breaker := range map[string]func(t *testing.T, f *childWorkflowFamily) string{
		"attempt finished": func(t *testing.T, f *childWorkflowFamily) string {
			if err := f.parent.run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			return f.grant.BearerToken
		},
		"parent no longer running": func(_ *testing.T, f *childWorkflowFamily) string {
			f.parent.execution.Status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
			return f.grant.BearerToken
		},
		"grant released": func(t *testing.T, f *childWorkflowFamily) string {
			if err := f.authorities().ReleaseChildAuthority(t.Context(), engine.ChildAuthorityRelease{RunID: f.parent.pins.RunID, Origin: f.parent.origin, GrantID: f.grant.GrantID}); err != nil {
				t.Fatal(err)
			}
			return f.grant.BearerToken
		},
		"self-minted grant control never bound": func(t *testing.T, f *childWorkflowFamily) string {
			verified, err := f.grants.VerifyChildWorkflowGrant(f.grant.BearerToken)
			if err != nil {
				t.Fatal(err)
			}
			forged := podauth.ChildWorkflowGrant{Gaggle: verified.Gaggle, RunID: verified.RunID, StageOccurrence: verified.StageOccurrence,
				AttemptID: verified.AttemptID, ConfigDigest: verified.ConfigDigest, PolicyDigest: verified.PolicyDigest}
			token, _, err := f.grants.MintChildWorkflowGrant(forged, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			return token
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newChildWorkflowFamily(t)
			if _, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, f.parent.pins.RunID, "call-1", []byte(generatedChild)); err != nil {
				t.Fatalf("start before the break: %v", err)
			}
			operations := f.operations("call-1")
			// Every route is open to the live grant before the break.
			if err := operations["status"](f.grant.BearerToken); err != nil {
				t.Fatalf("status before the break: %v", err)
			}
			token := breaker(t, f)
			for op, call := range operations {
				err := call(token)
				if err == nil {
					t.Fatalf("%s served after the break", op)
				}
				if status, code := statusOf(t, err); status != http.StatusForbidden || code != "child_workflow_authority_changed" {
					t.Fatalf("%s after the break = %d %s (%v), want the authority refusal", op, status, code, err)
				}
			}
		})
	}
}

// A check that cannot be made is a retry, not a refusal: the pod must not
// conclude its authority ended because the engine was briefly unreachable.
func TestChildWorkflowAuthorityOutageIsRetryable(t *testing.T) {
	f := newChildWorkflowFamily(t)
	f.parent.describe = serviceerror.NewUnavailable("temporal down")
	for op, call := range f.operations("call-1") {
		if status, code := statusOf(t, call(f.grant.BearerToken)); status != http.StatusServiceUnavailable || code != "child_workflow_authority_unavailable" {
			t.Fatalf("%s during an engine outage = %d %s, want a retryable 503", op, status, code)
		}
	}
}

type recordedUnpins struct {
	unpinned []string
	fail     error
}

func (r *recordedUnpins) UnpinGeneration(_ context.Context, generation, owner string) error {
	r.unpinned = append(r.unpinned, generation+"|"+owner)
	return r.fail
}

// The generation pin a child's acceptance took is released once the engine
// has bound the child, again on an already-bound Admit (so a failed release
// heals on the engine's retry), and a failed release is retryable.
func TestEngineBindReleasesTheChildGenerationPin(t *testing.T) {
	f := newChildWorkflowFamily(t)
	started, err := f.service().StartChildWorkflow(t.Context(), f.grant.BearerToken, f.parent.pins.RunID, "call-1", []byte(generatedChild))
	if err != nil {
		t.Fatal(err)
	}
	request := journal.ChildHandoffRequest{Gaggle: f.parent.pins.Gaggle, ParentRunID: f.parent.pins.RunID, RequestID: journal.Digest([]byte(started.RunID)),
		Action: "wait", ChildRunID: started.RunID, AcceptanceID: started.AcceptanceID, InvocationKey: "call-1", SourceDigest: started.SourceDigest, Origin: f.parent.origin}
	in := engine.ChildAdmissionInput{Request: request, ParentWorkflow: f.parent.pins.Workflow, ParentWorkflowID: f.parent.pins.RunID,
		ParentTemporalRunID: "temporal-run-1", Stage: "plan", OriginSeq: 2}
	unpins := &recordedUnpins{fail: errors.New("object store briefly down")}
	service := &Service{Custody: f.controller, Freeze: freezeForTest, Wait: time.Hour, Actor: "control-plane", Now: time.Now, Generations: unpins}
	if _, err := service.Admit(t.Context(), in); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("admit with a failed release = %v, want a retryable unavailability", err)
	}
	want := fixtureGeneration + "|" + started.AcceptanceID
	unpins.fail = nil
	binding, err := service.Admit(t.Context(), in)
	if err != nil || binding.Request.AcceptanceID != started.AcceptanceID {
		t.Fatalf("retried admit = %+v, %v", binding, err)
	}
	if len(unpins.unpinned) != 2 || unpins.unpinned[0] != want || unpins.unpinned[1] != want {
		t.Fatalf("released %v, want the child's generation owned by its acceptance (%s), on both admits", unpins.unpinned, want)
	}
}
