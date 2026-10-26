package executor

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestDSLVersionCannotBeAuthoredOrInherited(t *testing.T) {
	for _, key := range []string{DSLVersionEnvVar, strings.ToLower(DSLVersionEnvVar)} {
		_, _, err := buildStageEnv(t.Context(), nil, nil, nil, "run", "g", "wf", "", "", "", true, nil, map[string]string{key: "2.0"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), DSLVersionEnvVar) {
			t.Fatalf("override %s: %v", key, err)
		}
	}
	t.Setenv(DSLVersionEnvVar, "2.0")
	base, _, err := buildStageEnv(t.Context(), nil, nil, nil, "run", "g", "wf", "", "", "", true, nil, nil, []string{DSLVersionEnvVar}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range base {
		if strings.HasPrefix(kv, DSLVersionEnvVar+"=") {
			t.Fatalf("inherited %s", kv)
		}
	}
	for _, version := range []string{"", "2.0", "2.1", "3.0", "3.1"} {
		stage := (&ShellExecutor{}).runContextEnv(t.Context(), apiv1.InvocationEnvelope{RunID: "run", TaskID: "run:watch", DSLVersion: version})
		count := 0
		for _, kv := range stage {
			if strings.HasPrefix(kv, DSLVersionEnvVar+"=") {
				count++
				if kv != DSLVersionEnvVar+"="+version {
					t.Fatalf("stamp %q", kv)
				}
			}
		}
		if count != 1 {
			t.Fatalf("version %q has %d stamps", version, count)
		}
	}
}
