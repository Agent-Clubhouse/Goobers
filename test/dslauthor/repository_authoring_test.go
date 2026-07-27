package dslauthor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
)

const (
	secretFixtureValue          = "FIXTURE_SECRET_MUST_NOT_APPEAR"
	recordedAuthoringPathSHA256 = "7a1f2732053c6b29bd2955a03718786e911989f2851d4c40dafc684f2a741be1"
)

type fixtureDocument struct {
	Scenarios  []fixtureScenario `json:"scenarios"`
	Unresolved []fixtureScenario `json:"unresolved"`
}

type fixtureScenario struct {
	Name             string            `json:"name"`
	Request          string            `json:"request"`
	Identity         string            `json:"identity"`
	Access           string            `json:"access"`
	DefaultBranch    string            `json:"defaultBranch"`
	ProviderRefs     map[string]string `json:"providerRefs"`
	ExistingConfig   bool              `json:"existingConfig"`
	ExistingFiles    map[string]string `json:"existingFiles"`
	RepositoryFiles  map[string]string `json:"repositoryFiles"`
	EvidencePaths    []string          `json:"evidencePaths"`
	GuidancePaths    []string          `json:"guidancePaths"`
	RecordedResponse recordedResponse  `json:"recordedResponse"`
}

type recordedResponse struct {
	Summary      string            `json:"summary"`
	Files        map[string]string `json:"files"`
	InitialFiles map[string]string `json:"initialFiles"`
	Report       authoringReport   `json:"report"`
}

type authoringReport struct {
	Status     string               `json:"status"`
	Request    string               `json:"request"`
	Contract   contractEvidence     `json:"contract"`
	Target     targetEvidence       `json:"target"`
	Terms      []string             `json:"terms"`
	Command    []string             `json:"command,omitempty"`
	Evidence   []repositoryEvidence `json:"evidence"`
	Proposal   proposalReport       `json:"proposal"`
	Release    releaseEvidence      `json:"release"`
	Validation validationReport     `json:"validation"`
	Diff       string               `json:"diff"`
	Unresolved []string             `json:"unresolved,omitempty"`
}

type contractEvidence struct {
	LoadedPaths []string `json:"loadedPaths"`
	SkillSHA256 string   `json:"skillSHA256"`
}

type targetEvidence struct {
	Identity string `json:"identity"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Access   string `json:"access"`
}

type repositoryEvidence struct {
	Conclusion string `json:"conclusion"`
	Citation   string `json:"citation"`
}

type proposalReport struct {
	PresentedBeforeWrite bool              `json:"presentedBeforeWrite"`
	StateGraph           string            `json:"stateGraph"`
	Paths                []string          `json:"paths"`
	Capabilities         []capabilityGrant `json:"capabilities"`
	OmittedCapabilities  []string          `json:"omittedCapabilities"`
}

type capabilityGrant struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type releaseEvidence struct {
	BinaryPath       string `json:"binaryPath"`
	Version          string `json:"version"`
	Commit           string `json:"commit"`
	DSLVersion       string `json:"dslVersion"`
	CanonicalExample string `json:"canonicalExample"`
	ExampleReason    string `json:"exampleReason"`
}

type validationReport struct {
	Command  []string            `json:"command"`
	Attempts []validationAttempt `json:"attempts"`
	Status   string              `json:"status"`
}

type validationAttempt struct {
	Status  string `json:"status"`
	Finding string `json:"finding,omitempty"`
}

type resolvedTarget struct {
	Identity string `json:"identity"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Access   string `json:"access"`
	Root     string `json:"root,omitempty"`
}

type selectedBinary struct {
	path        string
	version     string
	commit      string
	dslVersions []supportmatrix.Version
	calls       []string
}

type recordedCopilotRunner struct {
	t                 *testing.T
	scenario          fixtureScenario
	response          recordedResponse
	binary            *selectedBinary
	target            resolvedTarget
	prompt            string
	events            []string
	providerCalls     []string
	validationResults []bool
}

func TestRepositoryAwareGoldenScenarios(t *testing.T) {
	binaryPath := buildSelectedBinary(t)
	for _, scenario := range loadScenarios(t).Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			runRecordedScenario(t, binaryPath, scenario)
		})
	}
}

