package dslauthor

import (
	"bytes"
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
	Name            string            `json:"name"`
	Request         string            `json:"request"`
	Identity        string            `json:"identity"`
	Access          string            `json:"access"`
	DefaultBranch   string            `json:"defaultBranch"`
	ProviderRefs    map[string]string `json:"providerRefs"`
	ExistingConfig  bool              `json:"existingConfig"`
	ExistingFiles   map[string]string `json:"existingFiles"`
	RepositoryFiles map[string]string `json:"repositoryFiles"`
	EvidencePaths   []string          `json:"evidencePaths"`
	GuidancePaths   []string          `json:"guidancePaths"`
	Capture         recordedResponse  `json:"recordedResponse"`
}

type recordingDocument struct {
	Recordings []invocationRecording `json:"recordings"`
}

type invocationRecording struct {
	Name           string                `json:"name"`
	CapturedWith   string                `json:"capturedWith"`
	CapturedAt     string                `json:"capturedAt"`
	Model          string                `json:"model"`
	SessionID      string                `json:"sessionId"`
	SkillSHA256    string                `json:"skillSHA256"`
	PromptSHA256   string                `json:"promptSHA256"`
	ResponseSHA256 string                `json:"responseSHA256"`
	DiffSHA256     string                `json:"diffSHA256"`
	Interactions   []recordedInteraction `json:"interactions"`
}

