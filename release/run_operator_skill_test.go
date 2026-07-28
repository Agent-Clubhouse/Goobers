package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type runOperatorCorpus struct {
	SchemaVersion            string                  `json:"schemaVersion"`
	DefaultRecentLimit       int                     `json:"defaultRecentLimit"`
	AllowedCommandPrefixes   []string                `json:"allowedCommandPrefixes"`
	MutationNegativeControls []string                `json:"mutationNegativeControls"`
	Cases                    []runOperatorCorpusCase `json:"cases"`
}

type runOperatorCorpusCase struct {
	Name        string          `json:"name"`
	DaemonState string          `json:"daemonState"`
	Question    string          `json:"question"`
	Commands    []string        `json:"commands"`
	Fixture     json.RawMessage `json:"fixture"`
	Expected    struct {
		Classification string   `json:"classification"`
		Facts          []string `json:"facts"`
		Citations      []struct {
			Source    string   `json:"source"`
			RunID     string   `json:"runId"`
			Seqs      []uint64 `json:"seqs"`
			Timestamp string   `json:"timestamp"`
			URL       string   `json:"url"`
		} `json:"citations"`
		Uncertainty []string `json:"uncertainty"`
	} `json:"expected"`
}

func TestRunOperatorQuestionCorpus(t *testing.T) {
	corpus := loadRunOperatorCorpus(t)
	if corpus.SchemaVersion != "1" {
		t.Fatalf("schemaVersion = %q, want 1", corpus.SchemaVersion)
	}
	if corpus.DefaultRecentLimit != 20 {
		t.Fatalf("defaultRecentLimit = %d, want 20", corpus.DefaultRecentLimit)
	}

	wantPrefixes := []string{
		"<goobers> runs list",
		"<goobers> status --daemon",
		"<goobers> trace",
		"<goobers> stats",
		"<goobers> escalations show",
		"<goobers> claims list",
		"<goobers> blocked list",
		"<goobers> workflow show",
		"gh issue view",
		"gh pr view",
		"az devops invoke --http-method GET",
	}
	if strings.Join(corpus.AllowedCommandPrefixes, "\n") != strings.Join(wantPrefixes, "\n") {
		t.Fatalf("allowed command prefixes drifted:\ngot  %q\nwant %q", corpus.AllowedCommandPrefixes, wantPrefixes)
	}

	requiredClassifications := map[string]bool{
		"first-pass-success": false,
		"failed":             false,
		"reviewer-repass":    false,
		"escalated":          false,
		"no-work":            false,
		"aborted":            false,
		"pr-created":         false,
		"merged":             false,
		"issue-linked":       false,
		"claim-active":       false,
		"uncertain":          false,
	}
	daemonStates := map[string]bool{"stopped": false, "live": false}
	names := make(map[string]struct{}, len(corpus.Cases))
	for _, testCase := range corpus.Cases {
		if _, duplicate := names[testCase.Name]; duplicate {
			t.Errorf("duplicate case name %q", testCase.Name)
		}
		names[testCase.Name] = struct{}{}
		if testCase.Question == "" || len(testCase.Commands) == 0 || len(testCase.Fixture) == 0 {
			t.Errorf("%s: question, commands, and fixture must be populated", testCase.Name)
		}
		if _, ok := daemonStates[testCase.DaemonState]; !ok {
			t.Errorf("%s: unknown daemonState %q", testCase.Name, testCase.DaemonState)
		} else {
			daemonStates[testCase.DaemonState] = true
		}
		if _, ok := requiredClassifications[testCase.Expected.Classification]; !ok {
			t.Errorf("%s: unexpected classification %q", testCase.Name, testCase.Expected.Classification)
		} else {
			requiredClassifications[testCase.Expected.Classification] = true
		}
		assertRunOperatorFixtureContracts(t, testCase.Name, testCase.Fixture)
		if len(testCase.Expected.Facts) == 0 || len(testCase.Expected.Citations) == 0 {
			t.Errorf("%s: expected facts and citations must be populated", testCase.Name)
		}
		if testCase.Expected.Classification == "uncertain" && len(testCase.Expected.Uncertainty) == 0 {
			t.Errorf("%s: uncertain case must say what is unknown", testCase.Name)
		}
		for _, command := range testCase.Commands {
			if !hasAllowedRunOperatorPrefix(command, corpus.AllowedCommandPrefixes) {
				t.Errorf("%s: command is not allowlisted: %q", testCase.Name, command)
			}
			for _, unsafe := range []string{";", " && ", " || ", "\n", "\r", "`", "$("} {
				if strings.Contains(command, unsafe) {
					t.Errorf("%s: command contains shell composition %q: %q", testCase.Name, unsafe, command)
				}
			}
			if strings.HasPrefix(command, "<goobers> runs list") && !strings.Contains(command, "--limit=") {
				t.Errorf("%s: run discovery is unbounded: %q", testCase.Name, command)
			}
			if strings.HasPrefix(command, "<goobers> stats") && !strings.Contains(command, "--since=") {
				t.Errorf("%s: stats query is unbounded: %q", testCase.Name, command)
			}
			if testCase.DaemonState == "stopped" && hasRunOperatorPrefix(command, []string{
				"<goobers> stats",
				"<goobers> claims list",
				"<goobers> blocked list",
			}) {
				t.Errorf("%s: stopped-daemon case invokes a store-coordinating read: %q", testCase.Name, command)
			}
			if strings.HasPrefix(command, "gh ") || strings.HasPrefix(command, "az ") {
				if err := validateRunOperatorProviderRead(command, testCase.Fixture); err != nil {
					t.Errorf("%s: %v", testCase.Name, err)
				}
			}
		}
		for _, citation := range testCase.Expected.Citations {
			if citation.Source == "" || citation.Timestamp == "" {
				t.Errorf("%s: every citation needs source and timestamp", testCase.Name)
			}
			if citation.RunID == "" && citation.URL == "" {
				t.Errorf("%s: citation must identify a run or provider URL", testCase.Name)
			}
			if citation.RunID != "" && len(citation.Seqs) == 0 {
				t.Errorf("%s: run citation %q has no event sequences", testCase.Name, citation.RunID)
			}
		}
	}
	for classification, found := range requiredClassifications {
		if !found {
			t.Errorf("corpus is missing %s coverage", classification)
		}
	}
	for state, found := range daemonStates {
		if !found {
			t.Errorf("corpus is missing a %s-daemon case", state)
		}
	}

	if len(corpus.MutationNegativeControls) < 10 {
		t.Fatalf("mutation negative controls = %d, want at least 10", len(corpus.MutationNegativeControls))
	}
	for _, command := range corpus.MutationNegativeControls {
		if hasAllowedRunOperatorPrefix(command, corpus.AllowedCommandPrefixes) {
			t.Errorf("mutation negative control is accidentally allowlisted: %q", command)
		}
	}
}

