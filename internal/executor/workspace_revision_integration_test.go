//go:build integration

package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationShellWorkspaceRevisionNormalization(t *testing.T) {
	testdep.Require(t, "sh")
	valid := `{"repository":{"provider":"github","owner":"org","name":"repo"},"commitSha":"` + strings.Repeat("a", 40) + `"}`
	for _, tc := range []struct {
		name, control, extra string
		exit                 int
		status               apiv1.ResultStatus
	}{
		{"success", valid, "", 0, apiv1.ResultSuccess},
		{"failure", valid, "", 3, apiv1.ResultFailure},
		{"no-work", valid, `,"noWork":true`, 0, apiv1.ResultNoWork},
		{"null-success", "null", "", 0, apiv1.ResultFailure},
		{"null-failure", "null", "", 3, apiv1.ResultFailure},
		{"null-no-work", "null", `,"noWork":true`, 0, apiv1.ResultFailure},
		{"nested-null", strings.Replace(valid, `"name":"repo"`, `"name":"repo","id":null`, 1), "", 0, apiv1.ResultFailure},
		{"duplicate", strings.TrimSuffix(valid, "}") + `,"sourceRef":"one","sourceRef":"two"}`, "", 0, apiv1.ResultFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, _ := newPortableTestExecutor(t, nil)
			env := baseEnvelope(t)
			env.Inputs = map[string]interface{}{InputResultFile: "out.json"}
			data := `{"legacy":"kept","workspaceRevision":` + tc.control + tc.extra + `}`
			script := fmt.Sprintf("printf '%%s' '%s' > out.json\nexit %d\n", data, tc.exit)
			if err := os.WriteFile(filepath.Join(env.Workspace, "stage.sh"), []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := executor.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "stage.sh"}})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.status || (result.WorkspaceRevision != nil) != (tc.status == apiv1.ResultSuccess) {
				t.Fatalf("wrong normalized result: %+v", result)
			}
			if result.Metrics["exitCode"] != float64(tc.exit) || len(result.Artifacts) == 0 {
				t.Fatalf("command diagnostics lost: %+v", result)
			}
			if tc.control != valid && (result.Error == nil || result.Error.Code != "workspace_revision_invalid" || result.Error.Retryable) {
				t.Fatalf("malformed control lost permanent code: %+v", result)
			}
		})
	}
}