func TestRepositoryAwareUnresolvedScenario(t *testing.T) {
	binaryPath := buildSelectedBinary(t)
	for _, scenario := range loadScenarios(t).Unresolved {
		t.Run(scenario.Name, func(t *testing.T) {
			runRecordedScenario(t, binaryPath, scenario)
		})
	}
}

func TestRepositoryAwareFixturesCoverAcceptanceMatrix(t *testing.T) {
	var names []string
	for _, scenario := range loadScenarios(t).Scenarios {
		names = append(names, scenario.Name)
		if strings.TrimSpace(scenario.Request) == "" {
			t.Errorf("scenario %q has no plain-English request", scenario.Name)
		}
	}
	sort.Strings(names)
	want := []string{"existing config", "go service", "node app", "remote-only target", "static documentation repo"}
	if !slices.Equal(names, want) {
		t.Fatalf("scenarios = %v, want %v", names, want)
	}
}

func runRecordedScenario(t *testing.T, binaryPath string, scenario fixtureScenario) {
	t.Helper()
	binary := openSelectedBinary(t, binaryPath)
	root, target := prepareWorkspace(t, scenario, &binary)
	loadedPaths, skillDigest := loadPackagedAuthoringPath(t, root)
	if skillDigest != recordedAuthoringPathSHA256 {
		t.Fatalf("installed authoring path digest = %s; update the recorded agent responses intentionally", skillDigest)
	}
	dslVersion := binary.selectDSL(t)
	response := expandRecordedResponse(t, scenario.RecordedResponse, map[string]string{
		"{binaryPath}":    binary.path,
		"{binaryVersion}": binary.version,
		"{binaryCommit}":  binary.commit,
		"{dslVersion}":    dslVersion,
		"{workspace}":     root,
		"{targetCommit}":  target.Commit,
		"{skillDigest}":   recordedAuthoringPathSHA256,
	})
	runner := &recordedCopilotRunner{
		t:        t,
		scenario: scenario,
		response: response,
		binary:   &binary,
		target:   target,
	}
	adapter := &harness.CopilotAdapter{
		Command:   []string{"copilot"},
		ExtraArgs: []string{},
		Runner:    runner,
	}
	outcome, err := adapter.Run(context.Background(), harness.RunRequest{
		Mode: harness.ModeInvoke,
		Envelope: apiv1.InvocationEnvelope{
			TaskID:     "dsl-author-fixture",
			WorkflowID: "repository-authoring",
			RunID:      "golden-" + strings.ReplaceAll(scenario.Name, " ", "-"),
			Gaggle:     "agent-toolkit",
			Goal:       scenario.Request,
			Workspace:  root,
			ContextPointers: []apiv1.ContextPointer{{
				Name: "environment-resolver-report",
			}},
		},
		Workspace:      root,
		CompletionPath: harness.DefaultResultPath,
		ContextPaths: map[string]string{
			"environment-resolver-report": ".goobers/context/environment-report.json",
		},
	})
	if err != nil {
		t.Fatalf("run packaged authoring path: %v", err)
	}

	var result apiv1.ResultEnvelope
	if err := json.Unmarshal(outcome.Payload, &result); err != nil {
		t.Fatalf("decode authoring completion: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("authoring completion status = %q", result.Status)
	}
	if result.Outputs["reportPath"] != ".goobers/authoring-report.json" {
		t.Fatalf("authoring report path = %v", result.Outputs["reportPath"])
	}

	reportData, err := os.ReadFile(filepath.Join(root, ".goobers", "authoring-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report authoringReport
	if err := json.Unmarshal(reportData, &report); err != nil {
		t.Fatalf("decode authoring report: %v", err)
	}
	expected := response.Report
	expected.Diff = workspaceDiff(t, root)
	if !reflect.DeepEqual(report, expected) {
		t.Fatalf("authoring report differs from recorded golden\n got: %+v\nwant: %+v", report, expected)
	}
	evaluateAuthoringResult(t, scenario, root, target, loadedPaths, &binary, runner, report)
}

func (r *recordedCopilotRunner) Run(_ context.Context, request harness.ProcessRequest) (harness.ProcessResult, error) {
	r.t.Helper()
	promptIndex := slices.Index(request.Command, "-p")
	if promptIndex < 0 || promptIndex+1 >= len(request.Command) {
		return harness.ProcessResult{}, fmt.Errorf("recorded Copilot invocation has no prompt")
	}
	r.prompt = request.Command[promptIndex+1]
	if !strings.Contains(r.prompt, r.scenario.Request) ||
		!strings.Contains(r.prompt, ".goobers/context/environment-report.json") {
		return harness.ProcessResult{}, fmt.Errorf("recorded Copilot prompt omitted request or resolver report")
	}

	loadedPaths, digest := loadPackagedAuthoringPath(r.t, request.Dir)
	if !slices.Equal(loadedPaths, r.response.Report.Contract.LoadedPaths) ||
		digest != r.response.Report.Contract.SkillSHA256 {
		return harness.ProcessResult{}, fmt.Errorf("recorded response does not match installed authoring path")
	}
	check := string(r.binary.run(r.t, "agent-kit", "check", request.Dir))
	if !strings.Contains(check, "state: current") {
		return harness.ProcessResult{}, fmt.Errorf("installed toolkit is not current: %s", check)
	}
	r.inspectReleaseContract()
	if err := r.readRepositoryEvidence(); err != nil {
		return harness.ProcessResult{}, err
	}

	r.events = append(r.events, "present-proposal")
	report := r.response.Report
	if report.Status == "unresolved" {
		r.events = append(r.events, "return-unresolved")
		report.Diff = ""
		if err := writeJSON(filepath.Join(request.Dir, ".goobers", "authoring-report.json"), report); err != nil {
			return harness.ProcessResult{}, err
		}
		return r.complete(request.Dir, r.response.Summary)
	}

	writeRecordedFiles(r.t, request.Dir, r.response.Files)
	r.events = append(r.events, "write-config")
	if len(r.response.InitialFiles) > 0 {
		writeRecordedFiles(r.t, request.Dir, r.response.InitialFiles)
		r.validationResults = append(r.validationResults, r.binary.validate(r.t, request.Dir, r.scenario.ExistingConfig, false))
		r.events = append(r.events, "validate-invalid", "repair-config")
		for path := range r.response.InitialFiles {
			body, ok := r.response.Files[path]
			if !ok {
				return harness.ProcessResult{}, fmt.Errorf("repair fixture %s has no final file", path)
			}
			writeFile(r.t, filepath.Join(request.Dir, filepath.FromSlash(path)), body)
		}
	}
	r.validationResults = append(r.validationResults, r.binary.validate(r.t, request.Dir, r.scenario.ExistingConfig, true))
	r.events = append(r.events, "validate-ready")

	for _, path := range report.Proposal.Paths {
		runGit(r.t, request.Dir, "add", "-N", "--", filepath.FromSlash(path))
	}
	report.Diff = workspaceDiff(r.t, request.Dir)
	if err := writeJSON(filepath.Join(request.Dir, ".goobers", "authoring-report.json"), report); err != nil {
		return harness.ProcessResult{}, err
	}
	return r.complete(request.Dir, r.response.Summary)
}

func (r *recordedCopilotRunner) complete(root, summary string) (harness.ProcessResult, error) {
	err := harness.WriteCompletion(root, harness.DefaultResultPath, apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{"reportPath": ".goobers/authoring-report.json"},
		Summary: summary,
	})
	return harness.ProcessResult{ExitCode: 0, Transcript: []byte("recorded Copilot authoring fixture\n")}, err
}

func (r *recordedCopilotRunner) inspectReleaseContract() {
	r.t.Helper()
	features := r.binary.run(r.t, "features", "--json", "--dsl-version", r.response.Report.Release.DSLVersion)
	if !json.Valid(features) {
		r.t.Fatal("selected binary returned invalid feature registry")
	}
	examples := strings.Fields(string(r.binary.run(r.t, "examples", "list")))
	if !slices.Contains(examples, r.response.Report.Release.CanonicalExample) {
		r.t.Fatalf("canonical example %q is not in %v", r.response.Report.Release.CanonicalExample, examples)
	}
	example := r.binary.run(r.t, "examples", "show", r.response.Report.Release.CanonicalExample)
	if !strings.Contains(string(example), "kind: Workflow") {
		r.t.Fatalf("canonical example %q is not a workflow", r.response.Report.Release.CanonicalExample)
	}
}

func (r *recordedCopilotRunner) readRepositoryEvidence() error {
	r.t.Helper()
	paths := append(append([]string(nil), r.scenario.EvidencePaths...), r.scenario.GuidancePaths...)
	sort.Strings(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if unsafeEvidencePath(path) {
			return fmt.Errorf("recorded authoring path requested unsafe evidence %q", path)
		}
		switch r.target.Access {
		case "local":
			runGit(r.t, r.target.Root, "show", r.target.Commit+":"+path)
		case "remote":
			r.providerCalls = append(r.providerCalls, "read "+r.target.Identity+"@"+r.target.Commit+":"+path)
			if _, ok := r.scenario.RepositoryFiles[path]; !ok {
				return fmt.Errorf("remote fixture has no evidence path %q", path)
			}
		default:
			return fmt.Errorf("unsupported target access %q", r.target.Access)
		}
	}
	return nil
}

func evaluateAuthoringResult(
	t *testing.T,
	scenario fixtureScenario,
	root string,
	target resolvedTarget,
	loadedPaths []string,
	binary *selectedBinary,
	runner *recordedCopilotRunner,
	report authoringReport,
) {
	t.Helper()
	if report.Request != scenario.Request || report.Target.Identity != target.Identity ||
		report.Target.Branch != target.Branch || report.Target.Commit != target.Commit ||
		report.Target.Access != target.Access {
		t.Fatalf("report request or target does not match fixture: %+v", report.Target)
	}
	if !slices.Equal(report.Contract.LoadedPaths, loadedPaths) || report.Contract.SkillSHA256 == "" {
		t.Fatalf("report did not identify the complete packaged authoring path: %+v", report.Contract)
	}
	if !report.Proposal.PresentedBeforeWrite || len(report.Terms) == 0 ||
		report.Release.ExampleReason == "" {
		t.Fatal("report omitted the pre-write explanation, relevant terms, or closest-example rationale")
	}
	if len(runner.events) == 0 || runner.events[0] != "present-proposal" {
		t.Fatalf("authoring event order = %v", runner.events)
	}
	assertEvidenceCitations(t, scenario, target, report.Evidence)
	assertReleaseCalls(t, binary.calls, report.Release)
	assertProviderAccess(t, scenario, target, runner.providerCalls)
	assertSecretFreeWorkspace(t, root)

	changed := diffNames(t, root)
	if !slices.Equal(changed, report.Proposal.Paths) {
		t.Fatalf("workspace diff paths = %v, proposed paths = %v", changed, report.Proposal.Paths)
	}
	if report.Diff != workspaceDiff(t, root) {
		t.Fatal("reported reviewable diff differs from the workspace diff")
	}

	if report.Status == "unresolved" {
		if len(changed) != 0 || len(report.Unresolved) == 0 ||
			report.Validation.Status != "unresolved" || len(runner.validationResults) != 0 {
			t.Fatalf("unresolved report wrote config or omitted diagnostics: %+v", report)
		}
		return
	}
	if report.Status != "ready" || report.Validation.Status != "ready" {
		t.Fatalf("ready fixture status = %q, validation = %q", report.Status, report.Validation.Status)
	}
	wantValidation := make([]bool, 0, len(report.Validation.Attempts))
	for _, attempt := range report.Validation.Attempts {
		switch attempt.Status {
		case "invalid":
			wantValidation = append(wantValidation, false)
			if attempt.Finding == "" {
				t.Fatal("invalid validation attempt omitted its finding")
			}
		case "ready":
			wantValidation = append(wantValidation, true)
		default:
			t.Fatalf("unknown validation attempt status %q", attempt.Status)
		}
	}
	if !slices.Equal(runner.validationResults, wantValidation) {
		t.Fatalf("validator results = %v, report attempts = %v", runner.validationResults, wantValidation)
	}

	configDir := root
	if scenario.ExistingConfig {
		configDir = filepath.Join(root, "config")
	}
	set, loadReport, err := instance.LoadConfigDir(configDir)
	if err != nil {
		t.Fatalf("load generated config: %v (report: %+v)", err, loadReport)
	}
	workflow := findWorkflow(t, set, "repository-check")
	if graph := renderGraph(workflow); graph != report.Proposal.StateGraph {
		t.Fatalf("generated state graph = %q, report = %q", graph, report.Proposal.StateGraph)
	}
	assertWorkflowCommand(t, workflow, report.Command)
	capabilities := configCapabilities(set, workflow)
	if grants := grantNames(report.Proposal.Capabilities); !slices.Equal(capabilities, grants) {
		t.Fatalf("generated capabilities = %v, report grants = %v", capabilities, grants)
	}
	for _, grant := range report.Proposal.Capabilities {
		if strings.TrimSpace(grant.Reason) == "" {
			t.Errorf("capability %q has no least-privilege explanation", grant.Name)
		}
	}
	for _, omitted := range report.Proposal.OmittedCapabilities {
		if slices.Contains(capabilities, omitted) {
			t.Errorf("omitted stronger capability %q was generated", omitted)
		}
	}
}

func assertProviderAccess(t *testing.T, scenario fixtureScenario, target resolvedTarget, calls []string) {
	t.Helper()
	if target.Access != "remote" {
		if len(calls) != 0 {
			t.Fatalf("local target made provider calls: %v", calls)
		}
		return
	}
	if target.Root != "" {
		t.Fatalf("remote-only target unexpectedly has a local checkout at %q", target.Root)
	}
	paths := append(append([]string(nil), scenario.EvidencePaths...), scenario.GuidancePaths...)
	sort.Strings(paths)
	paths = slices.Compact(paths)
	if len(calls) != len(paths) {
		t.Fatalf("remote provider calls = %v, want one exact-ref read per evidence path %v", calls, paths)
	}
	for index, path := range paths {
		want := "read " + target.Identity + "@" + target.Commit + ":" + path
		if calls[index] != want {
			t.Errorf("remote provider call = %q, want %q", calls[index], want)
		}
	}
}

func assertEvidenceCitations(t *testing.T, scenario fixtureScenario, target resolvedTarget, evidence []repositoryEvidence) {
	t.Helper()
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, path := range append(append([]string(nil), scenario.EvidencePaths...), scenario.GuidancePaths...) {
		if !strings.Contains(text, path+":") {
			t.Errorf("repository evidence does not cite %q: %s", path, text)
		}
	}
	if target.Access == "remote" {
		prefix := target.Identity + "@" + target.Commit + ":"
		for _, item := range evidence {
			if !strings.Contains(item.Citation, prefix) {
				t.Errorf("remote evidence %q does not cite exact ref %q", item.Citation, prefix)
			}
		}
	}
	for _, forbidden := range []string{secretFixtureValue, ".env", "secrets.API_TOKEN", "oauth2:"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("evidence contains forbidden credential material %q", forbidden)
		}
	}
}

func assertReleaseCalls(t *testing.T, calls []string, release releaseEvidence) {
	t.Helper()
	for _, want := range []string{
		"version --json",
		"versions --json",
		"agent-kit check ",
		"features --json --dsl-version " + release.DSLVersion,
		"examples list",
		"examples show " + release.CanonicalExample,
	} {
		if !slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, want) }) {
			t.Errorf("selected binary calls %v do not contain %q", calls, want)
		}
	}
}

