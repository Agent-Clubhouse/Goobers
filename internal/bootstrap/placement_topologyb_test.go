package bootstrap

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// selfAndLinuxPodConfig is the common cluster inventory: a self entry with no
// declared OS next to a Linux pod. SelectRunner's Linux preference picks the
// pod for any stage both satisfy.
func selfAndLinuxPodConfig() *instance.Config {
	return &instance.Config{Runners: []instance.RunnerEntry{
		{Name: "self", Host: "self"},
		{Name: "pod", Host: "ghcr.io/example/pod:v1", Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux},
			Restrictions: instance.KnownRunnerRestrictions()},
	}}
}

func backlogConfigSet(backlog apiv1.Provider) *instance.ConfigSet {
	return &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: "service"},
			Backlog: apiv1.BacklogRef{Provider: backlog, Project: "example-org/example-backlog"},
		},
	}}}
}

// A topology (b) stage (GitHub or Gitea backlog, Azure DevOps code) is refused
// every stage pod at dispatch, so whenever a self runner is eligible it is
// pinned to self, even against a Linux pod that a self entry with no declared
// OS would otherwise lose to. With self ineligible it still pins the pod; the
// engine refuses it there, and `goobers validate` warns about it (RNR007).
// Every other gaggle keeps the Linux-preferring choice.
func TestPinStagePlacementsTopologyBPrefersSelf(t *testing.T) {
	query := apiv1.Task{Name: "query-backlog", Type: apiv1.TaskDeterministic,
		Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--read-only"}}}
	restricted := query
	restricted.RunsOn = &apiv1.RunsOn{Restrictions: []string{"network:allowlist"}}
	for _, tc := range []struct {
		name     string
		backlog  apiv1.Provider
		task     apiv1.Task
		wantSelf bool
	}{
		{name: "GitHub backlog, self eligible", backlog: apiv1.ProviderGitHub, task: query, wantSelf: true},
		{name: "Gitea backlog, self eligible", backlog: apiv1.ProviderGitea, task: query, wantSelf: true},
		{name: "GitHub backlog, self ineligible", backlog: apiv1.ProviderGitHub, task: restricted},
		{name: "ADO backlog keeps the Linux preference", backlog: apiv1.ProviderADO, task: query},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placements, err := PinStagePlacements(selfAndLinuxPodConfig(), backlogConfigSet(tc.backlog), "web", placementSpecV30([]apiv1.Task{tc.task}, nil))
			if err != nil {
				t.Fatalf("PinStagePlacements: %v", err)
			}
			pin := findPin(t, placements, "query-backlog")
			if pin.Self != tc.wantSelf {
				t.Fatalf("pin = %+v, want Self=%t", pin, tc.wantSelf)
			}
			if !tc.wantSelf && pin.Queue != "goobers-dispatch.web.pod" {
				t.Fatalf("pin queue = %q, want the pod's queue", pin.Queue)
			}
		})
	}
}
