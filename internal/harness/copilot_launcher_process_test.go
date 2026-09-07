package harness

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestLauncherProcessFixture(t *testing.T) {
	if !slices.Contains(os.Args, "goobers-launcher-fixture") {
		return
	}
	if slices.Contains(os.Args, launcherContractFlag) {
		fmt.Fprintln(os.Stderr, "fixture launcher diagnostic: this is not protocol stdout")
		fmt.Println(`{"version":1,"sessionMode":"wrapper-managed"}`)
		os.Exit(0)
	}
	if slices.Contains(os.Args, "--version") {
		fmt.Println("fixture-wrapper 1.0")
		os.Exit(0)
	}
	if copilotCommandSelectsSession(os.Args) {
		fmt.Fprintln(os.Stderr, "wrapper received an adapter-owned session selector")
		os.Exit(2)
	}
	path := os.Getenv("GOOBERS_SESSION_TRANSCRIPT")
	if path == "" {
		fmt.Fprintln(os.Stderr, "wrapper export contract was not supplied")
		os.Exit(2)
	}
	if err := os.WriteFile(path, []byte(`{"type":"assistant.message","data":{"messageId":"native","content":"real subprocess wrapper capture"}}`+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := WriteCompletion(".", DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestLauncherRealSubprocessPreflightAndNativeCapture(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adapter := &CopilotAdapter{
		Command:                 []string{program, "-test.run=^TestLauncherProcessFixture$", "--", "goobers-launcher-fixture"},
		RequireLauncherContract: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if info, err := adapter.Preflight(ctx); err != nil || info.Version != "fixture-wrapper 1.0" {
		t.Fatalf("real launcher preflight: %+v %v", info, err)
	}
	workspace := t.TempDir()
	out, err := adapter.Run(ctx, RunRequest{Workspace: workspace, Envelope: testEnvelope(workspace), CompletionPath: DefaultResultPath})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Transcript), "real subprocess wrapper capture") {
		t.Fatalf("real launcher transcript missing: %s", out.Transcript)
	}
}
