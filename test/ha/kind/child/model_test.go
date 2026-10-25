package child

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// The model substitute must actually inspect the materialized receipt. A
// successful parent completion without a terminal receipt would fake the
// happy-path evidence even when the production context resolver was broken.
func TestModelConsumesOnlyCompletedReceipt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the test-only model")
	}
	script, err := filepath.Abs("fixture/model.cjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"completed", "running", "failed"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".goobers", "context")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"runId": "child", "input": map[string]string{"state": state}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "00-child-result"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), node, script)
			cmd.Dir = root
			cmd.Stdin = strings.NewReader("KIND_CHILD_HAPPY\nwrite your completion as JSON to `result.json`\n")
			output, runErr := cmd.CombinedOutput()
			if state != "completed" {
				if runErr == nil {
					t.Fatalf("accepted nonterminal receipt: %s", output)
				}
				if _, err := os.Stat(filepath.Join(root, "result.json")); !os.IsNotExist(err) {
					t.Fatal("model wrote successful completion for rejected receipt")
				}
				return
			}
			if runErr != nil {
				t.Fatalf("model: %v: %s", runErr, output)
			}
			completion, err := os.ReadFile(filepath.Join(root, "result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var got apiv1.ResultEnvelope
			if err := json.Unmarshal(completion, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != apiv1.ResultSuccess || got.Summary != "kind parent consumed child receipt" {
				t.Fatalf("completion=%s", completion)
			}
		})
	}
}
