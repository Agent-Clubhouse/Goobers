package dispatcher

import (
	"slices"
	"testing"
)

// An engine parent stage's child-workflow grant reaches its pod only as a
// privileged control variable, which __dispatch-exec strips from every stage,
// CLI included; any other stage carries none.
func TestRenderPodDeliversTheChildWorkflowGrantAsPrivilegedEnv(t *testing.T) {
	if !slices.Contains(DispatcherPrivilegedEnv, EnvChildWorkflowAccess) {
		t.Fatalf("%s must be in DispatcherPrivilegedEnv: it is a bearer", EnvChildWorkflowAccess)
	}
	attempt := testAttempt()
	attempt.ChildWorkflowAccess = `{"endpoint":"https://control.example","bearerToken":"goobers-child.signed"}`
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	var delivered []string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvChildWorkflowAccess {
			delivered = append(delivered, e.Value)
		}
	}
	if len(delivered) != 1 || delivered[0] != attempt.ChildWorkflowAccess {
		t.Fatalf("delivered %v, want exactly the attempt's grant", delivered)
	}
	pod, err = RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvChildWorkflowAccess {
			t.Fatalf("a stage without a grant carries %s=%q", e.Name, e.Value)
		}
	}
}
