package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestExactWorkItemClaimNeverSelectsAnotherEligibleIssue(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Older eligible issue", "goobers:approved", "goobers:ready")
	server.addIssue(42, "Explicitly selected issue", "goobers:approved", "goobers:ready")
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "exact-run")
	t.Setenv(executor.TriggerRefEnvVar, "work-item:42")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	work := t.TempDir()
	t.Chdir(work)
	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(work, "claimed-item.json"))
	if err != nil {
		t.Fatal(err)
	}
	var selected providers.WorkItem
	if err := json.Unmarshal(data, &selected); err != nil || selected.ID != "42" {
		t.Fatal(selected, err)
	}
	if strings.Contains(stdout, "claimed 7:") {
		t.Fatal("claimed a different eligible item:", stdout)
	}
}

func TestExactWorkItemMissingTargetCannotFallBackToAnotherIssue(t *testing.T) {
	items := []providers.WorkItem{{ID: "7"}, {ID: "8"}}
	if selected, err := restrictExactBacklogWorkItem(items, "42"); err == nil || len(selected) != 0 {
		t.Fatal("missing exact target did not fail closed", selected, err)
	}
	t.Setenv(executor.TriggerRefEnvVar, "work-item:../42")
	if _, err := exactBacklogWorkItem(); err == nil {
		t.Fatal("malformed targeted trigger accepted")
	}
}

func TestExactWorkItemClaimPreservesTrustAndOpenStateGates(t *testing.T) {
	for _, reason := range []string{"untrusted", "closed", "missing"} {
		t.Run(reason, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(7, "Another eligible issue", "goobers:approved", "goobers:ready")
			if reason != "missing" {
				labels := []string{"goobers:ready"}
				if reason == "closed" {
					labels = append(labels, "goobers:approved")
				}
				server.addIssue(42, "Selected but ineligible issue", labels...)
				if reason == "closed" {
					server.setIssueState(42, "closed")
				}
			}
			providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "ineligible-exact-run")
			t.Setenv(executor.TriggerRefEnvVar, "work-item:42")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
			work := t.TempDir()
			t.Chdir(work)
			code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
			if code == 0 {
				t.Fatal("ineligible exact selection succeeded", stderr)
			}
			data, err := os.ReadFile(filepath.Join(work, "claimed-item.json"))
			if err == nil {
				var claimed providers.WorkItem
				if json.Unmarshal(data, &claimed) == nil && claimed.ID != "" {
					t.Fatal("claimed an item despite an ineligible exact target", claimed)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		})
	}
}
