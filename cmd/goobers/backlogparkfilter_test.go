package main

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestBacklogQueryParkLabelFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, filter, extraExclude, wantID, wantDiagnostic string
	}{
		{name: "default excludes", wantDiagnostic: `has excluded label "goobers:needs-remediation"`},
		{name: "explicit exclusion", filter: "true", wantDiagnostic: `has excluded label "goobers:needs-remediation"`},
		{name: "opt out includes", filter: "false", wantID: "7"},
		{name: "explicit exclusion survives opt out", filter: "false", extraExclude: ",goobers:needs-remediation", wantDiagnostic: `has excluded label "goobers:needs-remediation"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(7, "Parked candidate", "trusted", "goobers:needs-remediation")
			server.addIssue(8, "Already ready", "trusted", "goobers:ready")
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "park-filter-run")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
			t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers:ready"+tc.extraExclude)
			t.Setenv("GOOBERS_INPUT_PARKLABELS", "goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation")
			t.Setenv("GOOBERS_INPUT_FILTERPARKLABELS", tc.filter)
			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "backlog-query", "--debug", root)
			if code != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			var ids []string
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				if id, _, ok := strings.Cut(line, "\t"); ok {
					ids = append(ids, id)
				}
			}
			if got := strings.Join(ids, ","); got != tc.wantID {
				t.Fatalf("IDs=%q want=%q; stdout=%q stderr=%q", got, tc.wantID, stdout, stderr)
			}
			if tc.wantDiagnostic != "" && !strings.Contains(stderr, tc.wantDiagnostic) {
				t.Fatalf("missing parked-only diagnostic %q: %s", tc.wantDiagnostic, stderr)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if hasAnyLabel(server.issues[7].labels, []string{"goobers:ready", "goobers:claimed"}) {
				t.Fatalf("selection mutated parked candidate: %v", server.issues[7].labels)
			}
		})
	}
}

func TestBacklogParkFilterRoutingAndRefillAgree(t *testing.T) {
	for _, filter := range []string{"", "true", "false", "invalid"} {
		t.Run("filter="+filter, func(t *testing.T) {
			project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "team", Name: "repo"}
			gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "local"}, Spec: apiv1.GaggleSpec{Project: project, RequireLabels: []string{"partition"}}}
			wf := apiv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "curate"}, Spec: apiv1.WorkflowSpec{
				Gaggle: "local", Start: "query", Readiness: apiv1.ReadinessConditions{DesiredConcurrentRuns: 1},
				Tasks: []apiv1.Task{{Name: "query", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}}, Inputs: map[string]string{
					"trustLabel": "approved", "parkLabels": "custom:park", "filterParkLabels": filter,
					"excludeLabels": "always-exclude", "labelPredicate": `!("deny" in labels)`,
				}}},
			}}
			cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "team", Name: "repo"}}}
			routes, _, routeErr := compileRoutingScopes(localBacklogRoutingScopes(gaggle, []apiv1.Workflow{wf}))
			counter, counterErr := buildRefillDemandCounter(cfg, gaggle, &wf, project, nil, nil, "", "", nil)
			if filter == "invalid" {
				if routeErr == nil || counterErr == nil {
					t.Fatalf("invalid filter accepted: route=%v refill=%v", routeErr, counterErr)
				}
				return
			}
			if routeErr != nil || counterErr != nil {
				t.Fatalf("route=%v refill=%v", routeErr, counterErr)
			}
			refill := counter.(*backlogCounter)
			for _, tc := range []struct {
				labels []string
				want   bool
			}{
				{[]string{"approved", "partition", "custom:park"}, filter == "false"},
				{[]string{"approved", "partition"}, true},
				{[]string{"approved", "custom:park"}, false},
				{[]string{"partition", "custom:park"}, false},
				{[]string{"approved", "partition", "custom:park", "always-exclude"}, false},
				{[]string{"approved", "partition", "custom:park", "deny"}, false},
			} {
				matched, err := matchesAnyRoutingScope(tc.labels, routes)
				if err != nil || matched != tc.want {
					t.Errorf("routing %v: %v/%v want %v", tc.labels, matched, err, tc.want)
				}
				matched, err = refill.labelPredicate.Matches(tc.labels)
				if err != nil || matched != tc.want {
					t.Errorf("refill %v: %v/%v want %v", tc.labels, matched, err, tc.want)
				}
			}
		})
	}
}

func TestBacklogParkFilterRejectsInvalidBoolean(t *testing.T) {
	if _, _, err := compileBacklogLabelSelection("", nil, nil, "", "flase"); err == nil || !strings.Contains(err.Error(), "filterParkLabels") {
		t.Fatalf("invalid configuration accepted: %v", err)
	}
}

func TestBacklogParkLabelsJoinPreflightInventory(t *testing.T) {
	labels := &connectLabelSet{}
	task := apiv1.Task{Inputs: map[string]string{"parkLabels": "custom:park,goobers:needs-remediation"}}
	connectTaskAppliedLabels(task, labels)
	for _, label := range []string{"custom:park", "goobers:needs-remediation"} {
		if !labels.has(label) {
			t.Errorf("park label %q absent from repository inventory", label)
		}
	}
	demand := &repoRealityDemand{}
	appendTaskLifecycleLabelUses(demand, task, "query", "workflow.yaml", "/spec/tasks/0/inputs")
	if len(demand.labelUses) != 2 {
		t.Fatalf("duplicate or missing park-label uses: %v", demand.labelUses)
	}
	for _, use := range demand.labelUses {
		if use.kind != labelUseExclude || !strings.HasSuffix(use.path, "/parkLabels") {
			t.Errorf("park selector misreported as an applied label: %+v", use)
		}
	}
}
