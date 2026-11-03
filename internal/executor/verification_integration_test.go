//go:build integration

package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationVerificationCheckerPublication(t *testing.T) {
	command := []string{"sh", "-c", "cat report.json"}
	failureCommand := []string{"sh", "-c", "cat report.json; exit 7"}
	if runtime.GOOS == "windows" {
		testdep.Require(t, "powershell.exe")
		command = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Get-Content -Raw -LiteralPath report.json"}
		failureCommand = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Get-Content -Raw -LiteralPath report.json; exit 7"}
	} else {
		testdep.Require(t, "sh", "cat")
	}
	for _, test := range []struct {
		name    string
		command []string
		enabled bool
		want    int
	}{
		{name: "opted in", command: command, enabled: true, want: 1},
		{name: "not opted in", command: command},
		{name: "failed process", command: failureCommand, enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, _ := newPortableTestExecutor(t, nil)
			jr, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "verification", Workflow: "verify"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = jr.Close() }()
			executor.Journal = jr
			env := baseEnvelope(t)
			env.Attempt = 1
			env.Inputs = map[string]any{InputVerificationReport: test.enabled}
			report := executorVerificationReport()
			for name, field := range map[string]*string{
				"subject": &report.Subject, "specification": &report.Specification, "context": &report.Context, "certificate": &report.Evidence[0],
			} {
				ref, err := jr.RecordArtifact(name, []byte(name))
				if err != nil {
					t.Fatal(err)
				}
				*field = ref.Digest
			}
			data, _ := json.Marshal(report)
			if err := os.WriteFile(filepath.Join(env.Workspace, "report.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: env.TaskID, Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			result, err := executor.Run(context.Background(), env, apiv1.DeterministicRun{Command: test.command})
			if err != nil {
				t.Fatal(err)
			}
			if test.name != "failed process" && result.Status != apiv1.ResultSuccess {
				t.Fatalf("checker command failed: %+v", result)
			}
			if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: env.TaskID, Attempt: 1, Status: string(result.Status)}); err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenRead(jr.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, event := range events {
				if event.Runner["kind"] == creditgraph.VerificationAnnotation {
					found++
					if event.Runner["source"] != result.Artifacts[0].Digest {
						t.Fatal("report was not bound to captured stdout")
					}
				}
			}
			if found != test.want {
				t.Fatalf("published %d verification records, want %d", found, test.want)
			}
			graph, err := creditgraph.Build(creditgraph.Input{RunID: "verification", Events: events})
			if err != nil || len(graph.Verifications) != test.want || len(graph.UnverifiedVerifications) != 0 {
				t.Fatalf("live journal verification evidence = %+v, %v", graph, err)
			}
		})
	}
}
