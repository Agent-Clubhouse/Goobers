package engine

import (
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

func TestOutboxPreExecutionFailureKeepsPodEnvelope(t *testing.T) {
	for _, code := range []string{"workspace_checkout_failed", dispatcher.CodeAgenticKitUnavailable} {
		t.Run(code, func(t *testing.T) {
			failure := apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure, Summary: "failed before execution",
				Error: journal.ErrorInfoFor(code, errors.New("connection reset"), true),
			}
			run := func(declared bool) (stageActivityResult, error) {
				store := surrenderStore(t)
				putSurrendered(t, store, "preexec", "build", 1, dispatcher.SurrenderedResult{
					ContractVersion: dispatcher.SurrenderContractForOutbox(declared),
					Result:          failure,
				})
				activities := &Activities{
					Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", SurrenderConfirmed: true}},
					Surrenders: store,
				}
				input := dispatchInput("preexec", "build", 1)
				if declared {
					input.Outbox = []string{"report.txt"}
				}
				return activities.DispatchStage(t.Context(), input)
			}
			baseResult, baseErr := run(false)
			gotResult, gotErr := run(true)
			describe := func(result stageActivityResult, err error) string {
				var appErr *temporal.ApplicationError
				if errors.As(err, &appErr) {
					return "error:" + appErr.Type() + ":" + appErr.Error()
				}
				if result.Error == nil {
					return "envelope:" + string(result.Status)
				}
				return "envelope:" + string(result.Status) + ":" + result.Error.Code + ":" + result.Error.Message
			}
			if (baseErr == nil) != (gotErr == nil) ||
				describe(baseResult, baseErr) != describe(gotResult, gotErr) {
				t.Fatalf("undeclared=%s declared=%s", describe(baseResult, baseErr), describe(gotResult, gotErr))
			}
		})
	}
}
