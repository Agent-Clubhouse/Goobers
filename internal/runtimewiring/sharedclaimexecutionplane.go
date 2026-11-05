package runtimewiring

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
)

// RemoteSharedExecutionFence observes claim ownership through the daemon.
// The parent runtime's bearer stays in the parent. Nothing in this reader
// passes claims authority to a shell command or an agentic subprocess.
func RemoteSharedExecutionFence(baseURL string, tokenForRun func(string) (string, error)) ExecutionFenceStart {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
		stop := func() {}
		read := func(ctx context.Context) (string, claimsclient.Listing, error) {
			token, err := tokenForRun(env.RunID)
			if err != nil {
				return "", claimsclient.Listing{}, err
			}
			client, err := claimsclient.NewExecutionObserver(claimsclient.HTTPConfig{BaseURL: baseURL, Token: token, RunID: env.RunID})
			if err != nil {
				return "", claimsclient.Listing{}, err
			}
			return client.ExecutionSnapshot(ctx)
		}
		mode, initial, err := read(ctx)
		if err != nil || mode != "shared" {
			return ctx, stop, err
		}
		first := true
		return claimsclient.StartExecutionFence(ctx, env.RunID, func(ctx context.Context) (claimsclient.Listing, error) {
			if first {
				first = false
				return initial, nil
			}
			mode, listing, err := read(ctx)
			if err == nil && mode != "shared" {
				err = fmt.Errorf("shared execution policy changed during invocation")
			}
			return listing, err
		})
	}
}
