package main

import (
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
)

func topologyBValidateGaggle(name string, backlog apiv1.Provider) apiv1.Gaggle {
	g := apiv1.Gaggle{}
	g.Name = name
	project, _ := topologyBProjectAndBacklog()
	g.Spec.Project = project
	g.Spec.Backlog = apiv1.BacklogRef{Provider: backlog, Project: topologyBBacklog}
	if backlog == apiv1.ProviderADO {
		g.Spec.Backlog.Project = topologyBCodeProject
	}
	return g
}

// RNR007: every task of a topology (b) run is refused a stage pod
// (engine.CrossProviderBacklogPodCode), so a task whose eligible runners
// exclude the daemon's own host warns at validate time. A task self can still
// run, a same-provider gaggle and an unsatisfiable task (RNR001's) do not.
// Under placement.selfExecution: deny nothing can run it, so it is an error.
func TestCrossProviderBacklogPlacementFinding(t *testing.T) {
	offSelf := apiv1.Task{
		Name: "query-backlog", Type: apiv1.TaskDeterministic,
		Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--read-only"}},
		RunsOn: &apiv1.RunsOn{Restrictions: []string{"network:allowlist"}},
	}
	tests := []struct {
		name             string
		backlog          apiv1.Provider
		task             apiv1.Task
		selfRestrictions []string
		selfExecution    string
		want             validate.Severity
	}{
		{name: "topology (b) task resolves off self", backlog: apiv1.ProviderGitHub, task: offSelf, want: validate.Warning},
		{name: "Gitea backlog too", backlog: apiv1.ProviderGitea, task: offSelf, want: validate.Warning},
		{name: "self stays eligible", backlog: apiv1.ProviderGitHub, task: offSelf, selfRestrictions: []string{"network:allowlist"}},
		{name: "no runsOn places on self", backlog: apiv1.ProviderGitHub, task: apiv1.Task{
			Name: "query-backlog", Type: apiv1.TaskDeterministic,
			Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--read-only"}},
		}},
		{name: "same-provider gaggle", backlog: apiv1.ProviderADO, task: offSelf},
		{name: "unsatisfiable task is RNR001's", backlog: apiv1.ProviderGitHub, task: apiv1.Task{
			Name: "query-backlog", Type: apiv1.TaskDeterministic,
			Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--read-only"}},
			RunsOn: &apiv1.RunsOn{Capabilities: []string{"no-runner-claims-this"}},
		}},
		{name: "self execution denied", backlog: apiv1.ProviderGitHub, task: offSelf, selfExecution: "deny", want: validate.Error},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			selfRestrictions := make([]instance.RunnerRestriction, len(tc.selfRestrictions))
			for i, r := range tc.selfRestrictions {
				selfRestrictions[i] = instance.RunnerRestriction(r)
			}
			cfg := &instance.Config{Runners: []instance.RunnerEntry{
				{Name: "self", Host: "self", Restrictions: selfRestrictions},
				{Name: "pod", Host: "ghcr.io/example/pod:v1", Restrictions: instance.KnownRunnerRestrictions()},
			}}
			if tc.selfExecution != "" {
				cfg.Placement = &instance.PlacementConfig{SelfExecution: tc.selfExecution}
			}
			set := &instance.ConfigSet{
				Gaggles: []apiv1.Gaggle{topologyBValidateGaggle("example", tc.backlog)},
				Workflows: []apiv1.Workflow{{
					ObjectMeta: metav1.ObjectMeta{Name: "backlog-curation"},
					DSLVersion: "3.0",
					Spec:       apiv1.WorkflowSpec{Gaggle: "example", Start: tc.task.Name, Tasks: []apiv1.Task{tc.task}},
				}},
			}
			type finding struct {
				severity      validate.Severity
				path, message string
			}
			var got []finding
			appendPlacementFindings("", "config", cfg, set, nil, false,
				func(code validate.WarningCode, severity validate.Severity, _, _, _, path, message string) {
					if code == validate.RunnerCrossProviderBacklogOffSelf {
						got = append(got, finding{severity, path, message})
					}
				})
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("RNR007 = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("RNR007 = %+v, want exactly one", got)
			}
			if got[0].severity != tc.want || got[0].path != "/spec/tasks/0/runsOn" {
				t.Errorf("RNR007 severity/path = %s %s, want %s /spec/tasks/0/runsOn", got[0].severity, got[0].path, tc.want)
			}
			for _, want := range []string{`stage "query-backlog"`, "[pod]", "topology (b)", engine.CrossProviderBacklogPodCode} {
				if !strings.Contains(got[0].message, want) {
					t.Errorf("RNR007 message missing %q: %s", want, got[0].message)
				}
			}
		})
	}
}