func buildSelectedBinary(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "goobers")
	command := exec.Command("go", "build", "-o", path, "./cmd/goobers")
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build selected goobers binary: %v\n%s", err, output)
	}
	return path
}

func openSelectedBinary(t *testing.T, path string) selectedBinary {
	t.Helper()
	binary := selectedBinary{path: path}
	var identity struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.Unmarshal(binary.run(t, "version", "--json"), &identity); err != nil {
		t.Fatalf("decode selected binary identity: %v", err)
	}
	var versions struct {
		DSLVersions []supportmatrix.Version `json:"dslVersions"`
	}
	if err := json.Unmarshal(binary.run(t, "versions", "--json"), &versions); err != nil {
		t.Fatalf("decode selected binary DSL versions: %v", err)
	}
	if identity.Version == "" || identity.Commit == "" || len(versions.DSLVersions) == 0 {
		t.Fatalf("selected binary identity or DSL support is incomplete")
	}
	binary.version, binary.commit, binary.dslVersions = identity.Version, identity.Commit, versions.DSLVersions
	return binary
}

func (b *selectedBinary) run(t *testing.T, args ...string) []byte {
	t.Helper()
	output, err := b.runResult(args...)
	if err == nil {
		return output
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		t.Fatalf("selected binary %q failed: %v\n%s", strings.Join(args, " "), err, exitError.Stderr)
	}
	t.Fatalf("run selected binary %q: %v", strings.Join(args, " "), err)
	return nil
}