func TestRunOperatorSkillContract(t *testing.T) {
	root := agentToolkitRepoRoot(t)
	skillPath := filepath.Join(root, "skills", "goobers-run-operator", "SKILL.md")
	body, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"runs list --json --limit=20",
		"trace --json <run-id>",
		"status --daemon",
		"stats --json --since=24h",
		"knownSchema: false",
		"externalRef.provider",
		"Reviewer repass",
		"No-work",
		"PR-created",
		"Merged",
		"gh pr view",
		"az devops invoke --http-method GET",
		"references/question-corpus.json",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("skill is missing contract text %q", required)
		}
	}
	for _, forbidden := range []string{
		"goobers runs cancel",
		"goobers claims release",
		"goobers blocked clear",
		"provider create/edit/comment/close/merge commands",
	} {
		if !strings.Contains(text, forbidden) {
			t.Errorf("skill is missing mutation prohibition %q", forbidden)
		}
	}
}

func loadRunOperatorCorpus(t *testing.T) runOperatorCorpus {
	t.Helper()
	path := filepath.Join(
		agentToolkitRepoRoot(t),
		"skills",
		"goobers-run-operator",
		"references",
		"question-corpus.json",
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var corpus runOperatorCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return corpus
}

func hasAllowedRunOperatorPrefix(command string, prefixes []string) bool {
	return hasRunOperatorPrefix(command, prefixes)
}

func hasRunOperatorPrefix(command string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if command == prefix || strings.HasPrefix(command, prefix+" ") {
			return true
		}
	}
	return false
}

func assertRunOperatorFixtureContracts(t *testing.T, name string, fixture json.RawMessage) {
	t.Helper()
	var root any
	if err := json.Unmarshal(fixture, &root); err != nil {
		t.Errorf("%s: decode fixture: %v", name, err)
		return
	}
	walkRunOperatorFixture(t, name, root)
}

