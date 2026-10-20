//go:build !windows

package interactiveexec

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/sandbox"
)

func TestRegistrySelectsNativeCodexWithHostProbeState(t *testing.T) {
	base, err := sandbox.New()
	if errors.Is(err, sandbox.ErrUnavailable) || errors.Is(err, sandbox.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	e, _ := bridgeExecutor(t)
	e.config.Sandbox = base
	e.runNative = nil
	e.chatRegistry = nil
	workspace, _ := e.workspace("codex-registry")
	if err := os.MkdirAll(filepath.Join(workspace, ".tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	state := chatStatePath(e.config.StateDir, workspace)
	if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run='^TestRegistryCodexChild$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	boundary, err := e.nativeSandbox()
	if err != nil {
		t.Fatal(err)
	}
	boundary, err = e.sessionSandbox(boundary, workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	request := harness.RunRequest{Workspace: workspace, Model: "fixture-model", Sandbox: boundary, IsolateHome: true,
		HarnessOptions: map[string]apiextensionsv1.JSON{"auth": {Raw: []byte(`"ambient-chatgpt"`)}}}
	registry := e.driverRegistry("codex", request)
	session, attempts, err := registry.Start(t.Context(), "codex", chat.StartOptions{Workspace: workspace, Level: chat.Auto, Ceiling: chat.NewCeiling(chat.Auto, chat.Auto, chat.Auto)})
	if err != nil {
		t.Fatalf("native registry startup: %v; attempts %+v", err, attempts)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Driver != "codex_app_server" || session.NativeID() != "thread-fixture" {
		t.Fatalf("native probe unexpectedly fell back: %+v, %s", attempts, session.NativeID())
	}
}

func TestRegistryCodexChild(t *testing.T) {
	if !slices.Contains(os.Args, "app-server") {
		t.Skip("native child fixture")
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"userAgent": "codex-cli fixture"}
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "chatgpt"}}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"id": "fixture-model", "model": "fixture-model", "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium"}}}}}
		case "thread/start":
			result = map[string]any{"thread": map[string]any{"id": "thread-fixture"}, "model": "fixture-model"}
		default:
			os.Exit(3)
		}
		if encoder.Encode(map[string]any{"id": request.ID, "result": result}) != nil {
			os.Exit(4)
		}
	}
	os.Exit(0)
}