func (b *selectedBinary) runResult(args ...string) ([]byte, error) {
	b.calls = append(b.calls, strings.Join(args, " "))
	command := exec.Command(b.path, args...)
	return command.Output()
}

func (b *selectedBinary) selectDSL(t *testing.T) string {
	t.Helper()
	for index := len(b.dslVersions) - 1; index >= 0; index-- {
		if b.dslVersions[index].Level != supportmatrix.LevelUnsupported {
			return b.dslVersions[index].Version
		}
	}
	t.Fatal("selected binary reports no supported DSL version")
	return ""
}

func (b *selectedBinary) validate(t *testing.T, root string, existing, wantOK bool) bool {
	t.Helper()
	args := []string{"validate", "--json", "--source-tree", root}
	if existing {
		args = []string{"validate", "--json", root}
	}
	data, commandErr := b.runResult(args...)
	var report struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode selected validator result: %v\n%s", err, data)
	}
	if report.OK != wantOK || (wantOK && commandErr != nil) || (!wantOK && commandErr == nil) {
		t.Fatalf("validator ok = %v, error = %v; want ok = %v\n%s", report.OK, commandErr, wantOK, data)
	}
	return report.OK
}

func prepareWorkspace(t *testing.T, scenario fixtureScenario, binary *selectedBinary) (string, resolvedTarget) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q", "-b", "main")
	for path, body := range scenario.ExistingFiles {
		writeFile(t, filepath.Join(root, filepath.FromSlash(path)), body)
	}
	target := resolveFixtureTarget(t, scenario)
	reportData, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".goobers", "context", "environment-report.json"), string(reportData)+"\n")
	binary.run(t, "agent-kit", "install", "--harness", "copilot", root)
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture baseline")
	return root, target
}