func walkRunOperatorFixture(t *testing.T, name string, value any) {
	t.Helper()
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			walkRunOperatorFixture(t, name, item)
		}
	case map[string]any:
		if value["type"] == "gate.evaluated" {
			assertRunOperatorVerdict(t, name, value["verdict"])
		}
		if outcome, ok := value["outcome"].(map[string]any); ok {
			assertRunOperatorVerdict(t, name, outcome["verdict"])
		}
		if externalRef, ok := value["externalRef"].(map[string]any); ok {
			provider, _ := externalRef["provider"].(string)
			if provider != "github" && provider != "ado" {
				t.Errorf("%s: externalRef provider = %q, want github or ado", name, provider)
			}
		}
		if escalation, ok := value["escalation"].(map[string]any); ok {
			allowed := map[string]bool{
				"stage":                  true,
				"gate":                   true,
				"repassCount":            true,
				"lastNeedsChangesReason": true,
			}
			for field := range escalation {
				if !allowed[field] {
					t.Errorf("%s: trace escalation fixture has unsupported field %q", name, field)
				}
			}
		}
		for _, item := range value {
			walkRunOperatorFixture(t, name, item)
		}
	}
}

func assertRunOperatorVerdict(t *testing.T, name string, value any) {
	t.Helper()
	verdict, _ := value.(string)
	switch verdict {
	case "pass", "fail", "needs-changes":
	default:
		t.Errorf("%s: gate verdict = %q, want canonical decision", name, verdict)
	}
}

type runOperatorResolvedTarget struct {
	Provider        string `json:"provider"`
	Repository      string `json:"repository"`
	OrganizationURL string `json:"organizationUrl"`
	Project         string `json:"project"`
}

type runOperatorExternalRef struct {
	Provider string
	Kind     string
	ID       string
}

func validateRunOperatorProviderRead(command string, fixture json.RawMessage) error {
	var root map[string]any
	if err := json.Unmarshal(fixture, &root); err != nil {
		return fmt.Errorf("decode provider fixture: %w", err)
	}
	targetData, ok := root["resolvedTarget"]
	if !ok {
		return fmt.Errorf("provider read has no resolver-selected target")
	}
	encodedTarget, err := json.Marshal(targetData)
	if err != nil {
		return err
	}
	var target runOperatorResolvedTarget
	if err := json.Unmarshal(encodedTarget, &target); err != nil {
		return err
	}
	refs := collectRunOperatorExternalRefs(root)
	words := strings.Fields(command)
	if strings.HasPrefix(command, "gh ") {
		if len(words) < 6 {
			return fmt.Errorf("malformed GitHub read %q", command)
		}
		kind, id := words[1], words[3]
		if target.Provider != "github" || commandValue(words, "--repo") != target.Repository {
			return fmt.Errorf("GitHub read does not use resolver-selected repository %q", target.Repository)
		}
		if !hasRunOperatorExternalRef(refs, "github", kind, id) {
			return fmt.Errorf("GitHub read does not follow a matching externalRef")
		}
		return nil
	}

	resource := commandValue(words, "--resource")
	kind, idKey := "issue", "id"
	if resource == "pullRequests" {
		kind, idKey = "pr", "pullRequestId"
	} else if resource != "workItems" {
		return fmt.Errorf("unsupported Azure DevOps read resource %q", resource)
	}
	id := commandAssignment(words, idKey)
	if target.Provider != "ado" ||
		commandValue(words, "--org") != target.OrganizationURL ||
		commandAssignment(words, "project") != target.Project {
		return fmt.Errorf("Azure DevOps read does not use the resolver-selected target")
	}
	if !hasRunOperatorExternalRef(refs, "ado", kind, id) {
		return fmt.Errorf("Azure DevOps read does not follow a matching externalRef")
	}
	return nil
}

func collectRunOperatorExternalRefs(value any) []runOperatorExternalRef {
	var refs []runOperatorExternalRef
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			if externalRef, ok := value["externalRef"].(map[string]any); ok {
				refs = append(refs, runOperatorExternalRef{
					Provider: fmt.Sprint(externalRef["provider"]),
					Kind:     fmt.Sprint(externalRef["kind"]),
					ID:       fmt.Sprint(externalRef["id"]),
				})
			}
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(value)
	return refs
}

func hasRunOperatorExternalRef(refs []runOperatorExternalRef, provider, kind, id string) bool {
	for _, ref := range refs {
		if ref.Provider == provider && ref.Kind == kind && ref.ID == id {
			return true
		}
	}
	return false
}

func commandValue(words []string, flag string) string {
	for i := 0; i+1 < len(words); i++ {
		if words[i] == flag {
			return words[i+1]
		}
	}
	return ""
}

func commandAssignment(words []string, key string) string {
	prefix := key + "="
	for _, word := range words {
		if strings.HasPrefix(word, prefix) {
			return strings.TrimPrefix(word, prefix)
		}
	}
	return ""
}
