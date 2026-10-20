package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/mergepolicy"
	"github.com/goobers/goobers/providers"
)

// A transport error can follow provider acceptance. Only a confirmed refusal,
// a confirmed result, or a subsequent merged-state read ends custody early.
// Keep the original result/error intact for existing receipt/refusal reporting.
func landWithPublicationCustody(ctx context.Context, lander mergepolicy.Lander, dispatcher *providers.Dispatcher, req mergepolicy.Request) (mergepolicy.Result, error) {
	result, err := lander.Land(ctx, dispatcher, req)
	if err == nil || result.Outcome != "" || providers.IsConfirmedMergeRefusal(err) {
		return result, err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	poll, verifyErr := dispatcher.PollPullRequest(verifyCtx, providers.PullRequestPollRequest{Repository: req.Repository, PullID: req.PullID})
	if verifyErr != nil || !poll.Merged || poll.HeadSHA != req.ExpectedHeadSHA {
		if custody, ok := ctx.Value(publicationCustodyKey{}).(*publicationCustodyState); ok {
			custody.retain = true
		}
	}
	return result, err
}