func resolveFixtureTarget(t *testing.T, scenario fixtureScenario) resolvedTarget {
	t.Helper()
	target := resolvedTarget{
		Identity: scenario.Identity,
		Branch:   scenario.RecordedResponse.Report.Target.Branch,
		Access:   scenario.Access,
	}
	switch scenario.Access {
	case "local":
		target.Root, target.Commit = materializeLocalRepository(t, scenario, target.Branch)
	case "remote":
		target.Commit = scenario.ProviderRefs["branch:"+target.Branch]
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(target.Commit) {
			t.Fatalf("remote target branch %q has no full fixture commit", target.Branch)
		}
	default:
		t.Fatalf("unsupported fixture access %q", scenario.Access)
	}
	return target
}

func materializeLocalRepository(t *testing.T, scenario fixtureScenario, branch string) (string, string) {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", branch)
	for path, body := range scenario.RepositoryFiles {
		writeFile(t, filepath.Join(root, filepath.FromSlash(path)), body)
	}
	parts := strings.Split(scenario.Identity, "/")
	if len(parts) != 3 {
		t.Fatalf("invalid fixture identity %q", scenario.Identity)
	}
	runGit(t, root, "remote", "add", "origin", "https://github.com/"+parts[1]+"/"+parts[2]+".git")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture target")
	commit := strings.TrimSpace(string(runGit(t, root, "rev-parse", "HEAD")))
	defaultBranch := scenario.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = branch
	}
	runGit(t, root, "update-ref", "refs/heads/"+defaultBranch, commit)
	runGit(t, root, "update-ref", "refs/remotes/origin/"+defaultBranch, commit)
	runGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+defaultBranch)
	return root, commit
}

