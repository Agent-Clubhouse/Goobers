package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

const tutorPolicyWorkflowBase = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: sample
spec:
  gaggle: sample
  triggers:
    - type: manual
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: Build the project.
      run:
        command: ["make", "build"]
      next: quality
  gates:
    - name: quality
      evaluator: automated
      automated:
        check: output-numeric-lte
        params:
          key: failures
          threshold: "2"
      branches:
        pass: ""
        fail: "@abort"
`

func TestClassifyTutorChanges(t *testing.T) {
	gateTune := strings.Replace(tutorPolicyWorkflowBase, `threshold: "2"`, `threshold: "1"`, 1)
	goalTune := strings.Replace(tutorPolicyWorkflowBase, "Build the project.", "Compile the project.", 1)
	topology := strings.Replace(tutorPolicyWorkflowBase, "next: quality", `next: validate
    - name: validate
      type: deterministic
      goal: Validate the drafted config.
      run:
        command: ["goobers", "validate"]
      next: quality`, 1)
	gateRemoval := strings.Replace(tutorPolicyWorkflowBase, `  gates:
    - name: quality
      evaluator: automated
      automated:
        check: output-numeric-lte
        params:
          key: failures
          threshold: "2"
      branches:
        pass: ""
        fail: "@abort"
`, "", 1)

	tests := []struct {
		name      string
		changes   []tutorFileChange
		wantTypes []tutorChangeType
		wantHuman bool
	}{
		{
			name: "persona",
			changes: []tutorFileChange{{
				Path: "gaggles/example/goobers/reviewer/instructions.md",
			}},
			wantTypes: []tutorChangeType{tutorChangePersona},
		},
		{
			name: "skill instructions",
			changes: []tutorFileChange{{
				Path: "gaggles/example/skills/review/instructions.md",
			}},
			wantTypes: []tutorChangeType{tutorChangeSkill},
			wantHuman: true,
		},
		{
			name: "gate tune",
			changes: []tutorFileChange{{
				Path: "gaggles/example/workflows/review.yaml", Before: []byte(tutorPolicyWorkflowBase), After: []byte(gateTune),
			}},
			wantTypes: []tutorChangeType{tutorChangeGateTune},
		},
		{
			name: "task goal tune",
			changes: []tutorFileChange{{
				Path: "gaggles/example/workflows/review.yaml", Before: []byte(tutorPolicyWorkflowBase), After: []byte(goalTune),
			}},
			wantTypes: []tutorChangeType{tutorChangePersona},
		},
		{
			name: "validation topology",
			changes: []tutorFileChange{{
				Path: "gaggles/example/workflows/review.yaml", Before: []byte(tutorPolicyWorkflowBase), After: []byte(topology),
			}},
			wantTypes: []tutorChangeType{tutorChangeStructure, tutorChangeValidation},
			wantHuman: true,
		},
		{
			name: "gate removal",
			changes: []tutorFileChange{{
				Path: "gaggles/example/workflows/review.yaml", Before: []byte(tutorPolicyWorkflowBase), After: []byte(gateRemoval),
			}},
			wantTypes: []tutorChangeType{tutorChangeStructure},
			wantHuman: true,
		},
		{
			name: "skill body",
			changes: []tutorFileChange{{
				Path: "gaggles/example/skills/review/SKILL.md",
			}},
			wantTypes: []tutorChangeType{tutorChangeSkill},
			wantHuman: true,
		},
		{
			name: "rename out of skills remains high risk",
			changes: []tutorFileChange{{
				Path: "gaggles/example/goobers/reviewer/instructions.md", PreviousPath: "gaggles/example/skills/review/SKILL.md",
			}},
			wantTypes: []tutorChangeType{tutorChangePersona, tutorChangeStructure, tutorChangeSkill},
			wantHuman: true,
		},
		{
			name: "workflow rename is structural",
			changes: []tutorFileChange{{
				Path:         "gaggles/example/workflows/review-v2.yaml",
				PreviousPath: "gaggles/example/workflows/review.yaml",
				Before:       []byte(tutorPolicyWorkflowBase),
				After:        []byte(gateTune),
			}},
			wantTypes: []tutorChangeType{tutorChangeStructure},
			wantHuman: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyTutorChanges(tc.changes)
			if err != nil {
				t.Fatalf("classifyTutorChanges: %v", err)
			}
			if !reflect.DeepEqual(got.Types, tc.wantTypes) {
				t.Fatalf("types = %v, want %v", got.Types, tc.wantTypes)
			}
			if got.RequiresHumanSignoff() != tc.wantHuman {
				t.Fatalf("RequiresHumanSignoff = %t, want %t", got.RequiresHumanSignoff(), tc.wantHuman)
			}
		})
	}
}

func TestTutorBranchDetectionHonorsNamespace(t *testing.T) {
	for _, tc := range []struct {
		head      string
		namespace string
		want      bool
	}{
		{head: "goobers/tutor/run-1", namespace: "", want: true},
		{head: "acme/workflow-tutor/run-1", namespace: "acme", want: true},
		{head: "acme/tutor-review/run-1", namespace: "acme/", want: true},
		{head: "goobers/implementation/run-1", namespace: "", want: false},
		{head: "other/tutor/run-1", namespace: "goobers/", want: false},
	} {
		if got := isTutorBranch(tc.head, tc.namespace); got != tc.want {
			t.Errorf("isTutorBranch(%q, %q) = %t, want %t", tc.head, tc.namespace, got, tc.want)
		}
	}
}

func TestParseTutorNameStatusPreservesRenameIdentity(t *testing.T) {
	changes, err := parseTutorNameStatus([]byte(
		"R097\x00gaggles/example/workflows/old.yaml\x00gaggles/example/workflows/new.yaml\x00" +
			"M\x00gaggles/example/goobers/reviewer/instructions.md\x00",
	))
	if err != nil {
		t.Fatalf("parseTutorNameStatus: %v", err)
	}
	want := []tutorFileChange{
		{Path: "gaggles/example/workflows/new.yaml", PreviousPath: "gaggles/example/workflows/old.yaml"},
		{Path: "gaggles/example/goobers/reviewer/instructions.md"},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %#v, want %#v", changes, want)
	}
}

func TestOpenPRStampsTutorReviewPath(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantType   string
		wantReview string
	}{
		{
			name:       "persona follows normal review",
			file:       "selfhost/gaggles/goobers/goobers/config-author/instructions.md",
			wantType:   "**Types:** `persona`",
			wantReview: "Normal review path",
		},
		{
			name:       "skill requires human signoff",
			file:       "selfhost/gaggles/goobers/skills/config-author/SKILL.md",
			wantType:   "**Types:** `skill`",
			wantReview: "Explicit human sign-off required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
			t.Setenv("GOOBERS_WORKFLOW", "tutor")
			t.Setenv(executor.RepoProviderEnvVar, "github")
			t.Setenv(executor.RepoOwnerEnvVar, "your-org")
			t.Setenv(executor.RepoNameEnvVar, "your-repo")
			wt := gitRepoWithRunBranchChanges(t, map[string]string{tc.file: "proposed content\n"})
			t.Chdir(wt)

			if code, _, stderr := runArgs(t, "open-pr", root); code != 0 {
				t.Fatalf("open-pr: code = %d, stderr = %q", code, stderr)
			}
			server.mu.Lock()
			body := server.prs[1].body
			server.mu.Unlock()
			for _, want := range []string{"## Tutor change classification", tc.wantType, tc.wantReview} {
				if !strings.Contains(body, want) {
					t.Errorf("PR body missing %q:\n%s", want, body)
				}
			}
		})
	}
}

func TestPRSelectRoutesOnlyLowRiskTutorChangesToAutomatedReview(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const workflowPath = "selfhost/gaggles/goobers/workflows/review.yaml"
	const skillPath = "selfhost/gaggles/goobers/skills/review/instructions.md"
	gateTune := strings.Replace(tutorPolicyWorkflowBase, `threshold: "2"`, `threshold: "1"`, 1)
	server.addOpenPR(10, "goobers/tutor/run-10", "main", "skill-head", "base",
		false, nil, []fakePRFile{{path: skillPath, status: "modified"}})
	server.addOpenPR(11, "goobers/tutor/run-11", "main", "gate-head", "base",
		false, nil, []fakePRFile{{path: workflowPath, status: "modified"}})
	server.compares["base...skill-head"] = fakeCompare{
		mergeBaseSHA: "base",
		files:        []fakePRFile{{path: skillPath, status: "modified"}},
	}
	server.compares["base...gate-head"] = fakeCompare{
		mergeBaseSHA: "base",
		files:        []fakePRFile{{path: workflowPath, status: "modified"}},
	}
	server.setFileContent("base", workflowPath, tutorPolicyWorkflowBase)
	server.setFileContent("gate-head", workflowPath, gateTune)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv(executor.RepoProviderEnvVar, "github")
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/tutor/")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "manual review required for Tutor PR #10") {
		t.Fatalf("stdout = %q, want high-risk Tutor classification", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	var selected map[string]string
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatal(err)
	}
	if selected["number"] != "11" {
		t.Fatalf("selected PR = %q, want low-risk gate-tune PR 11", selected["number"])
	}
}

func TestRemoteTutorClassificationFailsClosedAtGitHubFileLimit(t *testing.T) {
	const fileLimit = 3000
	files := make([]fakePRFile, fileLimit)
	for i := range files {
		files[i] = fakePRFile{
			path:   fmt.Sprintf("selfhost/gaggles/goobers/goobers/persona-%04d/instructions.md", i),
			status: "modified",
		}
	}
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(10, "goobers/tutor/run-10", "main", "head", "base", false, nil, files)
	server.compares["base...head"] = fakeCompare{mergeBaseSHA: "base", files: files[:300]}
	provider := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) {
		p.BaseURL = server.server.URL
	})

	_, err := classifyRemoteTutorChanges(
		context.Background(),
		provider,
		providers.RepositoryRef{Owner: "your-org", Name: "your-repo"},
		"10",
		"base",
		"head",
	)
	if err == nil || !strings.Contains(err.Error(), "3000-file limit") {
		t.Fatalf("classifyRemoteTutorChanges error = %v, want incomplete inventory failure", err)
	}
}
