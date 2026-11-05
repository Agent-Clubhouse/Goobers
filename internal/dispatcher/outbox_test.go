package dispatcher

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestRenderPodPinsOutboxDeclaration(t *testing.T) {
	attempt := testAttempt()
	attempt.Outbox = []string{"reports", "summary.json"}
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal([]byte(podEnvMap(pod)[EnvStageOutbox]), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, attempt.Outbox) || !slices.Contains(DispatcherPrivilegedEnv, EnvStageOutbox) {
		t.Fatalf("outbox declaration lost or exposed: %v", got)
	}
	attempt.Env = map[string]string{EnvStageOutbox: `["forged"]`}
	if _, err := RenderPod(testConfig(), attempt, linuxRunner()); err == nil {
		t.Fatal("stage environment overrode outbox declaration")
	}
}
