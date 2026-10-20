package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const interactiveGagglePath = "gaggles/interactive/gaggle.yaml"

func interactiveSample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../samples/interactive-config")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func editInteractiveSample(t *testing.T, dir, name string, edit func(string) string) {
	t.Helper()
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(edit(string(b))), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveConfigNormalValidation(t *testing.T) {
	r, err := newV(t).ValidateDir(interactiveSample(t))
	if err != nil || r.HasErrors() {
		t.Fatalf("sample invalid: %v %+v", err, r)
	}
}

func TestInteractiveSchemaClosedAndBounded(t *testing.T) {
	b, err := os.ReadFile("../../samples/interactive-config/" + interactiveGagglePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		edit      func(map[string]any)
		wantError bool
	}{
		{"valid", func(map[string]any) {}, false},
		{"legacy", func(d map[string]any) { delete(d, "dslVersion"); delete(d["spec"].(map[string]any), "interactive") }, false},
		{"empty", func(d map[string]any) { d["spec"].(map[string]any)["interactive"] = map[string]any{} }, false},
		{"missing pin", func(d map[string]any) { delete(d, "dslVersion") }, true},
		{"wrong pin", func(d map[string]any) { d["dslVersion"] = "2.0" }, true},
		{"credentials forbidden", func(d map[string]any) { interactiveObject(d)["credentials"] = "token" }, true},
		{"unknown tool", func(d map[string]any) { interactiveFirstSurface(d)["tools"] = []string{"arbitrary_shell"} }, true},
		{"unknown mode", func(d map[string]any) { interactiveFirstSurface(d)["executionModes"] = []string{"unrestricted"} }, true},
		{"default unavailable", func(d map[string]any) { interactiveFirstSurface(d)["executionModes"] = []string{"local-tools"} }, true},
		{"duplicate tool", func(d map[string]any) {
			interactiveFirstSurface(d)["tools"] = []string{"session_context", "session_context"}
		}, true},
		{"unbounded child", func(d map[string]any) { interactiveFirstSurface(d)["session"].(map[string]any)["maxChildren"] = 99 }, true},
		{"zero lease", func(d map[string]any) { interactiveFirstSurface(d)["session"].(map[string]any)["leaseSeconds"] = 0 }, true},
		{"secret broker syntax", func(d map[string]any) {
			interactiveObject(d)["authorities"].([]any)[0].(map[string]any)["brokerRef"] = "https://user:secret@host"
		}, true},
		{"unknown provider", func(d map[string]any) {
			interactiveObject(d)["authorities"].([]any)[0].(map[string]any)["provider"] = "generic-cloud"
		}, true},
		{"unknown child command", func(d map[string]any) {
			interactiveFirstSurface(d)["childWorkflows"].(map[string]any)["commandCapabilities"] = []string{"shell"}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d map[string]any
			if err := yaml.Unmarshal(b, &d); err != nil {
				t.Fatal(err)
			}
			tc.edit(d)
			data, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			err = newV(t).ValidateJSON("gaggle.schema.json", data)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}
func interactiveObject(d map[string]any) map[string]any {
	return d["spec"].(map[string]any)["interactive"].(map[string]any)
}
func interactiveFirstSurface(d map[string]any) map[string]any {
	return interactiveObject(d)["surfaces"].([]any)[0].(map[string]any)
}

func TestInteractiveCrossReferencesAndPreview(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"missing Goober", "gooberRef: helper", "gooberRef: absent", "missing Goober/absent"},
		{"missing workflow", "workflows: [invoice-check]", "workflows: [absent]", "missing Workflow/absent"},
		{"missing authority", "authorityRefs: [ado-context, telemetry-context]", "authorityRefs: [missing]", "undeclared local authority"},
		{"duplicate surface", "name: workbench", "name: helper", "surfaces repeats name"},
		{"duplicate authority", "name: telemetry-context", "name: ado-context", "authorities repeats name"},
		{"owner exceeds global", "maxActivePerOwner: 2", "maxActivePerOwner: 5", "maxActivePerOwner must not exceed"},
		{"lease exceeds turn", "maxTurnSeconds: 600", "maxTurnSeconds: 10", "leaseSeconds must not exceed"},
		{"own preview acknowledgement", "goobers.dev/allow-preview-features: \"true\"", "goobers.dev/allow-preview-features: \"false\"", "gaggle.spec.interactive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := interactiveSample(t)
			editInteractiveSample(t, dir, interactiveGagglePath, func(s string) string { return strings.ReplaceAll(s, tc.from, tc.to) })
			r, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, issue := range r.Issues {
				if issue.Severity == Error && strings.Contains(issue.Message, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing error %q: %+v", tc.want, r.Issues)
			}
		})
	}
}

func TestInteractiveVersionIsIndependentFromWorkflowPins(t *testing.T) {
	for _, version := range []string{"2.0", "3.0", "none"} {
		t.Run(version, func(t *testing.T) {
			dir := interactiveSample(t)
			path := "gaggles/interactive/workflows/invoice-check.yaml"
			if version == "none" {
				if err := os.Remove(filepath.Join(dir, path)); err != nil {
					t.Fatal(err)
				}
				editInteractiveSample(t, dir, interactiveGagglePath, func(s string) string { return strings.ReplaceAll(s, "        workflows: [invoice-check]\n", "") })
			} else {
				editInteractiveSample(t, dir, path, func(s string) string {
					s = strings.Replace(s, `dslVersion: "3.0"`, `dslVersion: "`+version+`"`, 1)
					if version == "2.0" {
						s = strings.Replace(s, "      runsOn: {cpu: 250m, memory: 128Mi, restrictions: [network:none]}\n", "", 1)
					}
					return s
				})
			}
			r, err := newV(t).ValidateDir(dir)
			if err != nil || r.HasErrors() {
				t.Fatalf("independent pin rejected: %v %+v", err, r)
			}
		})
	}
	// Gaggle acknowledgement never authorizes the Workflow's own preview pin.
	dir := interactiveSample(t)
	editInteractiveSample(t, dir, "gaggles/interactive/workflows/invoice-check.yaml", func(s string) string {
		return strings.Replace(s, "    goobers.dev/allow-preview-features: \"true\"\n", "", 1)
	})
	r, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasErrors() {
		t.Fatal("Gaggle acknowledgement authorized Workflow preview")
	}
}

func TestInteractiveCannotResolveForeignGooberOrWorkflow(t *testing.T) {
	for _, kind := range []string{"Goober", "Workflow"} {
		t.Run(kind, func(t *testing.T) {
			dir := interactiveSample(t)
			other := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata: {name: other}
spec:
  project: {provider: github, owner: acme, name: other}
  backlog: {provider: github, project: acme/other}
  isolation: {namespace: other}
`
			if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte(other), 0600); err != nil {
				t.Fatal(err)
			}
			path := "gaggles/interactive/goobers/helper/goober.yaml"
			want := "owned by another gaggle"
			if kind == "Workflow" {
				path = "gaggles/interactive/workflows/invoice-check.yaml"
				want = "missing Workflow/invoice-check in gaggle"
			}
			editInteractiveSample(t, dir, path, func(s string) string { return strings.Replace(s, "gaggle: interactive", "gaggle: other", 1) })
			r, err := newV(t).ValidateDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, i := range r.Issues {
				if i.Severity == Error && strings.Contains(i.Message, want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("foreign %s resolved: %+v", kind, r.Issues)
			}
		})
	}
}
