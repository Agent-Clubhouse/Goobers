package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
)

type runOperatorCorpus struct {
	SchemaVersion            string                  `json:"schemaVersion"`
	DefaultRecentLimit       int                     `json:"defaultRecentLimit"`
	AllowedCommandPrefixes   []string                `json:"allowedCommandPrefixes"`
	MutationNegativeControls []string                `json:"mutationNegativeControls"`
	Cases                    []runOperatorCorpusCase `json:"cases"`
}

type runOperatorCorpusCase struct {
	Name        string              `json:"name"`
	DaemonState string              `json:"daemonState"`
	Question    string              `json:"question"`
	Commands    []string            `json:"commands"`
	Fixture     json.RawMessage     `json:"fixture"`
	Expected    runOperatorExpected `json:"expected"`
}

type runOperatorExpected struct {
	Classification string                `json:"classification"`
	Facts          []string              `json:"facts"`
	Citations      []runOperatorCitation `json:"citations"`
	Uncertainty    []string              `json:"uncertainty"`
}

type runOperatorCitation struct {
	Source    string   `json:"source"`
	RunID     string   `json:"runId"`
	Seqs      []uint64 `json:"seqs"`
	Timestamp string   `json:"timestamp"`
	URL       string   `json:"url"`
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
		evidence := inspectRunOperatorFixtureEvidence(t, testCase.Name, testCase.Fixture)
		if len(evidence.Issues) > 0 && len(testCase.Expected.Uncertainty) == 0 {
			t.Errorf("%s: incomplete fixture evidence requires uncertainty: %s",
				testCase.Name, strings.Join(evidence.Issues, "; "))
		}
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
			for _, seq := range citation.Seqs {
				if _, ok := evidence.EventSeqs[seq]; !ok {
					t.Errorf("%s: citation references absent event sequence %d", testCase.Name, seq)
				}
			}
		}
		for _, causalSeq := range evidence.CausalSeqs {
			if !runOperatorCitationsContainSeq(testCase.Expected.Citations, causalSeq) {
				t.Errorf("%s: causal event sequence %d is not cited", testCase.Name, causalSeq)
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

func TestRunOperatorQuestionCorpusScenarios(t *testing.T) {
	corpus := loadRunOperatorCorpus(t)
	binary := buildRunOperatorCLI(t)
	// Every scenario whose evidence is journal-only is executed against a real
	// CLI and a materialized instance, and its answer is derived from what the
	// CLI actually printed. Two groups are still static fixture self-checks:
	// the provider-backed cases (GitHub and Azure DevOps), which need a stubbed
	// provider this harness does not have, and incomplete-unknown-journal,
	// whose fixture carries an unknown-schema event that cannot be written
	// through the schema-validating journal writer.
	for _, name := range []string{
		"recent-first-pass-success-stopped",
		"reviewer-repass-live",
		"ci-failure-stopped",
		"defined-abort",
		"scheduled-no-work-stopped",
		"escalated-after-review-budget",
		"resumed-after-escalation-completes",
	} {
		testCase := findRunOperatorCorpusCase(t, corpus, name)
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "instance")
			if _, err := instance.Init(root); err != nil {
				t.Fatalf("initialize fixture instance: %v", err)
			}
			materializeRunOperatorJournal(t, root, testCase.Fixture)
			configureRunOperatorDaemonMode(t, root, testCase.DaemonState)

			observation := executeRunOperatorReads(t, binary, root, testCase.Commands)
			got, err := answerRunOperatorObservation(observation)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, testCase.Expected) {
				t.Errorf("operator answer mismatch:\ngot  %#v\nwant %#v", got, testCase.Expected)
			}
		})
	}
}

func TestRunOperatorEvidenceInspectionRejectsMissingCausalEvent(t *testing.T) {
	fixture := json.RawMessage(`{
		"trace": {
			"phase": "completed",
			"outcome": {"causalEventSeq": 11},
			"terminalCause": {"code": "failed", "causalEventSeq": 12},
			"events": [
				{"seq": 9, "type": "stage.finished"},
				{"seq": 10, "type": "error", "error": {"code": "failed"}},
				{"seq": 12, "type": "run.finished"}
			]
		}
	}`)
	evidence := inspectRunOperatorFixtureEvidence(t, "missing-causal-event", fixture)
	for _, want := range []string{
		"event sequence gap between 10 and 12",
		"causal event sequence 11 is absent",
		"terminal cause sequence 12 does not match derived sequence 10",
	} {
		if !containsRunOperatorString(evidence.Issues, want) {
			t.Errorf("evidence issues = %q, want %q", evidence.Issues, want)
		}
	}
}

