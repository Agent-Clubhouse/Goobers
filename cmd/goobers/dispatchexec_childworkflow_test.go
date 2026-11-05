package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
)

// The engine parent stage's child-workflow grant is for the harness launcher's
// child-workflow tool only: no stage process, agentic or goobers-CLI, inherits
// it (docs/design/engine-parent-child-authority.md §2 step 4).
func TestChildWorkflowGrantNeverReachesTheStageEnvironment(t *testing.T) {
	for name, cli := range map[string]string{"agentic or command stage": "", "goobers-CLI stage": "true"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(dispatcher.EnvChildWorkflowAccess, `{"endpoint":"https://control.example","bearerToken":"goobers-child.signed-grant"}`)
			t.Setenv(dispatcher.EnvStageIsCLI, cli)
			t.Setenv("GOOBERS_STAGE_COMMAND", `["goobers","backlog-query"]`)
			for _, kv := range stageEnvironment() {
				if name, value, _ := strings.Cut(kv, "="); name == dispatcher.EnvChildWorkflowAccess || strings.Contains(value, "goobers-child.") {
					t.Fatalf("stage environment carries the child-workflow grant: %s", name)
				}
			}
		})
	}
}
