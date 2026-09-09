package executor

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestModuleDownloadDiagnosticPreservesScrubbedCause(t *testing.T) {
	const secret = "module-registry-test-credential"
	injector := newTestInjector(t, "test:cap", "GOOBERS_TEST_MODULE_TOKEN", secret)
	exec, rec := newTestExecutor(t, injector)
	env := baseEnvelope(t)
	env.Capabilities = []string{"test:cap"}
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `printf 'go: example.com/mod@v1.2.3: reading https://modules.example.com/mod.zip: 401 Unauthorized (credential %s)\n' "$GOOBERS_CRED_TEST_CAP"
printf 'make: *** [Makefile:12: ci] Error 1\n' >&2
exit 1`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || result.Error.Code != "nonzero_exit" {
		t.Fatalf("error = %+v, want original command exit error", result.Error)
	}
	for _, want := range []string{"401 Unauthorized", "credentials and repository access", journal.Redacted} {
		if !strings.Contains(result.Error.Message, want) || !strings.Contains(result.Summary, want) {
			t.Fatalf("result = %+v, want scrubbed cause and guidance containing %q", result, want)
		}
	}
	if strings.Contains(result.Error.Message, secret) || strings.Contains(result.Summary, secret) {
		t.Fatal("credential leaked into failure diagnostic")
	}
	for name, data := range rec.recorded {
		if strings.Contains(string(data), secret) {
			t.Fatalf("credential leaked into artifact %s", name)
		}
	}
	path := result.Outputs[outputFailureArtifact].(string)
	start := int(result.Outputs[outputFailureStartByte].(float64))
	end := int(result.Outputs[outputFailureEndByte].(float64))
	evidence := string(rec.recorded[path][start:end])
	if !strings.Contains(evidence, "401 Unauthorized") || !strings.Contains(evidence, journal.Redacted) {
		t.Fatalf("failure artifact range = %q, want original scrubbed cause", evidence)
	}
	if strings.Contains(evidence, "hint:") || strings.Contains(evidence, "make:") {
		t.Fatalf("failure artifact range = %q, want source output only", evidence)
	}
}