func loadPackagedAuthoringPath(t *testing.T, root string) ([]string, string) {
	t.Helper()
	paths := []string{
		".github/copilot-instructions.md",
		".goobers/agent-toolkit/adapters/copilot.md",
		".goobers/agent-toolkit/instructions/goobers.md",
		".goobers/agent-toolkit/skills/goobers-environment-resolver/SKILL.md",
		".goobers/agent-toolkit/skills/goobers-dsl-author/SKILL.md",
		".goobers/agent-toolkit/skills/goobers-dsl-author/references/repository-authoring.md",
	}
	hash := sha256.New()
	bodies := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read installed authoring path %s: %v", path, err)
		}
		bodies[path] = string(data)
		hash.Write([]byte(path))
		hash.Write([]byte{0})
		hash.Write(data)
	}
	if !strings.Contains(bodies[paths[0]], ".goobers/agent-toolkit/adapters/copilot.md") ||
		!strings.Contains(bodies[paths[1]], "skills/goobers-dsl-author/SKILL.md") ||
		!strings.Contains(bodies[paths[2]], "goobers-environment-resolver") ||
		!strings.Contains(bodies[paths[4]], "references/repository-authoring.md") {
		t.Fatal("installed adapter chain does not load the repository-aware authoring path")
	}
	return paths, fmt.Sprintf("%x", hash.Sum(nil))
}