type recordedInteraction struct {
	Action   string   `json:"action"`
	Tool     string   `json:"tool,omitempty"`
	Path     string   `json:"path,omitempty"`
	Args     []string `json:"args,omitempty"`
	Source   string   `json:"source,omitempty"`
	Contains string   `json:"contains,omitempty"`
	WantOK   *bool    `json:"wantOK,omitempty"`
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

type copilotReplayRunner struct {
	t                 *testing.T
	scenario          fixtureScenario
	recording         invocationRecording
	response          recordedResponse
	binary            *selectedBinary
	target            resolvedTarget
	prompt            string
	events            []string
	providerCalls     []string
	validationResults []bool
	skillReads        []string
	writtenFinal      map[string]bool
	writtenDraft      map[string]bool
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
	t.Setenv("HOME", t.TempDir())
	binary := openSelectedBinary(t, binaryPath)
	root, target := prepareWorkspace(t, scenario, &binary)
	loadedPaths, skillDigest := loadPackagedAuthoringPath(t, root)
	if skillDigest != recordedAuthoringPathSHA256 {
		t.Fatalf("installed authoring path digest = %s; update the recorded agent responses intentionally", skillDigest)
	}
	recording := loadRecording(t, scenario.Name)
	assertRecordingProvenance(t, recording, scenario.Capture)
	dslVersion := binary.selectDSL(t)
	response := expandRecordedResponse(t, scenario.Capture, map[string]string{
		"{binaryPath}":    binary.path,
		"{binaryVersion}": binary.version,
		"{binaryCommit}":  binary.commit,
		"{dslVersion}":    dslVersion,
		"{workspace}":     root,
		"{targetCommit}":  target.Commit,
		"{skillDigest}":   recordedAuthoringPathSHA256,
	})
	runner := &copilotReplayRunner{
		t:            t,
		scenario:     scenario,
		recording:    recording,
		response:     response,
		binary:       &binary,
		target:       target,
		writtenFinal: map[string]bool{},
		writtenDraft: map[string]bool{},
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
		Model:          recording.Model,
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
	assertRecordedTranscript(t, recording, outcome)

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

func (r *copilotReplayRunner) Run(_ context.Context, request harness.ProcessRequest) (harness.ProcessResult, error) {
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
	if got := normalizedPromptSHA256(r.prompt, request.Dir, r.target, r.binary.path); got != r.recording.PromptSHA256 {
		return harness.ProcessResult{}, fmt.Errorf("recorded Copilot prompt digest = %s, want %s", got, r.recording.PromptSHA256)
	}

	loadedPaths, digest := loadPackagedAuthoringPath(r.t, request.Dir)
	if !slices.Equal(loadedPaths, r.response.Report.Contract.LoadedPaths) ||
		digest != r.response.Report.Contract.SkillSHA256 ||
		digest != r.recording.SkillSHA256 {
		return harness.ProcessResult{}, fmt.Errorf("recorded response does not match installed authoring path")
	}
	if err := r.replayInteractions(request); err != nil {
		return harness.ProcessResult{}, err
	}
	if err := writeReplaySessionLog(request, r.recording, r.prompt); err != nil {
		return harness.ProcessResult{}, err
	}
	return harness.ProcessResult{ExitCode: 0, Transcript: []byte("Copilot invocation replay\n")}, nil
}

func (r *copilotReplayRunner) complete(root, summary string) error {
	return harness.WriteCompletion(root, harness.DefaultResultPath, apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{"reportPath": ".goobers/authoring-report.json"},
		Summary: summary,
	})
}

func (r *copilotReplayRunner) replayInteractions(request harness.ProcessRequest) error {
	r.t.Helper()
	presented, reported, completed := false, false, false
	for _, interaction := range r.recording.Interactions {
		switch interaction.Action {
		case "skill.read":
			if _, err := os.ReadFile(filepath.Join(request.Dir, filepath.FromSlash(interaction.Path))); err != nil {
				return fmt.Errorf("replay skill read %q: %w", interaction.Path, err)
			}
			r.skillReads = append(r.skillReads, interaction.Path)
		case "repository.read":
			if r.target.Access != "local" {
				return fmt.Errorf("repository read recorded for remote target")
			}
			if unsafeEvidencePath(interaction.Path) {
				return fmt.Errorf("recorded authoring path requested unsafe evidence %q", interaction.Path)
			}
			runGit(r.t, r.target.Root, "show", r.target.Commit+":"+interaction.Path)
		case "provider.read":
			if r.target.Access != "remote" {
				return fmt.Errorf("provider read recorded for local target")
			}
			if unsafeEvidencePath(interaction.Path) {
				return fmt.Errorf("recorded authoring path requested unsafe evidence %q", interaction.Path)
			}
			if _, ok := r.scenario.RepositoryFiles[interaction.Path]; !ok {
				return fmt.Errorf("remote fixture has no evidence path %q", interaction.Path)
			}
			r.providerCalls = append(r.providerCalls, "read "+r.target.Identity+"@"+r.target.Commit+":"+interaction.Path)
		case "goobers.run":
			args := expandInteractionArgs(interaction.Args, request.Dir, r.response)
			output := r.binary.run(r.t, args...)
			if interaction.Contains != "" && !strings.Contains(string(output), interaction.Contains) {
				return fmt.Errorf("recorded command %q output omitted %q", strings.Join(args, " "), interaction.Contains)
			}
			if args[0] == "features" && !json.Valid(output) {
				return fmt.Errorf("selected binary returned invalid feature registry")
			}
		case "proposal.present":
			if presented {
				return fmt.Errorf("recorded proposal was presented more than once")
			}
			presented = true
			r.events = append(r.events, "present-proposal")
		case "config.write":
			if !presented {
				return fmt.Errorf("recorded config write preceded proposal")
			}
			files, written := r.response.Files, r.writtenFinal
			if interaction.Source == "draft" {
				files, written = r.response.InitialFiles, r.writtenDraft
			}
			body, ok := files[interaction.Path]
			if !ok {
				return fmt.Errorf("recorded %s output has no file %q", interaction.Source, interaction.Path)
			}
			writeFile(r.t, filepath.Join(request.Dir, filepath.FromSlash(interaction.Path)), body)
			written[interaction.Path] = true
			r.events = append(r.events, "write-"+interaction.Source)
		case "config.validate":
			if interaction.WantOK == nil {
				return fmt.Errorf("recorded validation omitted wantOK")
			}
			ok := r.binary.validate(r.t, request.Dir, r.scenario.ExistingConfig, *interaction.WantOK)
			r.validationResults = append(r.validationResults, ok)
			if ok {
				r.events = append(r.events, "validate-ready")
			} else {
				r.events = append(r.events, "validate-invalid")
			}
		case "report.write":
			report := r.response.Report
			if report.Status == "unresolved" {
				report.Diff = ""
				r.events = append(r.events, "return-unresolved")
			} else {
				for _, path := range report.Proposal.Paths {
					runGit(r.t, request.Dir, "add", "-N", "--", filepath.FromSlash(path))
				}
				report.Diff = workspaceDiff(r.t, request.Dir)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256([]byte(report.Diff))); got != r.recording.DiffSHA256 {
				return fmt.Errorf("replayed diff digest = %s, want %s", got, r.recording.DiffSHA256)
			}
			if err := writeJSON(filepath.Join(request.Dir, ".goobers", "authoring-report.json"), report); err != nil {
				return err
			}
			reported = true
		case "completion.write":
			if !reported {
				return fmt.Errorf("recorded completion preceded report")
			}
			if err := r.complete(request.Dir, r.response.Summary); err != nil {
				return err
			}
			completed = true
		default:
			return fmt.Errorf("unknown recorded interaction %q", interaction.Action)
		}
	}
	if !reported || !completed {
		return fmt.Errorf("recording omitted report or completion")
	}
	for path := range r.response.Files {
		if !r.writtenFinal[path] {
			return fmt.Errorf("recording did not replay final output %q", path)
		}
	}
	for path := range r.response.InitialFiles {
		if !r.writtenDraft[path] {
			return fmt.Errorf("recording did not replay draft output %q", path)
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
	runner *copilotReplayRunner,
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
	if !slices.Equal(runner.skillReads, loadedPaths) {
		t.Fatalf("recorded skill reads = %v, want %v", runner.skillReads, loadedPaths)
	}
	if !report.Proposal.PresentedBeforeWrite || len(report.Terms) == 0 ||
		report.Release.ExampleReason == "" {
		t.Fatal("report omitted the pre-write explanation, relevant terms, or closest-example rationale")
	}
	if len(runner.events) == 0 || runner.events[0] != "present-proposal" {
		t.Fatalf("authoring event order = %v", runner.events)
	}
	assertReplayOrder(t, report, runner.events)
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

func assertReplayOrder(t *testing.T, report authoringReport, events []string) {
	t.Helper()
	hasInvalid := slices.ContainsFunc(report.Validation.Attempts, func(attempt validationAttempt) bool {
		return attempt.Status == "invalid"
	})
	if !hasInvalid {
		if slices.Contains(events, "write-draft") || slices.Contains(events, "validate-invalid") {
			t.Fatalf("ready-only recording contains a repair loop: %v", events)
		}
		return
	}
	draft := slices.Index(events, "write-draft")
	invalid := slices.Index(events, "validate-invalid")
	ready := slices.Index(events, "validate-ready")
	repaired := -1
	for index := invalid + 1; index < len(events); index++ {
		if events[index] == "write-final" {
			repaired = index
			break
		}
	}
	if draft < 0 || invalid <= draft || repaired <= invalid || ready <= repaired {
		t.Fatalf("recorded repair order = %v", events)
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
		Branch:   scenario.Capture.Report.Target.Branch,
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

func loadRecording(t *testing.T, name string) invocationRecording {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "repository-invocations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document recordingDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, recording := range document.Recordings {
		if recording.Name == name {
			return recording
		}
	}
	t.Fatalf("invocation recording %q not found", name)
	return invocationRecording{}
}

func assertRecordingProvenance(t *testing.T, recording invocationRecording, response recordedResponse) {
	t.Helper()
	if recording.CapturedWith == "" || recording.CapturedAt == "" || recording.Model == "" ||
		recording.SessionID == "" || recording.SkillSHA256 == "" ||
		recording.PromptSHA256 == "" || recording.ResponseSHA256 == "" ||
		recording.DiffSHA256 == "" ||
		len(recording.Interactions) == 0 {
		t.Fatalf("recording provenance is incomplete: %+v", recording)
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != recording.ResponseSHA256 {
		t.Fatalf("captured response digest = %s, want %s", got, recording.ResponseSHA256)
	}
	for _, interaction := range recording.Interactions {
		if interaction.Action == "" {
			t.Fatal("recorded interaction omitted its action")
		}
		if interaction.Action != "proposal.present" && interaction.Tool == "" {
			t.Fatalf("recorded interaction %q omitted its native tool", interaction.Action)
		}
	}
}

func normalizedPromptSHA256(prompt, workspace string, target resolvedTarget, binaryPath string) string {
	replacements := []string{
		workspace, "{workspace}",
		binaryPath, "{binaryPath}",
		target.Commit, "{targetCommit}",
	}
	if target.Root != "" {
		replacements = append(replacements, target.Root, "{targetRoot}")
	}
	normalized := strings.NewReplacer(replacements...).Replace(prompt)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(normalized)))
}

func expandInteractionArgs(args []string, workspace string, response recordedResponse) []string {
	replacer := strings.NewReplacer(
		"{workspace}", workspace,
		"{dslVersion}", response.Report.Release.DSLVersion,
		"{canonicalExample}", response.Report.Release.CanonicalExample,
	)
	expanded := make([]string, len(args))
	for index, arg := range args {
		expanded[index] = replacer.Replace(arg)
	}
	return expanded
}

func assertRecordedTranscript(t *testing.T, recording invocationRecording, outcome harness.Outcome) {
	t.Helper()
	if outcome.TranscriptSchema == "" {
		t.Fatal("Copilot replay did not expose captured native session events")
	}
	var tools []string
	for _, line := range bytes.Split(outcome.Transcript, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Role     string `json:"role"`
			ToolCall *struct {
				Name string `json:"name"`
			} `json:"tool_call"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode replay transcript: %v", err)
		}
		if event.Role == "assistant" && event.ToolCall != nil && event.ToolCall.Name != "" {
			tools = append(tools, event.ToolCall.Name)
		}
	}
	want := make([]string, len(recording.Interactions))
	want = want[:0]
	for _, interaction := range recording.Interactions {
		if interaction.Tool != "" {
			want = append(want, interaction.Tool)
		}
	}
	if !slices.Equal(tools, want) {
		t.Fatalf("captured tool interactions = %v, want %v", tools, want)
	}
}

func writeReplaySessionLog(request harness.ProcessRequest, recording invocationRecording, prompt string) error {
	path, err := replaySessionLogPath(request)
	if err != nil {
		return err
	}
	var log bytes.Buffer
	emit := func(eventType string, data interface{}) error {
		event := map[string]interface{}{"type": eventType, "data": data}
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		log.Write(encoded)
		log.WriteByte('\n')
		return nil
	}
	if err := emit("session.start", map[string]interface{}{"sessionId": recording.SessionID}); err != nil {
		return err
	}
	if err := emit("user.message", map[string]interface{}{"content": prompt}); err != nil {
		return err
	}
	// The cassette normalizes machine-specific arguments but preserves the native
	// Copilot tool sequence, which is re-emitted through the adapter's real parser.
	for index, interaction := range recording.Interactions {
		if interaction.Action == "proposal.present" {
			if err := emit("assistant.message", map[string]interface{}{
				"messageId": fmt.Sprintf("recorded-proposal-%03d", index+1),
				"content":   "Presenting the evidence ledger, state graph, paths, and capability rationale before writing.",
				"model":     recording.Model,
			}); err != nil {
				return err
			}
			continue
		}
		callID := fmt.Sprintf("recorded-%03d", index+1)
		if err := emit("tool.execution_start", map[string]interface{}{
			"toolCallId": callID,
			"toolName":   interaction.Tool,
			"arguments":  interaction,
			"model":      recording.Model,
		}); err != nil {
			return err
		}
		success := interaction.WantOK == nil || *interaction.WantOK
		data := map[string]interface{}{
			"toolCallId": callID,
			"success":    success,
			"model":      recording.Model,
		}
		if success {
			data["result"] = map[string]string{"content": "captured interaction replayed"}
		} else {
			data["error"] = map[string]string{"message": "captured validation returned findings"}
		}
		if err := emit("tool.execution_complete", data); err != nil {
			return err
		}
	}
	if err := emit("assistant.message", map[string]interface{}{
		"messageId": "recorded-final",
		"content":   "Repository-aware authoring capture replayed.",
		"model":     recording.Model,
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, log.Bytes(), 0o600)
}

func replaySessionLogPath(request harness.ProcessRequest) (string, error) {
	var copilotHome, home string
	for _, entry := range request.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "COPILOT_HOME":
			copilotHome = value
		case "HOME":
			home = value
		}
	}
	if copilotHome == "" {
		if home == "" {
			return "", fmt.Errorf("recorded Copilot invocation has no home")
		}
		copilotHome = filepath.Join(home, ".copilot")
	}
	var sessionID string
	for index, arg := range request.Command {
		switch {
		case arg == "--session-id" && index+1 < len(request.Command):
			sessionID = request.Command[index+1]
		case strings.HasPrefix(arg, "--session-id="):
			sessionID = strings.TrimPrefix(arg, "--session-id=")
		}
	}
	if sessionID == "" {
		return "", fmt.Errorf("recorded Copilot invocation has no session id")
	}
	return filepath.Join(copilotHome, "session-state", sessionID, "events.jsonl"), nil
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
