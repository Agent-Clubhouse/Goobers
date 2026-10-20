package interactiveexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/invoke"
)

func TestSnapshotExcludesCredentialsAndRejectsSymlinks(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"invoice.mjs": "safe", ".env": "TOKEN=do-not-copy", ".npmrc": "private", "auth.json": "secret", "client.pem": "secret", ".gitignore": "build/\n"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := copySource(source, dest); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", ".npmrc", "auth.json", "client.pem"} {
		if _, err := os.Stat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Errorf("credential-shaped source copied: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "invoice.mjs"), filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copySource(source, t.TempDir()); err == nil {
		t.Fatal("snapshot accepted a symlink")
	}
}

func TestNativeUnconfirmedInterruptionNeverAcknowledgesOrRestarts(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".goobers"), 0700); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	calls := 0
	e := &Executor{runNative: func(ctx context.Context, _ harness.RunRequest) (harness.Outcome, error) {
		calls++
		ack := invoke.RegisterWorkspaceWriter(ctx)
		close(started)
		<-ctx.Done()
		ack(errors.New("simulated missing descendant custody"))
		return harness.Outcome{}, harness.ErrCanceled
	}}
	steering := make(chan interactive.Steering, 1)
	acknowledged := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		_, err := e.native(context.Background(), interactive.ExecutionRequest{Steering: steering}, workspace, "", func(string, map[string]any) error { return nil })
		done <- err
	}()
	<-started
	steering <- interactive.Steering{Text: "continue", Ack: func(err error) { acknowledged <- err }}
	select {
	case err := <-done:
		if !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
			t.Fatalf("lost custody error %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interruption did not return")
	}
	if err := <-acknowledged; err == nil {
		t.Fatal("unconfirmed interruption acknowledged")
	}
	if calls != 1 {
		t.Fatalf("restarted without custody %d", calls)
	}
}

func TestLaunchRefusalDoesNotInventUnknownWriter(t *testing.T) {
	_, proof := invoke.WithWorkspaceQuiescence(context.Background())
	refusal := errors.New("binary absent")
	if got := verifyWriters(proof, refusal); !errors.Is(got, refusal) {
		t.Fatalf("launch refusal converted to custody uncertainty: %v", got)
	}
	if err := verifyWriters(proof, nil); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatal("completion without a tracked writer trusted")
	}
}

func TestFencedToolEventRetainsUnconfirmedProcessCustody(t *testing.T) {
	e := &Executor{runProcess: func(context.Context, string, []string, func(harness.TranscriptDelta) error) (harness.ProcessResult, error) {
		return harness.ProcessResult{}, invoke.ErrWorkspaceNotQuiescent
	}}
	fenced := errors.New("stale_fence")
	_, err := e.local(context.Background(), interactive.ExecutionRequest{ChildID: "review"}, t.TempDir(), "", func(kind string, _ map[string]any) error {
		if kind == "tool.completed" {
			return fenced
		}
		return nil
	})
	if !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) || !errors.Is(err, fenced) {
		t.Fatalf("lost process or event error: %v", err)
	}
}

func TestPreparationFailureRetainsWriterCustodyAndBlocksReuse(t *testing.T) {
	principal, err := auth.LocalPrincipal("owner")
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{StateDir: t.TempDir(), SourceDir: t.TempDir(), Mode: "local-tools", Principal: principal})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.runProcess = func(context.Context, string, []string, func(harness.TranscriptDelta) error) (harness.ProcessResult, error) {
		calls++
		return harness.ProcessResult{}, invoke.ErrWorkspaceNotQuiescent
	}
	if _, err := e.PrepareWorkspace(context.Background(), "owner", "prepare-session"); err == nil || calls != 0 {
		t.Fatal("Helper lookup initialized a workspace outside a leased turn")
	}
	req := interactive.ExecutionRequest{SessionID: "prepare-session", Owner: "owner"}
	result, err := e.Execute(context.Background(), req, func(interactive.ExecutionEvent) error { return nil })
	if !result.CustodyPending || !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatalf("preparation custody lost: %+v %v", result, err)
	}
	if err := e.Recover(context.Background(), req); err == nil {
		t.Fatal("preparation writer marker not retained")
	}
	if _, err := e.prepare(context.Background(), req); !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) || calls != 1 {
		t.Fatalf("unconfirmed preparation reused: calls=%d err=%v", calls, err)
	}
}

func TestReadWorkspaceDeniesPrivateAndEscapingPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".env", "../private", "/etc/passwd"} {
		if _, err := readWorkspace(root, path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}

func TestNativeStreamOmitsReasoningAndUnknownRecords(t *testing.T) {
	var output []string
	stream := &nativeStream{emit: func(kind string, data map[string]any) error {
		output = append(output, kind+":"+data["text"].(string))
		return nil
	}}
	first := `{"type":"item.completed","item":{"type":"reasoning","text":"private reasoning"}}` + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}` + "\n"
	for _, part := range []string{first[:15], first[15:]} {
		if err := stream.write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(output, ",") != "assistant.message:done" {
		t.Fatalf("unexpected projection %v", output)
	}
}

func TestCrashCustodyCannotBeClaimedByWorkspaceMarker(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "metadata"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{config: Config{StateDir: state}}
	req := interactive.ExecutionRequest{SessionID: "session", ChildID: "child"}
	marker := filepath.Join(state, "metadata", "session-child.writer-active")
	if err := os.WriteFile(marker, []byte("unknown writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(context.Background(), req); err == nil {
		t.Fatal("unknown child writer custody reported recovered")
	}
}
