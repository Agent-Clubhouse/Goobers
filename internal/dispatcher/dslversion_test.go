package dispatcher

import (
	"strings"
	"testing"
)

func TestPodDSLVersionIsPinnedAndLegacyShadowsImage(t *testing.T) {
	for _, version := range []string{"", "2.0", "2.1", "3.0", "3.1"} {
		attempt := testAttempt()
		attempt.CLIStage, attempt.DSLVersion = true, version
		env := stageEnv(testConfig(), attempt, nil, nil)
		count := 0
		for _, item := range env {
			if item.Name == executorDSLVersionEnv {
				count++
				if item.Value != version {
					t.Fatalf("stamp %q want %q", item.Value, version)
				}
			}
		}
		if count != 1 {
			t.Fatalf("version %q has %d stamps", version, count)
		}
	}
	for _, key := range []string{executorDSLVersionEnv, strings.ToLower(executorDSLVersionEnv)} {
		attempt := testAttempt()
		attempt.Env = map[string]string{key: "2.0"}
		if err := refuseControlEnvOverrides(attempt); err == nil {
			t.Fatalf("accepted authored %s", key)
		}
	}
}
