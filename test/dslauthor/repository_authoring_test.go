package dslauthor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/testgit"
)

const (
	secretFixtureValue          = "FIXTURE_SECRET_MUST_NOT_APPEAR"
	recordedAuthoringPathSHA256 = "453b1521aa9625c290eae8c6e9a9e75263156cd27d684a0f8ed6467fef44a1d6"
	captureSchema               = "goobers.dev/dsl-author-captures/v1"
)

// TestMain doubles the test binary as the Copilot SDK stdio server used by
// selectedBinary.validate; that helper hard-links this executable as "copilot".
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "copilot" {
		if err := serveTestCopilotModels(os.Stdin, os.Stdout); err != nil && !errors.Is(err, io.EOF) {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func serveTestCopilotModels(input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(input)
	for {
		payload, err := readTestCopilotRPCFrame(reader)
		if err != nil {
			return err
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(payload, &request); err != nil {
			return fmt.Errorf("decode Copilot SDK request: %w", err)
		}
		var result any
		switch request.Method {
		case "connect":
			result = map[string]any{"ok": true, "protocolVersion": 3, "version": "test"}
		case "models.list":
			result = map[string]any{"models": []map[string]any{{
				"id":   "gpt-5.6-sol",
				"name": "GPT-5.6 Sol",
			}}}
		default:
			return fmt.Errorf("unexpected Copilot SDK method %q", request.Method)
		}
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode Copilot SDK result: %w", err)
		}
		response, err := json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
		}{
			JSONRPC: "2.0",
			ID:      request.ID,
			Result:  resultJSON,
		})
		if err != nil {
			return fmt.Errorf("encode Copilot SDK response: %w", err)
		}
		if _, err := fmt.Fprintf(output, "Content-Length: %d\r\n\r\n", len(response)); err != nil {
			return fmt.Errorf("write Copilot SDK response header: %w", err)
		}
		if _, err := output.Write(response); err != nil {
			return fmt.Errorf("write Copilot SDK response: %w", err)
		}
	}
}

func readTestCopilotRPCFrame(reader *bufio.Reader) ([]byte, error) {
	contentLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name != "Content-Length" {
			return nil, fmt.Errorf("invalid Copilot SDK header %q", line)
		}
		contentLength, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil || contentLength <= 0 {
			return nil, fmt.Errorf("invalid Copilot SDK content length %q", value)
		}
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

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
}

type captureDocument struct {
	Schema   string              `json:"schema"`
	Captures []invocationCapture `json:"captures"`
}

type invocationCapture struct {
	Name                   string            `json:"name"`
	CapturedWith           string            `json:"capturedWith"`
	CapturedAt             string            `json:"capturedAt"`
	Model                  string            `json:"model"`
	SessionID              string            `json:"sessionId"`
	SkillSHA256            string            `json:"skillSHA256"`
	RawEventsSHA256        string            `json:"rawEventsSHA256"`
	NormalizedEventsSHA256 string            `json:"normalizedEventsSHA256"`
	Events                 []json.RawMessage `json:"events"`
	Files                  map[string]string `json:"files"`
	Report                 json.RawMessage   `json:"report"`
	Result                 json.RawMessage   `json:"result"`
}

type authoringReport struct {
	Status     string               `json:"status"`
	Request    string               `json:"request"`
	Contract   contractEvidence     `json:"contract"`
	Target     targetEvidence       `json:"target"`
	Terms      map[string]string    `json:"terms"`
	Command    json.RawMessage      `json:"command"`
	Evidence   []repositoryEvidence `json:"evidence"`
	Proposal   proposalReport       `json:"proposal"`
	Release    releaseEvidence      `json:"release"`
	Validation validationReport     `json:"validation"`
	Diff       string               `json:"diff"`
	Unresolved []string             `json:"unresolved"`
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
	OmittedCapabilities  json.RawMessage   `json:"omittedCapabilities"`
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
	Command  json.RawMessage     `json:"command"`
	Attempts []validationAttempt `json:"attempts"`
	Status   string              `json:"status"`
}

type validationAttempt struct {
	Status  string          `json:"status"`
	Finding json.RawMessage `json:"finding"`
}

