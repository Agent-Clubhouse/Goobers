package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const claudeStdinReceiverSentinel = "claude-stdin-receiver"

// claudeStdinFixtures are goal payloads that defeat argv transport: sizes
// at and beyond the cmd.exe (8,191) and CreateProcess (32,767) limits, plus
// content a shell or shim would split, re-encode, or reinterpret.
func claudeStdinFixtures() map[string]string {
	return map[string]string{
		"ascii-8192":   strings.Repeat("a", 8192),
		"ascii-32768":  strings.Repeat("b", 32768),
		"ascii-131072": strings.Repeat("c", 131072),
		"mixed": "---\nrole: curator\n---\n" +
			`{"nested": {"quote": "say \"hi\"", "path": "C:\\Users\\x\\"}}` + "\r\n" +
			"BMP: é 漢字 — supplementary: 😀 𝄞\r\nLF only\n$input `$(whoami)` %PATH% ^& | < >\n",
	}
}

// TestClaudeAdapterSendsPromptsOnStdinNotArgv pins #6871: the initial and
// completion-repair prompts travel on stdin in full, and argv stays bounded
// control metadata regardless of prompt size or content.
func TestClaudeAdapterSendsPromptsOnStdinNotArgv(t *testing.T) {
	stubClaudeCredentialsHome(t)
	for name, payload := range claudeStdinFixtures() {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			runner := &claudeSequenceRunner{
				results: []ProcessResult{
					{ExitCode: 0, Transcript: []byte(claudeResultStream)},
					{ExitCode: 0, Transcript: []byte(claudeResultStream)},
				},
				acts: []func(ProcessRequest) error{nil, writeSuccessCompletion},
			}
			adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
			envelope := testEnvelope(workspace)
			envelope.Goal = payload
			if _, err := adapter.Run(context.Background(), RunRequest{
				Envelope: envelope, Workspace: workspace, CompletionPath: DefaultResultPath, Timeout: time.Minute,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(runner.reqs) != 2 {
				t.Fatalf("process calls = %d, want initial plus repair", len(runner.reqs))
			}
			rendered, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
			if err != nil {
				t.Fatal(err)
			}
			if string(runner.reqs[0].Stdin) != string(rendered) || !strings.Contains(string(rendered), payload) {
				t.Fatalf("initial stdin (%d bytes) is not the full rendered prompt (%d bytes)", len(runner.reqs[0].Stdin), len(rendered))
			}
			if !strings.Contains(string(runner.reqs[1].Stdin), "previous turn ended without writing") {
				t.Fatalf("repair stdin = %q, want the completion-repair prompt", runner.reqs[1].Stdin)
			}
			for i, req := range runner.reqs {
				assertClaudeArgvCarriesNoPrompt(t, req.Command, string(req.Stdin))
				if i == 1 && commandOptionValue(req.Command, "--resume") != commandOptionValue(runner.reqs[0].Command, "--session-id") {
					t.Fatalf("repair did not resume the initial session: %v", req.Command)
				}
			}
		})
	}
}

func assertClaudeArgvCarriesNoPrompt(t *testing.T, argv []string, prompt string) {
	t.Helper()
	// cmd.exe's 8,191-character limit is the tightest launcher bound.
	if serialized := strings.Join(argv, " "); len(serialized) >= 8191 {
		t.Fatalf("serialized argv is %d characters, want under the cmd.exe limit", len(serialized))
	}
	for _, arg := range argv {
		if len(arg) > 64 && strings.Contains(prompt, arg) || strings.Contains(arg, "write your result") {
			t.Fatalf("argv carries prompt content: %q", arg)
		}
	}
}

// TestClaudeAdapterStdinReachesRealProcess launches a real receiver process
// (on Windows, a real CreateProcess) with a long executable/workspace path
// and asserts the receiver read exactly the rendered prompt — by length and
// digest — for both the initial and the resumed completion-repair turn.
func TestClaudeAdapterStdinReachesRealProcess(t *testing.T) {
	stubClaudeCredentialsHome(t)
	for name, payload := range claudeStdinFixtures() {
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), strings.Repeat("long-workspace-segment-", 6))
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			adapter := &ClaudeAdapter{
				Command: []string{os.Args[0], "-test.run=^TestClaudeStdinReceiverHelper$", "--", claudeStdinReceiverSentinel},
				Runner:  ExecProcessRunner{},
			}
			envelope := testEnvelope(workspace)
			envelope.Goal = payload
			if _, err := adapter.Run(context.Background(), RunRequest{
				Envelope: envelope, Workspace: workspace, CompletionPath: DefaultResultPath, Timeout: time.Minute,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			rendered, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := readStdinReceipt(t, workspace, "initial"), stdinDigest(rendered); got != want {
				t.Fatalf("initial receiver got %s, want %s", got, want)
			}
			repair := readStdinReceipt(t, workspace, "resume")
			if repair == stdinDigest(nil) || repair == stdinDigest(rendered) {
				t.Fatalf("resumed receiver did not get the repair prompt: %s", repair)
			}
		})
	}
}

// TestClaudeStdinReceiverHelper is the receiver process for
// TestClaudeAdapterStdinReachesRealProcess; it is inert unless launched with
// the receiver sentinel argument.
func TestClaudeStdinReceiverHelper(t *testing.T) {
	if !slices.Contains(os.Args, claudeStdinReceiverSentinel) {
		return
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	turn := "initial"
	if slices.Contains(os.Args, "--resume") {
		turn = "resume"
		if err := WriteCompletion(".", DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if err := os.WriteFile(filepath.Join(".goobers", "stdin-"+turn+".txt"), []byte(stdinDigest(data)), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Print(claudeResultStream)
	os.Exit(0)
}

func stdinDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%d:%s", len(data), hex.EncodeToString(sum[:]))
}

func readStdinReceipt(t *testing.T, workspace, turn string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, ".goobers", "stdin-"+turn+".txt"))
	if err != nil {
		t.Fatalf("receiver did not record the %s turn: %v", turn, err)
	}
	return string(data)
}
