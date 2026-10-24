package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

const cloudReferenceDir = "../../deploy/reference/goobers-cloud"

// substituteEnv expands the $(VAR) references Kubernetes resolves in args from
// the container's own env, using placeholder values (the files and URLs are
// never read: the role stops at the first unreadable credential file).
var envRef = regexp.MustCompile(`\$\(([A-Z_]+)\)`)

// TestReferenceControlArgsParseWithRealFlagSet runs the control Deployment's
// exact args through the control role's real flag parsing, so an argument the
// role does not define (or a misspelled one) fails here instead of in a
// crash-looping pod. A usage/parse failure exits 2 with "flag provided but not
// defined" or "invalid value"; a clean parse proceeds to the credential files,
// which do not exist in this test, and fails there without touching a network.
func TestReferenceControlArgsParseWithRealFlagSet(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(cloudReferenceDir, "workload", "control-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.UnmarshalStrict(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if len(container.Args) == 0 || container.Args[0] != "control" {
		t.Fatalf("control container args must start with the role name, got %v", container.Args)
	}
	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = "value"
	}
	args := make([]string, 0, len(container.Args))
	for _, a := range container.Args[1:] {
		args = append(args, envRef.ReplaceAllStringFunc(a, func(ref string) string {
			name := strings.TrimSuffix(strings.TrimPrefix(ref, "$("), ")")
			if _, ok := env[name]; !ok {
				t.Fatalf("arg %q references $(%s), which the container does not define", a, name)
			}
			return "password"
		}))
	}
	var stdout, stderr bytes.Buffer
	code := runControlRole(args, &stdout, &stderr)
	output := stderr.String() + stdout.String()
	for _, parseFailure := range []string{"flag provided but not defined", "invalid value", "invalid argument", "Usage: goobers-cloud control"} {
		if strings.Contains(output, parseFailure) {
			t.Fatalf("control role rejected the reference Deployment's args (exit %d): %s", code, output)
		}
	}
}