type resolvedTarget struct {
	Identity string `json:"identity"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Access   string `json:"access"`
	Root     string `json:"root,omitempty"`
}

type selectedBinary struct {
	path    string
	version string
	commit  string
}

type captureReplayRunner struct {
	t           *testing.T
	capture     invocationCapture
	loadedPaths []string
	skillDigest string
}

func TestRepositoryAwareGoldenScenarios(t *testing.T) {
	binaryPath := buildSelectedBinary(t)
	for _, scenario := range loadScenarios(t).Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			runCapturedScenario(t, binaryPath, scenario)
		})
	}
}

func TestRepositoryAwareUnresolvedScenario(t *testing.T) {
	binaryPath := buildSelectedBinary(t)
	for _, scenario := range loadScenarios(t).Unresolved {
		t.Run(scenario.Name, func(t *testing.T) {
			runCapturedScenario(t, binaryPath, scenario)
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

func runCapturedScenario(t *testing.T, binaryPath string, scenario fixtureScenario) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	binary := openSelectedBinary(t, binaryPath)
	root, target, loadedPaths, skillDigest := prepareWorkspace(t, scenario, &binary)
	if skillDigest != recordedAuthoringPathSHA256 {
		t.Fatalf("installed authoring path digest = %s; record fresh live invocations intentionally", skillDigest)
	}

	capture := loadCapture(t, scenario.Name)
	assertCaptureProvenance(t, capture, skillDigest)
	capture = expandCapture(t, capture, root, target, binary)
	runner := &captureReplayRunner{
		t:           t,
		capture:     capture,
		loadedPaths: loadedPaths,
		skillDigest: skillDigest,
	}
	adapter := &harness.CopilotAdapter{
		Command:   []string{"copilot"},
		ExtraArgs: []string{},
		Runner:    runner,
	}
	contextPointers := []apiv1.ContextPointer{{Name: "environment-resolver-report"}}
	contextPaths := map[string]string{
		"environment-resolver-report": ".goobers/context/environment-report.json",
	}
	if target.Access == "remote" {
		contextPointers = append(contextPointers, apiv1.ContextPointer{Name: "read-only-provider-response"})
		contextPaths["read-only-provider-response"] = ".goobers/context/provider-response.json"
	}
	outcome, err := adapter.Run(context.Background(), harness.RunRequest{
		Mode: harness.ModeInvoke,
		Envelope: apiv1.InvocationEnvelope{
			TaskID:          "dsl-author-fixture",
			WorkflowID:      "repository-authoring",
			RunID:           "capture-" + strings.ReplaceAll(scenario.Name, " ", "-"),
			Gaggle:          "agent-toolkit",
			Goal:            scenario.Request,
			Workspace:       root,
			ContextPointers: contextPointers,
		},
		Instructions:          authoringInvocationInstructions,
		Workspace:             root,
		CompletionPath:        harness.DefaultResultPath,
		Model:                 capture.Model,
		HarnessConfigResolved: true,
		ContextPaths:          contextPaths,
	})
	if err != nil {
		t.Fatalf("replay captured installed-skill invocation: %v", err)
	}
	assertCapturedTranscript(t, outcome)

	var result apiv1.ResultEnvelope
	if err := json.Unmarshal(outcome.Payload, &result); err != nil {
		t.Fatalf("decode captured completion: %v", err)
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
		t.Fatalf("decode captured authoring report: %v", err)
	}
	if report.Status == "unresolved" {
		if result.Status != apiv1.ResultBlocked {
			t.Fatalf("unresolved completion status = %q, want blocked", result.Status)
		}
	} else if result.Status != apiv1.ResultSuccess {
		t.Fatalf("ready completion status = %q, want success", result.Status)
	}
	evaluateAuthoringResult(t, scenario, root, target, loadedPaths, &binary, capture, report)
}

func (r *captureReplayRunner) Run(_ context.Context, request harness.ProcessRequest) (harness.ProcessResult, error) {
	r.t.Helper()
	promptIndex := slices.Index(request.Command, "-p")
	if promptIndex < 0 || promptIndex+1 >= len(request.Command) {
		return harness.ProcessResult{}, fmt.Errorf("captured Copilot invocation has no prompt")
	}
	prompt := request.Command[promptIndex+1]
	if prompt != capturedPrompt(r.capture.Events) {
		return harness.ProcessResult{}, fmt.Errorf("runtime prompt differs from the captured invocation")
	}
	loadedPaths, digest := loadPackagedAuthoringPath(r.t, request.Dir)
	if !slices.Equal(loadedPaths, r.loadedPaths) || digest != r.skillDigest {
		return harness.ProcessResult{}, fmt.Errorf("installed authoring path differs from the captured invocation")
	}
	for path, body := range r.capture.Files {
		writeFile(r.t, filepath.Join(request.Dir, filepath.FromSlash(path)), body)
		runGit(r.t, request.Dir, "add", "-N", "--", filepath.FromSlash(path))
	}
	if err := writeRawJSON(filepath.Join(request.Dir, ".goobers", "authoring-report.json"), r.capture.Report); err != nil {
		return harness.ProcessResult{}, err
	}
	if err := writeRawJSON(filepath.Join(request.Dir, harness.DefaultResultPath), r.capture.Result); err != nil {
		return harness.ProcessResult{}, err
	}
	sessionPath, err := replaySessionLogPath(request)
	if err != nil {
		return harness.ProcessResult{}, err
	}
	var events bytes.Buffer
	for _, event := range r.capture.Events {
		events.Write(event)
		events.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		return harness.ProcessResult{}, err
	}
	if err := os.WriteFile(sessionPath, events.Bytes(), 0o600); err != nil {
		return harness.ProcessResult{}, err
	}
	return harness.ProcessResult{ExitCode: 0, Transcript: []byte("captured invocation replayed\n")}, nil
}

func evaluateAuthoringResult(
	t *testing.T,
	scenario fixtureScenario,
	root string,
	target resolvedTarget,
	loadedPaths []string,
	binary *selectedBinary,
	capture invocationCapture,
	report authoringReport,
) {
	t.Helper()
	if report.Request != scenario.Request || report.Target.Identity != target.Identity ||
		report.Target.Branch != target.Branch || report.Target.Commit != target.Commit ||
		report.Target.Access != target.Access {
		t.Fatalf("report request or target does not match fixture: %+v", report.Target)
	}
	if !slices.Equal(report.Contract.LoadedPaths, loadedPaths) ||
		report.Contract.SkillSHA256 != capture.SkillSHA256 {
		t.Fatalf("report did not identify the captured installed authoring path: %+v", report.Contract)
	}
	if !report.Proposal.PresentedBeforeWrite || len(report.Terms) == 0 ||
		report.Release.ExampleReason == "" || report.Release.CanonicalExample == "" {
		t.Fatal("report omitted its pre-write explanation, terms, or closest-example rationale")
	}
	assertEvidenceCitations(t, scenario, target, report.Evidence)
	assertSecretFreeWorkspace(t, root)

	changed := diffNames(t, root)
	capturedPaths := sortedKeys(capture.Files)
	if !slices.Equal(changed, capturedPaths) {
		t.Fatalf("workspace diff paths = %v, captured output paths = %v", changed, capturedPaths)
	}
	if report.Status == "unresolved" {
		if len(changed) != 0 || report.Diff != "" || len(report.Unresolved) == 0 ||
			report.Validation.Status != "unresolved" || len(capture.Files) != 0 {
			t.Fatalf("unresolved capture wrote config or omitted diagnostics: %+v", report)
		}
		return
	}
	if report.Status != "ready" || (report.Validation.Status != "ready" && report.Validation.Status != "passed") {
		t.Fatalf("ready capture status = %q, validation = %q", report.Status, report.Validation.Status)
	}
	proposalPaths := append([]string(nil), report.Proposal.Paths...)
	sort.Strings(proposalPaths)
	if !slices.Equal(changed, proposalPaths) {
		t.Fatalf("workspace diff paths = %v, proposed paths = %v", changed, proposalPaths)
	}
	replayedDiff := workspaceDiff(t, root)
	if canonicalDiff(report.Diff) != canonicalDiff(replayedDiff) {
		t.Fatalf("captured reviewable diff differs from the replayed workspace diff: %s",
			firstDifference(canonicalDiff(report.Diff), canonicalDiff(replayedDiff)))
	}
	binary.validate(t, root, scenario.ExistingConfig, true)
	if scenario.ExistingConfig {
		assertExistingFilesPreserved(t, scenario, root)
	}

	configDir := root
	if scenario.ExistingConfig {
		configDir = filepath.Join(root, "config")
	}
	set, loadReport, err := instance.LoadConfigDir(configDir)
	if err != nil {
		t.Fatalf("load captured config: %v (report: %+v)", err, loadReport)
	}
	command := reportCommand(t, report.Command)
	workflow := findWorkflowForCommand(t, set, command)
	if workflow.DSLVersion != report.Release.DSLVersion {
		t.Fatalf("generated DSL version = %q, report = %q", workflow.DSLVersion, report.Release.DSLVersion)
	}
	if !strings.Contains(report.Proposal.StateGraph, workflow.Spec.Start) {
		t.Fatalf("reported state graph %q omits start state %q", report.Proposal.StateGraph, workflow.Spec.Start)
	}
	wantCapabilities := expectedCapabilities(scenario.Name)
	if got := configCapabilities(set, workflow); !slices.Equal(got, wantCapabilities) {
		t.Fatalf("generated capabilities = %v, want %v", got, wantCapabilities)
	}
	if got := grantNames(report.Proposal.Capabilities); !slices.Equal(got, wantCapabilities) {
		t.Fatalf("reported capabilities = %v, want %v", got, wantCapabilities)
	}
	for _, grant := range report.Proposal.Capabilities {
		if strings.TrimSpace(grant.Reason) == "" {
			t.Errorf("capability %q has no least-privilege explanation", grant.Name)
		}
	}
}

func assertCaptureProvenance(t *testing.T, capture invocationCapture, skillDigest string) {
	t.Helper()
	if capture.CapturedWith != "GitHub Copilot CLI 1.0.75" || capture.Model != "gpt-5.6-sol" ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(capture.SkillSHA256) || len(capture.Events) == 0 {
		t.Fatal("capture provenance is incomplete")
	}
	if _, err := time.Parse(time.RFC3339Nano, capture.CapturedAt); err != nil {
		t.Fatalf("capture time %q: %v", capture.CapturedAt, err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f-]{27}$`).MatchString(capture.SessionID) {
		t.Fatalf("capture session id = %q", capture.SessionID)
	}
	digestPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !digestPattern.MatchString(capture.RawEventsSHA256) ||
		!digestPattern.MatchString(capture.NormalizedEventsSHA256) {
		t.Fatal("capture event digests are malformed")
	}
	var tools []string
	completed := map[string]bool{}
	var preWrite strings.Builder
	writing := false
	eventsText := ""
	for _, raw := range capture.Events {
		eventsText += string(raw)
		var event struct {
			ID        string          `json:"id"`
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Data      json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		if event.ID == "" || event.Timestamp == "" || event.Type == "" {
			t.Fatalf("captured native event lacks identity: %s", raw)
		}
		switch event.Type {
		case "session.start":
			var data struct {
				SessionID      string `json:"sessionId"`
				CopilotVersion string `json:"copilotVersion"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.SessionID != capture.SessionID || "GitHub Copilot CLI "+data.CopilotVersion != capture.CapturedWith {
				t.Fatalf("native session identity does not match capture metadata")
			}
		case "assistant.message":
			if writing {
				continue
			}
			var data struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			preWrite.WriteString(data.Content)
		case "tool.execution_start":
			var data struct {
				ToolCallID string `json:"toolCallId"`
				ToolName   string `json:"toolName"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			tools = append(tools, data.ToolName)
			if data.ToolName == "apply_patch" {
				writing = true
			}
		case "tool.execution_complete":
			var data struct {
				ToolCallID string `json:"toolCallId"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			completed[data.ToolCallID] = true
		}
	}
	for _, required := range []string{"skill", "view", "bash", "apply_patch"} {
		if !slices.Contains(tools, required) {
			t.Fatalf("captured tools %v omit %q", tools, required)
		}
	}
	preWriteText := strings.ToLower(preWrite.String())
	for _, required := range []string{"evidence", "state", "capabilit"} {
		if !strings.Contains(preWriteText, required) {
			t.Fatalf("captured pre-write explanation omits %q", required)
		}
	}
	for _, path := range []string{
		".goobers/agent-toolkit/skills/goobers-dsl-author/SKILL.md",
		".goobers/agent-toolkit/skills/goobers-dsl-author/references/repository-authoring.md",
	} {
		if !strings.Contains(eventsText, path) {
			t.Fatalf("captured native events do not load %s", path)
		}
	}
	if !strings.Contains(eventsText, skillDigest) {
		t.Fatalf("captured native events do not contain recorder-computed installed-path digest %s", skillDigest)
	}
	hash := sha256.New()
	for _, event := range capture.Events {
		hash.Write(event)
		hash.Write([]byte{'\n'})
	}
	if got := fmt.Sprintf("%x", hash.Sum(nil)); got != capture.NormalizedEventsSHA256 {
		t.Fatalf("normalized event digest = %s, want %s", got, capture.NormalizedEventsSHA256)
	}
	if len(completed) == 0 {
		t.Fatal("captured native event stream has no tool completions")
	}
}

func assertCapturedTranscript(t *testing.T, outcome harness.Outcome) {
	t.Helper()
	if outcome.TranscriptSchema == "" {
		t.Fatal("captured invocation did not replay through the native Copilot transcript parser")
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
			t.Fatalf("decode replayed transcript: %v", err)
		}
		if event.Role == "assistant" && event.ToolCall != nil {
			tools = append(tools, event.ToolCall.Name)
		}
	}
	if !slices.Contains(tools, "skill") || !slices.Contains(tools, "apply_patch") {
		t.Fatalf("replayed native transcript tools = %v", tools)
	}
}

func assertEvidenceCitations(t *testing.T, scenario fixtureScenario, target resolvedTarget, evidence []repositoryEvidence) {
	t.Helper()
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, path := range scenario.EvidencePaths {
		if !strings.Contains(text, path) {
			t.Errorf("repository evidence does not cite %q: %s", path, text)
		}
	}
	if target.Access == "local" && len(scenario.GuidancePaths) > 0 &&
		!slices.ContainsFunc(scenario.GuidancePaths, func(path string) bool {
			return strings.Contains(text, path)
		}) {
		t.Errorf("repository evidence cites none of the applicable guidance paths %v: %s",
			scenario.GuidancePaths, text)
	}
	if target.Access == "remote" {
		prefix := target.Identity + "@" + target.Commit + ":"
		for _, item := range evidence {
			if strings.HasPrefix(item.Citation, target.Identity+"@") && !strings.Contains(item.Citation, prefix) {
				t.Errorf("remote evidence %q does not cite exact commit %q", item.Citation, prefix)
			}
		}
	}
}

func expectedCapabilities(name string) []string {
	switch name {
	case "node app":
		return []string{"agent:model", "github:issues:write", "repo:read"}
	case "go service":
		return []string{"go@1.26.5"}
	default:
		return nil
	}
}

func reportCommand(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var command []string
	if err := json.Unmarshal(raw, &command); err == nil {
		return command
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.Fields(text)
	}
	t.Fatalf("ready report command has unsupported shape: %s", raw)
	return nil
}

func findWorkflowForCommand(t *testing.T, set *instance.ConfigSet, command []string) apiv1.Workflow {
	t.Helper()
	for _, workflow := range set.Workflows {
		for _, task := range workflow.Spec.Tasks {
			if task.Run != nil && slices.Equal(task.Run.Command, command) {
				return workflow
			}
		}
	}
	t.Fatalf("no generated workflow runs %v", command)
	return apiv1.Workflow{}
}

func configCapabilities(set *instance.ConfigSet, workflow apiv1.Workflow) []string {
	unique := map[string]bool{}
	for _, task := range workflow.Spec.Tasks {
		for _, capability := range task.Capabilities {
			unique[capability] = true
		}
		for _, capability := range task.RequiredCapabilities {
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

func assertExistingFilesPreserved(t *testing.T, scenario fixtureScenario, root string) {
	t.Helper()
	for path, want := range scenario.ExistingFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("existing file %s changed outside the request", path)
		}
	}
}

func buildSelectedBinary(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	name := "goobers"
	if runtime.GOOS == "windows" {
		// go build -o writes exactly this name; unlike a bare `go build`, it
		// never appends the platform exe suffix on its own, and exec.Command
		// below can't resolve an extensionless path on Windows.
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
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
		t.Fatal("selected binary identity or DSL support is incomplete")
	}
	binary.version, binary.commit = identity.Version, identity.Commit
	return binary
}

func (b *selectedBinary) run(t *testing.T, args ...string) []byte {
	t.Helper()
	command := exec.Command(b.path, args...)
	output, err := command.Output()
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

// installTestCopilot places a fake "copilot" binary on PATH that re-enters
// this same test binary (see TestMain). On Windows a hard link to the
// running test binary shares its inode, and Windows refuses to delete a
// file that is still mapped as an executing image — which breaks
// t.TempDir's cleanup even after the copilot subprocess itself has exited.
// Copying instead avoids sharing that inode.
func installTestCopilot(t *testing.T, destination string) {
	t.Helper()
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Link(testExecutable, destination); err != nil {
			t.Fatalf("install test Copilot model server: %v", err)
		}
		return
	}
	input, err := os.Open(testExecutable)
	if err != nil {
		t.Fatalf("install test Copilot model server: %v", err)
	}
	defer func() {
		if err := input.Close(); err != nil {
			t.Errorf("close test executable: %v", err)
		}
	}()
	info, err := input.Stat()
	if err != nil {
		t.Fatalf("install test Copilot model server: %v", err)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		t.Fatalf("install test Copilot model server: %v", err)
	}
	defer func() {
		if err := output.Close(); err != nil {
			t.Errorf("close installed copilot binary: %v", err)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		t.Fatalf("install test Copilot model server: %v", err)
	}
}

func (b *selectedBinary) validate(t *testing.T, root string, existing, wantOK bool) {
	t.Helper()
	helperDir := t.TempDir()
	installTestCopilot(t, filepath.Join(helperDir, "copilot"))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	args := []string{"validate", "--json", "--source-tree", root}
	if existing {
		args = []string{"validate", "--json", root}
	}
	command := exec.Command(b.path, args...)
	data, commandErr := command.Output()
	var report struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode selected validator result: %v\n%s", err, data)
	}
	if report.OK != wantOK || (wantOK && commandErr != nil) || (!wantOK && commandErr == nil) {
		t.Fatalf("validator ok = %v, error = %v; want ok = %v\n%s", report.OK, commandErr, wantOK, data)
	}
}

func prepareWorkspace(
	t *testing.T,
	scenario fixtureScenario,
	binary *selectedBinary,
) (string, resolvedTarget, []string, string) {
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
	binary.run(t, "agent-kit", "install", "--harness", "copilot", root)
	loadedPaths, skillDigest := loadPackagedAuthoringPath(t, root)
	resolverReport := map[string]interface{}{
		"status": "ready",
		"executable": map[string]string{
			"path":    binary.path,
			"version": binary.version,
			"commit":  binary.commit,
		},
		"contractSource": map[string]interface{}{
			"kind":        "installed-toolkit",
			"root":        filepath.Join(root, ".goobers", "agent-toolkit"),
			"integrity":   "current",
			"loadedPaths": loadedPaths,
			"sha256":      skillDigest,
		},
		"target": target,
	}
	writeJSON(t, filepath.Join(root, ".goobers", "context", "environment-report.json"), resolverReport)
	if target.Access == "remote" {
		writeJSON(t, filepath.Join(root, ".goobers", "context", "provider-response.json"), map[string]interface{}{
			"identity": target.Identity,
			"branch":   target.Branch,
			"commit":   target.Commit,
			"files":    scenario.RepositoryFiles,
		})
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture baseline")
	return root, target, loadedPaths, skillDigest
}

func resolveFixtureTarget(t *testing.T, scenario fixtureScenario) resolvedTarget {
	t.Helper()
	target := resolvedTarget{
		Identity: scenario.Identity,
		Branch:   capturedTargetBranch(t, scenario.Name),
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
	digestPaths := append(append([]string(nil), paths...),
		".goobers/agent-toolkit/skills/goobers-dsl-author/references/dsl-reference.md",
		".goobers/agent-toolkit/skills/goobers-dsl-author/references/terminology.md",
	)
	hash := sha256.New()
	bodies := map[string]string{}
	for _, path := range digestPaths {
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

func loadCapture(t *testing.T, name string) invocationCapture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "repository-captures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document captureDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.Schema != captureSchema {
		t.Fatalf("capture schema = %q, want %q", document.Schema, captureSchema)
	}
	for _, capture := range document.Captures {
		if capture.Name == name {
			return capture
		}
	}
	t.Fatalf("capture %q not found", name)
	return invocationCapture{}
}

func capturedTargetBranch(t *testing.T, name string) string {
	t.Helper()
	capture := loadCapture(t, name)
	var report authoringReport
	if err := json.Unmarshal(capture.Report, &report); err != nil {
		t.Fatal(err)
	}
	return report.Target.Branch
}

func expandCapture(
	t *testing.T,
	capture invocationCapture,
	workspace string,
	target resolvedTarget,
	binary selectedBinary,
) invocationCapture {
	t.Helper()
	workspaceCommit := strings.TrimSpace(string(runGit(t, workspace, "rev-parse", "HEAD")))
	data, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	// The placeholders are substituted into an already-JSON-encoded string, so
	// each replacement must itself be JSON-string-escaped first. Raw
	// substitution works by coincidence on POSIX (forward slashes need no
	// escaping) but corrupts the JSON on Windows, where workspace/binary/target
	// paths contain backslashes (e.g. "C:\Users\..." decodes "\U" as an
	// invalid unicode escape).
	text := strings.NewReplacer(
		"{workspace}", jsonStringBody(t, workspace),
		"{workspaceCommit}", jsonStringBody(t, workspaceCommit),
		"{binaryPath}", jsonStringBody(t, binary.path),
		"{targetRoot}", jsonStringBody(t, target.Root),
		"{targetCommit}", jsonStringBody(t, target.Commit),
		"{home}", jsonStringBody(t, os.Getenv("HOME")),
	).Replace(string(data))
	var expanded invocationCapture
	if err := json.Unmarshal([]byte(text), &expanded); err != nil {
		t.Fatalf("expand captured invocation: %v", err)
	}
	return expanded
}

// jsonStringBody returns s encoded as a JSON string, with the surrounding
// quotes stripped, so it can be substituted directly into the body of an
// existing JSON string literal.
func jsonStringBody(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded[1 : len(encoded)-1])
}

func capturedPrompt(events []json.RawMessage) string {
	for _, raw := range events {
		var event struct {
			Type string `json:"type"`
			Data struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &event) == nil && event.Type == "user.message" {
			return event.Data.Content
		}
	}
	return ""
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
			return "", fmt.Errorf("captured Copilot invocation has no home")
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
		return "", fmt.Errorf("captured Copilot invocation has no session id")
	}
	return filepath.Join(copilotHome, "session-state", sessionID, "events.jsonl"), nil
}

func workspaceDiff(t *testing.T, root string) string {
	t.Helper()
	return string(runGit(t, root, "diff", "--no-ext-diff", "--"))
}

func firstDifference(left, right string) string {
	limit := min(len(left), len(right))
	index := 0
	for index < limit && left[index] == right[index] {
		index++
	}

	start := max(0, index-80)
	leftEnd := min(len(left), index+80)
	rightEnd := min(len(right), index+80)
	return fmt.Sprintf("byte %d (lengths %d/%d), captured %q, replayed %q",
		index, len(left), len(right), left[start:leftEnd], right[start:rightEnd])
}

func canonicalDiff(diff string) string {
	var sections []string
	for _, section := range strings.Split(diff, "diff --git ") {
		if section != "" {
			sections = append(sections, "diff --git "+section)
		}
	}
	sort.Strings(sections)
	return strings.Join(sections, "")
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

func writeRawJSON(path string, data json.RawMessage) error {
	if !json.Valid(data) {
		return fmt.Errorf("captured output %q is not valid JSON", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(append([]byte(nil), data...), '\n'), 0o644)
}

func writeJSON(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
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

// protocol.file.allow keeps behaviour stable across git versions that changed
// the default, so a fixture repository is materialized the same way everywhere.
var fixtureGitConfig = []string{
	"-c", "protocol.file.allow=always",
}

func runGit(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	full := append([]string{"-C", root}, fixtureGitConfig...)
	command := testgit.Command(append(full, args...)...)
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

const authoringInvocationInstructions = `Exercise the installed Goobers adapter chain and goobers-dsl-author skill exactly as an adopter would. Treat the task as a plain-English authoring request. Use the supplied environment-resolver report rather than selecting another binary or contract source. A read-only-provider-response context, when present, is the captured response from existing provider access at the exact target commit.

Before writing config, present the evidence ledger, state graph, changed paths, release and closest-example choice, and least-privilege capability rationale. After authoring, write .goobers/authoring-report.json as JSON with: status (ready or unresolved), request, contract {loadedPaths, skillSHA256}, target {identity, branch, commit, access}, terms, command, evidence [{conclusion,citation}], proposal {presentedBeforeWrite,stateGraph,paths,capabilities [{name,reason}],omittedCapabilities}, release {binaryPath,version,commit,dslVersion,canonicalExample,exampleReason}, validation {command,attempts [{status,finding}],status}, diff, and unresolved. The diff must match the final workspace diff. Set the completion result output reportPath to .goobers/authoring-report.json.`