func expandRecordedResponse(t *testing.T, response recordedResponse, replacements map[string]string) recordedResponse {
	t.Helper()
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for token, value := range replacements {
		text = strings.ReplaceAll(text, token, value)
	}
	var expanded recordedResponse
	if err := json.Unmarshal([]byte(text), &expanded); err != nil {
		t.Fatalf("expand recorded response: %v", err)
	}
	return expanded
}

func writeRecordedFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, body := range files {
		if path == "" || filepath.IsAbs(path) || strings.Contains(filepath.ToSlash(path), "../") {
			t.Fatalf("unsafe recorded output path %q", path)
		}
		writeFile(t, filepath.Join(root, filepath.FromSlash(path)), body)
	}
}

func unsafeEvidencePath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == ".env" || strings.HasPrefix(base, ".env.") ||
		strings.Contains(base, "credentials") || strings.Contains(base, "auth")
}

func workspaceDiff(t *testing.T, root string) string {
	t.Helper()
	return string(runGit(t, root, "diff", "--no-ext-diff", "--"))
}

func diffNames(t *testing.T, root string) []string {
	t.Helper()
	output := strings.TrimSpace(string(runGit(t, root, "diff", "--name-only", "--")))
	if output == "" {
		return nil
	}
	names := strings.Split(output, "\n")
	sort.Strings(names)
	return names
}

func assertSecretFreeWorkspace(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), secretFixtureValue) {
			t.Errorf("%s contains fixture secret value", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func findWorkflow(t *testing.T, set *instance.ConfigSet, name string) apiv1.Workflow {
	t.Helper()
	for _, workflow := range set.Workflows {
		if workflow.Name == name {
			return workflow
		}
	}
	t.Fatalf("workflow %q was not loaded", name)
	return apiv1.Workflow{}
}

func renderGraph(workflow apiv1.Workflow) string {
	parts := []string{workflow.Spec.Start}
	for _, task := range workflow.Spec.Tasks {
		if task.Name == workflow.Spec.Start && task.Next != "" {
			parts = append(parts, task.Next)
			break
		}
	}
	for _, gate := range workflow.Spec.Gates {
		if gate.Name != parts[len(parts)-1] {
			continue
		}
		outcomes := sortedKeys(gate.Branches)
		branches := make([]string, 0, len(outcomes))
		for _, outcome := range outcomes {
			branches = append(branches, outcome+":"+gate.Branches[outcome])
		}
		parts[len(parts)-1] += "(" + strings.Join(branches, ", ") + ")"
	}
	return strings.Join(parts, " -> ")
}

func configCapabilities(set *instance.ConfigSet, workflow apiv1.Workflow) []string {
	unique := map[string]bool{}
	for _, task := range workflow.Spec.Tasks {
		for _, capability := range task.Capabilities {
			unique[capability] = true
		}
	}
	for _, goober := range set.Goobers {
		if slices.Contains(goober.Spec.Workflows, workflow.Name) {
			for _, capability := range goober.Spec.Capabilities {
				unique[capability] = true
			}
		}
	}
	return sortedKeys(unique)
}

func grantNames(grants []capabilityGrant) []string {
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		names = append(names, grant.Name)
	}
	sort.Strings(names)
	return names
}

func assertWorkflowCommand(t *testing.T, workflow apiv1.Workflow, want []string) {
	t.Helper()
	for _, task := range workflow.Spec.Tasks {
		if task.Name == workflow.Spec.Start && task.Run != nil {
			if !slices.Equal(task.Run.Command, want) {
				t.Fatalf("workflow command = %v, report = %v", task.Run.Command, want)
			}
			return
		}
	}
	t.Fatalf("workflow start task %q has no deterministic command", workflow.Spec.Start)
}

func loadScenarios(t *testing.T) fixtureDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "repository-scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document fixtureDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func writeJSON(path string, value interface{}) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.Output()
	if err == nil {
		return output
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		t.Fatalf("git command failed: %v\n%s", err, exitError.Stderr)
	}
	t.Fatalf("run git: %v", err)
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