func TestRunOperatorADOProviderReadRejectsWrongRepository(t *testing.T) {
	testCase := findRunOperatorCorpusCase(t, loadRunOperatorCorpus(t), "azure-devops-pr-created")
	var command string
	for _, candidate := range testCase.Commands {
		if strings.Contains(candidate, "--resource pullRequests") {
			command = candidate
			break
		}
	}
	if command == "" {
		t.Fatal("Azure DevOps PR case has no pullRequests command")
	}
	if err := validateRunOperatorProviderRead(command, testCase.Fixture); err != nil {
		t.Fatalf("valid provider read rejected: %v", err)
	}
	wrongRepository := strings.Replace(command, "repositoryId=widgets-repo-id", "repositoryId=other-repo-id", 1)
	if err := validateRunOperatorProviderRead(wrongRepository, testCase.Fixture); err == nil ||
		!strings.Contains(err.Error(), "repository") {
		t.Fatalf("wrong-repository read error = %v, want repository mismatch", err)
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
		"repositoryId",
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

func findRunOperatorCorpusCase(t *testing.T, corpus runOperatorCorpus, name string) runOperatorCorpusCase {
	t.Helper()
	for _, testCase := range corpus.Cases {
		if testCase.Name == name {
			return testCase
		}
	}
	t.Fatalf("corpus case %q not found", name)
	return runOperatorCorpusCase{}
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

type runOperatorFixtureEvidence struct {
	EventSeqs  map[uint64]struct{}
	CausalSeqs []uint64
	Issues     []string
}

func inspectRunOperatorFixtureEvidence(
	t *testing.T,
	name string,
	fixture json.RawMessage,
) runOperatorFixtureEvidence {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(fixture, &root); err != nil {
		t.Fatalf("%s: decode fixture evidence: %v", name, err)
	}
	evidence := runOperatorFixtureEvidence{EventSeqs: make(map[uint64]struct{})}
	trace, _ := root["trace"].(map[string]any)
	events, _ := trace["events"].([]any)
	var previous uint64
	var derivedTerminalCauseSeq uint64
	hasRunFinished := false
	for _, rawEvent := range events {
		event, _ := rawEvent.(map[string]any)
		seq := uint64(runOperatorJSONNumber(event["seq"]))
		if seq == 0 {
			evidence.Issues = append(evidence.Issues, "event has no positive sequence")
			continue
		}
		if previous != 0 && seq != previous+1 {
			evidence.Issues = append(evidence.Issues,
				fmt.Sprintf("event sequence gap between %d and %d", previous, seq))
		}
		previous = seq
		evidence.EventSeqs[seq] = struct{}{}
		if event["type"] == "run.finished" {
			hasRunFinished = true
		}
		if known, ok := event["knownSchema"].(bool); ok && !known {
			evidence.Issues = append(evidence.Issues,
				fmt.Sprintf("event sequence %d has an unknown schema", seq))
		} else if _, ok := event["error"].(map[string]any); ok &&
			(event["type"] == "error" || event["type"] == "run.finished") {
			derivedTerminalCauseSeq = seq
		}
	}
	switch trace["phase"] {
	case "completed", "failed", "aborted", "escalated":
		if !hasRunFinished {
			evidence.Issues = append(evidence.Issues, "terminal phase has no run.finished event")
		}
	}
	collectRunOperatorCausalSeqs(root, &evidence.CausalSeqs)
	for _, seq := range evidence.CausalSeqs {
		if _, ok := evidence.EventSeqs[seq]; !ok {
			evidence.Issues = append(evidence.Issues,
				fmt.Sprintf("causal event sequence %d is absent", seq))
		}
	}
	if cause, ok := trace["terminalCause"].(map[string]any); ok && cause["code"] != nil {
		declared := uint64(runOperatorJSONNumber(cause["causalEventSeq"]))
		if declared != derivedTerminalCauseSeq {
			evidence.Issues = append(evidence.Issues, fmt.Sprintf(
				"terminal cause sequence %d does not match derived sequence %d",
				declared, derivedTerminalCauseSeq))
		}
	}
	return evidence
}

func collectRunOperatorCausalSeqs(value any, seqs *[]uint64) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			collectRunOperatorCausalSeqs(item, seqs)
		}
	case map[string]any:
		for key, item := range value {
			if key == "causalEventSeq" {
				if seq := uint64(runOperatorJSONNumber(item)); seq != 0 {
					*seqs = append(*seqs, seq)
				}
				continue
			}
			collectRunOperatorCausalSeqs(item, seqs)
		}
	}
}

