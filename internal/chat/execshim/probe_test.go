package execshim

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/goobers/goobers/internal/harness"
)

type fixtureCodexProcess struct{}

func (fixtureCodexProcess) Run(_ context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	output := "codex-cli 0.0.0\n"
	if strings.Contains(strings.Join(req.Command, " "), "login status") {
		output = "Logged in using ChatGPT\n"
	}
	return harness.ProcessResult{Transcript: []byte(output)}, nil
}
func TestProbeUsesPinnedAmbientCodexAuthentication(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adapter := &harness.CodexAdapter{Command: []string{binary}, Runner: fixtureCodexProcess{}}
	options := map[string]apiextensionsv1.JSON{"auth": {Raw: []byte(`"ambient-chatgpt"`)}, "allowFileBackedCredentials": {Raw: []byte(`true`)}}
	if _, err := adapter.PreflightConfig(t.Context(), "", options); err != nil {
		t.Fatalf("control preflight: %v", err)
	}
	d := &Driver{Adapter: adapter, Request: harness.RunRequest{HarnessOptions: options}}
	p, err := d.Probe(t.Context())
	if err != nil || !p.HandshakeOK {
		t.Fatalf("valid pinned ambient-login mode rejected by registry probe: %v", err)
	}
}
