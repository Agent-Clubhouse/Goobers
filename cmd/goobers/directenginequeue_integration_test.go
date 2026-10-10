//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/enginestartintent"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporaldial"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type directLostReplyClient struct {
	client.Client
	starts *atomic.Int32
	lose   bool
}

func (c *directLostReplyClient) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error) {
	c.starts.Add(1)
	run, err := c.Client.ExecuteWorkflow(ctx, options, workflow, args...)
	if err == nil && c.lose {
		return nil, errors.New("fixture discarded successful start reply")
	}
	return run, err
}

// A real frontend supplies the actual SDK-encoded first history batch. The
// injected fault discards only a successful response, after the remote effect.
func TestIntegrationDirectEngineQueueVerifiesRealHistoryAfterLostReply(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	server, err := temporaltest.StartDevServer(t.Context(), t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	original := dialDirectEngine
	t.Cleanup(func() { dialDirectEngine = original })
	for _, mode := range []string{"observed", "lost-reply"} {
		t.Run(mode, func(t *testing.T) {
			var starts atomic.Int32
			dialDirectEngine = func(ctx context.Context, host, namespace string, tls *temporaldial.TLS, dc ...converter.DataConverter) (client.Client, error) {
				c, err := original(ctx, host, namespace, tls, dc...)
				if err != nil {
					return nil, err
				}
				return &directLostReplyClient{Client: c, starts: &starts, lose: mode == "lost-reply"}, nil
			}
			root := initDeterministicDemo(t)
			layout := instance.NewLayout(root)
			args := []string{"--direct", "--gaggle", "example", "--temporal-hostport", server.FrontendHostPort(), "--temporal-namespace", "default", "--task-queue", "direct-queue-qualification", "--dedupe-key", mode, "default-implement", root}
			var stdout, stderr bytes.Buffer
			wantCode := 0
			if mode == "lost-reply" {
				wantCode = 1
			}
			if code := runEngineStart(args, &stdout, &stderr); code != wantCode || starts.Load() != 1 {
				t.Fatal(code, starts.Load(), stdout.String(), stderr.String())
			}
			// Reopen through the daemon's real queue owner, without authored source.
			writeFileContent(t, filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml"), "broken pending definition: [")
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			service := acceptedService(t, filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
			service.directEngine = directEngineService(layout, service.queue, cfg)
			if err := service.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			binding, err := enginestartintent.CredentialBinding(cfg)
			if err != nil {
				t.Fatal(err)
			}
			request := enginestartintent.Request{HostPort: server.FrontendHostPort(), Namespace: "default", TaskQueue: "direct-queue-qualification", Gaggle: "example", Workflow: "default-implement", DedupeKey: mode, Binding: binding}
			receipt, err := service.queue.ByKey(t.Context(), request.Key())
			if err != nil || receipt.State != triggerqueue.Dispatched || receipt.RunID == "" {
				t.Fatal(receipt, err)
			}
			if code := runEngineStart(args, &stdout, &stderr); code != 0 || starts.Load() != 1 {
				t.Fatal("retry repeated remote effect", code, starts.Load(), stderr.String())
			}
		})
	}
}
