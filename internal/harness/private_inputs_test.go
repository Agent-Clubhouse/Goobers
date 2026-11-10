package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestPrivateInputReachesAgentButNotTheRecord(t *testing.T) {
	const secret = "please hide the clock widget"
	registry, scrubber := journal.DefaultScrubber()
	var read []byte
	var prompt string
	var gotPath string
	adapter := &FakeAdapter{
		Transcript: []byte(`{"role":"assistant","content":"working on ` + secret + `"}`),
		Stderr:     []byte("echo: " + secret),
		Act: func(_ context.Context, req RunRequest) error {
			gotPath = req.PrivatePaths["request.txt"]
			data, err := os.ReadFile(filepath.Join(req.Workspace, gotPath))
			if err != nil {
				return err
			}
			read, prompt = data, renderPrompt(req)
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	rec := &fakeRecorder{dir: t.TempDir()}
	source := func(_ context.Context, env apiv1.InvocationEnvelope) ([]PrivateInput, error) {
		return []PrivateInput{{Name: "request.txt", Data: []byte(secret), Sensitive: [][]byte{[]byte(secret)}}}, nil
	}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, scrubber, "",
		WithPrivateInputs(source, registry))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if _, err := exec.Invoke(context.Background(), testEnvelope(workspace)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(read) != secret {
		t.Fatalf("agent read %q, want the private input", read)
	}
	if !strings.Contains(prompt, "## Private inputs") || !strings.Contains(prompt, gotPath) || strings.Contains(prompt, secret) {
		t.Fatalf("prompt must list the private file without its content:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(workspace, gotPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private input left in the workspace: %v", err)
	}
	if len(rec.spans) == 0 {
		t.Fatal("no transcript recorded")
	}
	for _, span := range rec.spans {
		if strings.Contains(string(span.data), secret) {
			t.Fatalf("span %s carries the private input: %s", span.name, span.data)
		}
	}
	for _, artifact := range rec.artifacts {
		if strings.Contains(string(artifact.data), secret) {
			t.Fatalf("artifact %s carries the private input", artifact.name)
		}
	}
}

func TestPrivateInputFailsClosed(t *testing.T) {
	ok := func(context.Context, apiv1.InvocationEnvelope) ([]PrivateInput, error) {
		return []PrivateInput{{Name: "request.txt", Data: []byte("x"), Sensitive: [][]byte{[]byte("hidden value")}}}, nil
	}
	failing := func(context.Context, apiv1.InvocationEnvelope) ([]PrivateInput, error) {
		return nil, errors.New("no bound request")
	}
	registry, _ := journal.DefaultScrubber()
	for name, opt := range map[string]Option{
		"source error": WithPrivateInputs(failing, registry),
		"no registrar": WithPrivateInputs(ok, nil),
	} {
		t.Run(name, func(t *testing.T) {
			acted := false
			adapter := &FakeAdapter{Act: func(context.Context, RunRequest) error { acted = true; return nil }}
			rec := &fakeRecorder{dir: t.TempDir()}
			exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "", opt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir())); err == nil || acted {
				t.Fatalf("Invoke err = %v, agent ran = %v; want a refusal before the agent runs", err, acted)
			}
		})
	}
}