func runOperatorJSONNumber(value any) float64 {
	number, _ := value.(float64)
	return number
}

func runOperatorCitationsContainSeq(citations []runOperatorCitation, seq uint64) bool {
	for _, citation := range citations {
		for _, cited := range citation.Seqs {
			if cited == seq {
				return true
			}
		}
	}
	return false
}

func containsRunOperatorString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
	if resource == "pullRequests" &&
		(target.Repository == "" || commandAssignment(words, "repositoryId") != target.Repository) {
		return fmt.Errorf("Azure DevOps PR read does not use resolver-selected repository %q", target.Repository)
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

type executableRunOperatorFixture struct {
	Trace struct {
		Identity journal.RunIdentity `json:"identity"`
		Events   []journal.Event     `json:"events"`
	} `json:"trace"`
}

func buildRunOperatorCLI(t *testing.T) string {
	t.Helper()
	name := "goobers"
	if runtime.GOOS == "windows" {
		// `go build -o <path>` writes exactly the given file name — unlike a
		// bare `go build`/`go install`, it never appends the platform's exe
		// suffix on its own. Without ".exe" here, exec.Command below fails on
		// Windows with "executable file not found in %PATH%": since Go 1.19,
		// os/exec's LookPath refuses to resolve an extensionless absolute
		// path via the implicit PATHEXT search it used to perform.
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/goobers")
	cmd.Dir = agentToolkitRepoRoot(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build goobers CLI: %v\n%s", err, output)
	}
	return binary
}

func materializeRunOperatorJournal(t *testing.T, root string, raw json.RawMessage) {
	t.Helper()
	var fixture executableRunOperatorFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode executable fixture: %v", err)
	}
	if fixture.Trace.Identity.Trigger.Kind == "" {
		fixture.Trace.Identity.Trigger = journal.Trigger{Kind: journal.TriggerManual}
	}
	now := fixture.Trace.Identity.StartedAt
	run, err := journal.Create(
		instance.NewLayout(root).RunsDir(),
		fixture.Trace.Identity,
		nil,
		journal.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("create fixture journal: %v", err)
	}
	nextSeq := uint64(2)
	for _, event := range fixture.Trace.Events {
		wantedSeq := event.Seq
		for nextSeq < wantedSeq {
			if err := run.Append(journal.Event{
				Type:   journal.EventRunnerAnnotation,
				Runner: map[string]any{"fixture": "omitted non-material event"},
			}); err != nil {
				t.Fatalf("append fixture filler event: %v", err)
			}
			nextSeq++
		}
		if wantedSeq != nextSeq {
			t.Fatalf("fixture event sequence %d follows %d", wantedSeq, nextSeq-1)
		}
		now = event.Time
		if err := run.Append(event); err != nil {
			t.Fatalf("append fixture event %d: %v", wantedSeq, err)
		}
		nextSeq++
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close fixture journal: %v", err)
	}
}

func configureRunOperatorDaemonMode(t *testing.T, root, mode string) {
	t.Helper()
	lockPath := filepath.Join(instance.NewLayout(root).SchedulerDir(), "up.lock")
	if mode == "stopped" {
		held, err := lock.TryAcquire(lockPath)
		if err != nil {
			t.Fatalf("stopped-daemon fixture lock is held: %v", err)
		}
		if err := held.Release(); err != nil {
			t.Fatalf("release stopped-daemon fixture probe: %v", err)
		}
		return
	}

	held, err := lock.TryAcquire(lockPath)
	if err != nil {
		t.Fatalf("acquire live-daemon fixture lock: %v", err)
	}
	t.Cleanup(func() {
		if err := held.Release(); err != nil {
			t.Errorf("release live-daemon fixture lock: %v", err)
		}
	})
	startedAt := time.Now().UTC().Add(-time.Minute)
	state := struct {
		PID          int       `json:"pid"`
		StartedAt    time.Time `json:"startedAt"`
		InstanceRoot string    `json:"instanceRoot"`
		Version      string    `json:"version"`
		HolderKind   string    `json:"holderKind"`
		HolderPID    int       `json:"holderPid"`
	}{
		PID:          os.Getpid(),
		StartedAt:    startedAt,
		InstanceRoot: root,
		Version:      "fixture",
		HolderKind:   "daemon",
		HolderPID:    os.Getpid(),
	}
	file := held.File()
	if err := file.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(state); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := daemonstate.Refresh(lockPath, time.Now()); err != nil {
		t.Fatalf("refresh live-daemon fixture heartbeat: %v", err)
	}
}

type runOperatorReadObservation struct {
	DaemonRunning   bool
	RecentLimit     int
	ListedRunIDs    map[string]struct{}
	Trace           []byte
	SupportingReads []string
}

func executeRunOperatorReads(
	t *testing.T,
	binary string,
	root string,
	commands []string,
) runOperatorReadObservation {
	t.Helper()
	observation := runOperatorReadObservation{ListedRunIDs: make(map[string]struct{})}
	for _, command := range commands {
		words := strings.Fields(command)
		if len(words) < 2 || words[0] != "<goobers>" {
			t.Fatalf("scenario command is not a local Goobers read: %q", command)
		}
		args := append([]string(nil), words[1:]...)
		for i, arg := range args {
			if arg == "<instance-root>" {
				args[i] = root
			}
			if strings.HasPrefix(arg, "--limit=") {
				limit, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
				if err != nil {
					t.Fatalf("parse scenario limit: %v", err)
				}
				observation.RecentLimit = limit
			}
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(binary, args...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v\n%s", command, err, stderr.String())
		}
		switch args[0] {
		case "runs":
			var output struct {
				Runs []struct {
					RunID string `json:"runId"`
				} `json:"runs"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("decode runs list output: %v\n%s", err, stdout.String())
			}
			for _, run := range output.Runs {
				observation.ListedRunIDs[run.RunID] = struct{}{}
			}
		case "status":
			observation.DaemonRunning = strings.Contains(stdout.String(), "daemon running:")
		case "trace":
			observation.Trace = append([]byte(nil), stdout.Bytes()...)
		case "escalations", "claims":
			// Supporting reads must succeed against the materialized instance
			// — `escalations show` in particular rejects a run whose current
			// phase is not escalated, which is what makes a stale pre-resume
			// classification observable rather than merely asserted.
			observation.SupportingReads = append(observation.SupportingReads, args[0])
		default:
			t.Fatalf("unsupported scenario read %q", command)
		}
	}
	return observation
}

type observedRunOperatorEvent struct {
	journal.Event
	KnownSchema *bool `json:"knownSchema,omitempty"`
}

// runOperatorRepassCount words a small repass count the way an operator answer
// reads, so the derived fact matches how the corpus states it.
func runOperatorRepassCount(repasses int) string {
	switch repasses {
	case 0:
		return "zero"
	case 1:
		return "one"
	case 2:
		return "two"
	default:
		return strconv.Itoa(repasses)
	}
}

// observedRunOperatorCause mirrors the trace `terminalCause` object the CLI
// emits, so a derived answer can cite the stable code and causal sequence
// rather than restating a human message.
type observedRunOperatorCause struct {
	Phase          string `json:"phase"`
	Stage          string `json:"stage"`
	Gate           string `json:"gate"`
	Code           string `json:"code"`
	CausalEventSeq uint64 `json:"causalEventSeq"`
}

func answerRunOperatorObservation(observation runOperatorReadObservation) (runOperatorExpected, error) {
	var trace struct {
		Identity      journal.RunIdentity        `json:"identity"`
		Phase         journal.RunPhase           `json:"phase"`
		Repasses      int                        `json:"repasses"`
		TerminalCause *observedRunOperatorCause  `json:"terminalCause"`
		Events        []observedRunOperatorEvent `json:"events"`
	}
	if err := json.Unmarshal(observation.Trace, &trace); err != nil {
		return runOperatorExpected{}, fmt.Errorf("decode observed trace: %w", err)
	}

	// Classification reads the current lifecycle segment only: the events at or
	// after the last run.resumed, or every event when the run was never
	// resumed. A journal keeps its pre-resume events, so a gate that escalated
	// before a resume is history rather than the current outcome, and treating
	// it as current would report a resumed-then-completed run as escalated.
	segment := trace.Events
	for i := range trace.Events {
		if trace.Events[i].Type == journal.EventRunResumed {
			segment = trace.Events[i:]
		}
	}

	// Journal integrity is assessed across every event: a gap or an unreadable
	// schema before a resume still limits what the run as a whole can prove.
	var uncertainty []string
	var previous uint64
	for i := range trace.Events {
		event := &trace.Events[i]
		if previous != 0 && event.Seq != previous+1 {
			uncertainty = append(uncertainty,
				fmt.Sprintf("Sequence %d is absent.", previous+1))
		}
		previous = event.Seq
		if event.KnownSchema != nil && !*event.KnownSchema {
			uncertainty = append(uncertainty,
				fmt.Sprintf("Sequence %d has an unknown schema.", event.Seq))
		}
	}

	// Outcome, by contrast, is read from the current segment only.
	var terminal *observedRunOperatorEvent
	var needsChanges, passed, escalated, noWork, abortGate *observedRunOperatorEvent
	for i := range segment {
		event := &segment[i]
		switch {
		case event.Type == journal.EventRunFinished:
			terminal = event
		case event.Type == journal.EventGateEvaluated && event.Escalated:
			escalated = event
		case event.Type == journal.EventGateEvaluated && event.Target == "abort":
			abortGate = event
		case event.Type == journal.EventGateEvaluated && event.Verdict == "needs-changes":
			needsChanges = event
		case event.Type == journal.EventGateEvaluated && event.Verdict == "pass":
			passed = event
		case event.Type == journal.EventStageFinished && event.Status == "no-work":
			noWork = event
		}
	}
	if terminal == nil && trace.Phase != journal.PhaseRunning {
		uncertainty = append(uncertainty, "No understood run.finished event is available.")
	}
	if len(uncertainty) > 0 {
		citation := runOperatorCitation{Source: "trace", RunID: trace.Identity.RunID}
		for i := range trace.Events {
			citation.Seqs = append(citation.Seqs, trace.Events[i].Seq)
		}
		if n := len(trace.Events); n > 0 {
			citation.Timestamp = trace.Events[n-1].Time.UTC().Format(time.RFC3339)
		}
		return runOperatorExpected{
			Classification: "uncertain",
			Facts:          []string{"The available evidence does not prove successful completion."},
			Citations:      []runOperatorCitation{citation},
			Uncertainty:    uncertainty,
		}, nil
	}
	if terminal == nil {
		return runOperatorExpected{}, fmt.Errorf("observed trace has no supported answer")
	}

	citation := runOperatorCitation{
		Source:    "trace",
		RunID:     trace.Identity.RunID,
		Timestamp: terminal.Time.UTC().Format(time.RFC3339),
	}

	// Terminal phases that are not a success are classified before the
	// success paths so that a failed, aborted, or escalated run can never fall
	// through to a "completed" answer.
	switch trace.Phase {
	case journal.PhaseFailed:
		cause := trace.TerminalCause
		if cause == nil || terminal.Status != string(journal.PhaseFailed) {
			return runOperatorExpected{}, fmt.Errorf("failed evidence is inconsistent")
		}
		citation.Seqs = []uint64{cause.CausalEventSeq, terminal.Seq}
		return runOperatorExpected{
			Classification: "failed",
			Facts: []string{
				fmt.Sprintf("Execution failed in %s with stable code %s.", cause.Stage, cause.Code),
				fmt.Sprintf("The terminal cause points to event sequence %d.", cause.CausalEventSeq),
			},
			Citations:   []runOperatorCitation{citation},
			Uncertainty: []string{},
		}, nil
	case journal.PhaseAborted:
		cause := trace.TerminalCause
		if cause == nil || terminal.Status != string(journal.PhaseAborted) || abortGate == nil {
			return runOperatorExpected{}, fmt.Errorf("aborted evidence is inconsistent")
		}
		citation.Seqs = []uint64{abortGate.Seq, terminal.Seq}
		return runOperatorExpected{
			Classification: "aborted",
			Facts: []string{
				"The run followed a defined abort branch; it did not fail or return no-work.",
			},
			Citations:   []runOperatorCitation{citation},
			Uncertainty: []string{},
		}, nil
	case journal.PhaseEscalated:
		if escalated == nil {
			return runOperatorExpected{}, fmt.Errorf("escalated phase has no escalating gate in the current segment")
		}
		// The structured selector comes from `escalations show`, so the
		// citation records both sources when that supporting read was made.
		if slices.Contains(observation.SupportingReads, "escalations") {
			citation.Source = "trace-and-escalation"
		}
		citation.Seqs = []uint64{escalated.Seq, terminal.Seq}
		return runOperatorExpected{
			Classification: "escalated",
			Facts: []string{
				fmt.Sprintf("The %s gate selected its escalation branch after %s repasses.",
					escalated.Gate, runOperatorRepassCount(trace.Repasses)),
				fmt.Sprintf("The escalation cause is the %s event at sequence %d.",
					escalated.Gate, escalated.Seq),
			},
			Citations:   []runOperatorCitation{citation},
			Uncertainty: []string{},
		}, nil
	}

	// A pre-resume escalation that the current segment does not carry must not
	// reach any success classification as an escalation; it is history only.
	if escalated != nil {
		return runOperatorExpected{}, fmt.Errorf(
			"gate %s escalated in the current segment but phase is %s", escalated.Gate, trace.Phase)
	}

	if noWork != nil {
		if trace.Phase != journal.PhaseCompleted || terminal.Status != string(journal.PhaseCompleted) {
			return runOperatorExpected{}, fmt.Errorf("no-work evidence is inconsistent")
		}
		citation.Seqs = []uint64{noWork.Seq, terminal.Seq}
		return runOperatorExpected{
			Classification: "no-work",
			Facts: []string{
				"The scheduled run completed successfully.",
				"The decisive stage correctly returned no-work; this was not a failure, abort, or skipped tick.",
			},
			Citations:   []runOperatorCitation{citation},
			Uncertainty: []string{},
		}, nil
	}

	if passed == nil {
		return runOperatorExpected{}, fmt.Errorf("observed trace has no supported answer")
	}
	if needsChanges != nil {
		if !observation.DaemonRunning {
			return runOperatorExpected{}, fmt.Errorf("live-daemon observation did not report a running daemon")
		}
		if trace.Phase != journal.PhaseCompleted ||
			terminal.Status != string(journal.PhaseCompleted) ||
			trace.Repasses != 1 ||
			needsChanges.Target != "implement" ||
			passed.Target != "done" {
			return runOperatorExpected{}, fmt.Errorf("reviewer-repass evidence is inconsistent")
		}
		citation.Seqs = []uint64{needsChanges.Seq, passed.Seq, terminal.Seq}
		return runOperatorExpected{
			Classification: "reviewer-repass",
			Facts: []string{
				"Review requested changes and routed back to implement.",
				"The later review passed and the run completed after one repass.",
				"Daemon liveness is separate from the workflow outcome.",
			},
			Citations:   []runOperatorCitation{citation},
			Uncertainty: []string{},
		}, nil
	}
	if _, ok := observation.ListedRunIDs[trace.Identity.RunID]; !ok {
		return runOperatorExpected{}, fmt.Errorf("bounded run list did not contain %s", trace.Identity.RunID)
	}
	if trace.Phase != journal.PhaseCompleted ||
		terminal.Status != string(journal.PhaseCompleted) ||
		trace.Repasses != 0 {
		return runOperatorExpected{}, fmt.Errorf("first-pass evidence is inconsistent")
	}
	citation.Seqs = []uint64{passed.Seq, terminal.Seq}
	return runOperatorExpected{
		Classification: "first-pass-success",
		Facts: []string{
			fmt.Sprintf("The bounded recent window was %d runs.", observation.RecentLimit),
			fmt.Sprintf("%s completed after %s passed with zero repasses.",
				trace.Identity.RunID, passed.Gate),
		},
		Citations:   []runOperatorCitation{citation},
		Uncertainty: []string{},
	}, nil
}