// CFG015: a topology (b) gaggle whose deterministic stages declare
// backlog-family capabilities that nothing backs, because the backlog
// repository has no repos[] entry with a credential, warns and names exactly
// the unbacked capabilities. A daemonIdentity backs github:issues:write only.
func TestCrossProviderBacklogUnboundWarning(t *testing.T) {
	adoEntry := instance.RepoRef{Provider: "ado", Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo,
		Token: instance.TokenRef{Env: "EXAMPLE_ADO_TOKEN"}}
	backlogEntry := instance.RepoRef{Provider: "github", Owner: "example-org", Name: "example-backlog",
		Token: instance.TokenRef{Env: "EXAMPLE_BACKLOG_TOKEN"}}
	tasks := []apiv1.Task{
		{Name: "query", Type: apiv1.TaskDeterministic, Capabilities: []string{"github:issues:read", "github:pr:write"}},
		{Name: "close-out", Type: apiv1.TaskDeterministic, Capabilities: []string{"github:issues:write", "github:issues:read"}},
		{Name: "curate", Type: apiv1.TaskAgentic, Capabilities: []string{"github:milestones:write"}},
	}
	tests := []struct {
		name     string
		backlog  apiv1.Provider
		repos    []instance.RepoRef
		identity bool
		want     string
	}{
		{name: "no backlog entry", backlog: apiv1.ProviderGitHub, repos: []instance.RepoRef{adoEntry}, want: "github:issues:read, github:issues:write"},
		{name: "daemon identity backs write only", backlog: apiv1.ProviderGitHub, repos: []instance.RepoRef{adoEntry}, identity: true, want: "github:issues:read"},
		{name: "backlog entry without a credential", backlog: apiv1.ProviderGitHub,
			repos: []instance.RepoRef{adoEntry, {Provider: "github", Owner: "example-org", Name: "example-backlog"}}, want: "github:issues:read, github:issues:write"},
		{name: "backlog entry backs every capability", backlog: apiv1.ProviderGitHub, repos: []instance.RepoRef{adoEntry, backlogEntry}},
		{name: "same-provider gaggle", backlog: apiv1.ProviderADO, repos: []instance.RepoRef{adoEntry}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &instance.Config{Repos: tc.repos}
			if tc.identity {
				cfg.DaemonIdentity = &instance.DaemonIdentityConfig{Kind: instance.GitHubAuthPAT, Token: &instance.TokenRef{Env: "EXAMPLE_DAEMON_TOKEN"}}
			}
			set := &instance.ConfigSet{
				Gaggles: []apiv1.Gaggle{topologyBValidateGaggle("example", tc.backlog)},
				Workflows: []apiv1.Workflow{{
					ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
					Spec:       apiv1.WorkflowSpec{Gaggle: "example", Start: "query", Tasks: tasks},
				}},
			}
			var got []validate.CodedWarning
			var paths []string
			for _, w := range appendStaticRealityWarnings("", "config", instance.ConfigFileName,
				cfg, set, nil, &validate.Report{}, false, false) {
				if w.warning.Code == validate.WarningCrossProviderBacklogUnbound {
					got = append(got, w.warning)
					paths = append(paths, w.path)
				}
			}
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("CFG015 = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Severity != validate.Warning || got[0].Scope != "Gaggle/example" ||
				!slices.Equal(paths, []string{"/spec/backlog/project"}) {
				t.Fatalf("CFG015 = %+v at %q, want one warning on Gaggle/example /spec/backlog/project", got, paths)
			}
			if !strings.Contains(got[0].Explanation, "declare "+tc.want+",") || !strings.Contains(got[0].Explanation, topologyBBacklog) {
				t.Errorf("CFG015 explanation = %q, want it to name %q and the backlog repository", got[0].Explanation, tc.want)
			}
			if strings.Contains(got[0].Explanation, "milestones") {
				t.Errorf("CFG015 names the agentic stage's capability, which the backlog credential never reaches: %q", got[0].Explanation)
			}
		})
	}
	if slices.Contains(strictNeutralWarningCodes, validate.WarningCrossProviderBacklogUnbound) {
		t.Fatal("CFG015 must not be strict-neutral: the stage cannot run as configured")
	}
}
