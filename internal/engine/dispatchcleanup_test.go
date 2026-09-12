package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

type dispatchCleanupLogger struct {
	mu         sync.Mutex
	diagnostic string
	accepted   bool
}

func (*dispatchCleanupLogger) Debug(string, ...any) {}
func (*dispatchCleanupLogger) Info(string, ...any)  {}
func (*dispatchCleanupLogger) Error(string, ...any) {}
func (l *dispatchCleanupLogger) Warn(message string, values ...any) {
	if message != "dispatch pod cleanup was not confirmed" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i+1 < len(values); i += 2 {
		switch values[i] {
		case "error":
			l.diagnostic, _ = values[i+1].(string)
		case "deleteAccepted":
			l.accepted, _ = values[i+1].(bool)
		}
	}
}

func TestDispatchCleanupWarningIsBoundedAndScrubbed(t *testing.T) {
	const secret = "registered-cleanup-canary"
	scrubber := journal.NewRegistryScrubber()
	scrubber.Register([]byte(secret))
	logger := &dispatchCleanupLogger{}
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(logger)
	env := suite.NewTestActivityEnvironment()
	acts := &Activities{Scrubber: scrubber, Surrenders: surrenderStore(t), Dispatcher: &fakeStageDispatcher{
		report: dispatcher.Report{Pod: "pod", Disposed: true, DisposeErr: errors.New(secret + strings.Repeat("x", 4096))},
		err:    context.Canceled,
	}}
	env.RegisterActivity(acts)
	if _, err := env.ExecuteActivity(acts.DispatchStage, dispatchInput("cleanup-log", "build", 1)); err == nil {
		t.Fatal("dispatch cancellation disappeared")
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if logger.diagnostic == "" || len(logger.diagnostic) > 2048 || strings.Contains(logger.diagnostic, secret) || !logger.accepted {
		t.Fatal("cleanup warning was absent, unbounded, unsanitized or lost DELETE acceptance")
	}
}
