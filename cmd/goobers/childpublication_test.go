package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestChildPublicationAcceptsOnlyCanonicalHostActions(t *testing.T) {
	for command, want := range map[string]string{"push-branch": "branch", "open-pr": "pr"} {
		original := apiv1.DeterministicRun{Command: []string{"goobers", command}, Workspace: apiv1.WorkspaceRepo}
		if action, err := childPublicationAction(&original); err != nil || action != want {
			t.Fatal(action, err)
		}
		for name, modify := range map[string]func(*apiv1.DeterministicRun){
			"flags":       func(r *apiv1.DeterministicRun) { r.Command = append(r.Command, "--repo=foreign") },
			"script":      func(r *apiv1.DeterministicRun) { r.Script = "touch unauthorized" },
			"environment": func(r *apiv1.DeterministicRun) { r.Env = map[string]string{"GIT_CONFIG_COUNT": "1"} },
			"network":     func(r *apiv1.DeterministicRun) { r.Network = "host" },
			"sync base":   func(r *apiv1.DeterministicRun) { r.SyncBase = true },
			"run context": func(r *apiv1.DeterministicRun) { r.InjectRunContext = true },
		} {
			t.Run(command+"/"+name, func(t *testing.T) {
				changed := original
				changed.Command = append([]string(nil), original.Command...)
				modify(&changed)
				if _, err := childPublicationAction(&changed); err == nil {
					t.Fatal("authored host override admitted")
				}
			})
		}
	}
	if action, err := childPublicationAction(&apiv1.DeterministicRun{Command: []string{"sh", "-c", "goobers open-pr"}}); err != nil || action != "" {
		t.Fatal("authored command must stay in pod path", action, err)
	}
}

func TestChildPublicationRequiresActionSpecificCapability(t *testing.T) {
	for _, action := range []string{"branch", "pr"} {
		if _, err := childPublicationCapability(apiv1.Task{Capabilities: []string{"agent:model"}}, action); err == nil {
			t.Fatal("model capability authorized publication", action)
		}
	}
	if _, err := childPublicationCapability(apiv1.Task{Capabilities: []string{"repo:push"}}, "pr"); err == nil {
		t.Fatal("branch capability authorized PR")
	}
	if _, err := childPublicationCapability(apiv1.Task{Capabilities: []string{"provider:pr:write"}}, "branch"); err == nil {
		t.Fatal("PR capability authorized branch")
	}
}
