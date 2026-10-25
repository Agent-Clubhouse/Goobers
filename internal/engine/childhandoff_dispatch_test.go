package engine

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

type handoffAuthority struct {
	fakeChildAuthority
	observe func(context.Context, ChildHandoffObservation) (*journal.ChildHandoffRequest, error)
}

func (a *handoffAuthority) ObserveChildHandoff(ctx context.Context, in ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
	return a.observe(ctx, in)
}

type handoffDispatcher struct {
	dispatch func(context.Context, dispatcher.Attempt) (dispatcher.Report, error)
}

func (d handoffDispatcher) Dispatch(ctx context.Context, attempt dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	return d.dispatch(ctx, attempt)
}

func handoffInput() DispatchStageInput {
	in := dispatchInput(authorityRunID, "plan", 1)
	in.ChildWorkflowsEnabled = true
	in.Envelope.ChildWorkflowOrigin = childCapableInput().Envelope.ChildWorkflowOrigin
	return in
}
func acceptedHandoff(in DispatchStageInput) journal.ChildHandoffRequest {
	request := journal.ChildHandoffRequest{Gaggle: in.Envelope.Gaggle, ParentRunID: in.Envelope.RunID, Action: "wait", ChildRunID: "accepted-child", AcceptanceID: "trigger-accepted-child", InvocationKey: "first", SourceDigest: journal.Digest([]byte("source")), Origin: *in.Envelope.ChildWorkflowOrigin}
	request.RequestID = ChildHandoffDigest(request)
	return request
}

func TestDispatchStageAcceptedChildStopsBeforeYield(t *testing.T) {
	for _, finalOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "while-running", true: "exit-race"}[finalOnly], func(t *testing.T) {
			input := handoffInput()
			handoff := acceptedHandoff(input)
			stopped := make(chan struct{})
			finishedRead := false
			authority := &handoffAuthority{fakeChildAuthority: fakeChildAuthority{grant: liveGrant()}}
			authority.observe = func(_ context.Context, request ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
				if request.GrantID != "grant-1" || request.Origin != handoff.Origin {
					t.Error("observation did not bind the lent grant")
				}
				if request.Finish {
					select {
					case <-stopped:
					default:
						t.Error("final observation preceded stop")
					}
					finishedRead = true
				}
				return &handoff, nil
			}
			a := &Activities{ChildAuthority: authority, Surrenders: surrenderStore(t), Dispatcher: handoffDispatcher{dispatch: func(ctx context.Context, attempt dispatcher.Attempt) (dispatcher.Report, error) {
				if attempt.ChildWorkflowAccess == "" {
					t.Error("stage got no child tool authority")
				}
				if !finalOnly {
					<-ctx.Done()
				}
				close(stopped)
				return dispatcher.Report{ParentWritersStopped: true}, ctx.Err()
			}}}
			result, err := a.DispatchStage(t.Context(), input)
			if err != nil || result.ChildWait == nil || *result.ChildWait != handoff || !finishedRead {
				t.Fatalf("handoff=%+v err=%v final=%v", result.ChildWait, err, finishedRead)
			}
			if len(authority.released) != 1 {
				t.Fatal("grant not released")
			}
		})
	}
}

func TestDispatchStageChildHandoffRefusals(t *testing.T) {
	for _, scenario := range []string{"forged-origin", "forged-digest", "untrusted-output", "disabled-pin", "ambiguous-stop", "observation-refused"} {
		t.Run(scenario, func(t *testing.T) {
			input := handoffInput()
			handoff := acceptedHandoff(input)
			stopped := true
			authority := &handoffAuthority{fakeChildAuthority: fakeChildAuthority{grant: liveGrant()}}
			authority.observe = func(context.Context, ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
				if scenario == "observation-refused" {
					return nil, ErrChildCustodyRefused
				}
				if scenario == "untrusted-output" {
					return nil, nil
				}
				return &handoff, nil
			}
			switch scenario {
			case "forged-origin":
				handoff.Origin.AttemptID = "forged"
				handoff.RequestID = ChildHandoffDigest(handoff)
			case "forged-digest":
				handoff.InvocationKey = "forged"
			case "disabled-pin":
				input.ChildWorkflowsEnabled = false
			case "ambiguous-stop":
				stopped = false
			}
			store := surrenderStore(t)
			putSurrendered(t, store, input.Envelope.RunID, "plan", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"childWait": handoff}}})
			calls := 0
			a := &Activities{ChildAuthority: authority, Surrenders: store, Dispatcher: handoffDispatcher{dispatch: func(context.Context, dispatcher.Attempt) (dispatcher.Report, error) {
				calls++
				return dispatcher.Report{ParentWritersStopped: stopped, SurrenderConfirmed: true}, nil
			}}}
			result, err := a.DispatchStage(t.Context(), input)
			if result.ChildWait != nil {
				t.Fatalf("refused request yielded %+v", result.ChildWait)
			}
			if scenario == "untrusted-output" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var application *temporal.ApplicationError
			if !errors.As(err, &application) || !application.NonRetryable() {
				t.Fatalf("error=%v, want final refusal", err)
			}
			if scenario == "disabled-pin" {
				if calls != 0 || len(authority.acquired) != 0 {
					t.Fatal("disabled pin lent authority or dispatched")
				}
			} else if !isChildHandoffFailure(err) {
				t.Fatalf("error permits stage retry: %v", err)
			}
		})
	}
}
