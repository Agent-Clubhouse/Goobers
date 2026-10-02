package harness

import (
	"context"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/launchreceipt"
)

// WithLaunchReceipts installs the local/self persistence barrier. Remote pods
// use their independently recorded control-plane receipt, not this authority.
func WithLaunchReceipts(recorder launchreceipt.Recorder) Option {
	return func(e *Executor) { e.launchReceipts = recorder }
}

func (e *Executor) recordPreparedLaunch(ctx context.Context, req RunRequest) error {
	if e.launchReceipts == nil {
		return nil
	}
	facts := launchreceipt.PreparedLocal("agent")
	facts.HarnessDigest = launchreceipt.Digest([]byte(e.adapter.Name()))
	facts.HarnessVersionDigest = launchreceipt.Digest([]byte(e.harnessVersion))
	facts.RequestedModelDigest = launchreceipt.Digest([]byte(e.model))
	effort, err := configuredReasoningEffort(e.harnessOptions)
	if err != nil {
		return launchreceipt.ErrLocalPersistence
	}
	facts.RequestedEffortDigest = launchreceipt.Digest([]byte(effort))
	if req.Sandbox != nil {
		facts.PreparedSandbox = req.Sandbox.Mechanism()
	}
	if err := launchreceipt.RecordLocalInvocation(ctx, e.launchReceipts, req.Envelope, req.Mode == ModeReview, facts); err != nil {
		return invoke.InfrastructureFailure(err)
	}
	return nil
}
