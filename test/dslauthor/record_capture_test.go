//go:build authoringcapture

package dslauthor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

// TestRecordRepositoryAuthoring is an opt-in maintenance recorder, never part
// of ordinary CI. It invokes the installed CLI and saves new evidence outside
// the repository for review; it does not update fixture hashes or accept its
// own output as a passing replay. Select individual scenarios with -run.
func TestRecordRepositoryAuthoring(t *testing.T) {
	output := os.Getenv("GOOBERS_AUTHORING_CAPTURE_DIR")
	if !filepath.IsAbs(output) {
		t.Fatal("GOOBERS_AUTHORING_CAPTURE_DIR must name an existing absolute output directory")
	}
	if info, err := os.Stat(output); err != nil || !info.IsDir() {
		t.Fatal("capture output directory is unavailable")
	}
	binaryPath := buildSelectedBinary(t)
	scenarios := loadScenarios(t)
	for _, scenario := range append(scenarios.Scenarios, scenarios.Unresolved...) {
		t.Run(scenario.Name, func(t *testing.T) {
			binary := openSelectedBinary(t, binaryPath)
			root, target, loadedPaths, digest := prepareWorkspace(t, scenario, &binary)
			prefix := filepath.Join(output, strings.ReplaceAll(scenario.Name, " ", "-"))
			runner := &nativeCaptureRunner{prefix: prefix}
			adapter := &harness.CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
			pointers := []apiv1.ContextPointer{{Name: "environment-resolver-report"}}
			paths := map[string]string{"environment-resolver-report": ".goobers/context/environment-report.json"}
			if target.Access == "remote" {
				pointers = append(pointers, apiv1.ContextPointer{Name: "read-only-provider-response"})
				paths["read-only-provider-response"] = ".goobers/context/provider-response.json"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
			defer cancel()
			outcome, err := adapter.Run(ctx, harness.RunRequest{
				Mode: harness.ModeInvoke,
				Envelope: apiv1.InvocationEnvelope{
					TaskID: "dsl-author-fixture", WorkflowID: "repository-authoring",
					RunID:  "capture-" + strings.ReplaceAll(scenario.Name, " ", "-"),
					Gaggle: "agent-toolkit", Goal: scenario.Request, Workspace: root, ContextPointers: pointers,
				},
				Instructions: authoringInvocationInstructions, Workspace: root,
				CompletionPath: harness.DefaultResultPath, Model: "gpt-5.6-sol",
				HarnessConfigResolved: true, ContextPaths: paths,
			})
			if err != nil {
				t.Fatalf("native invocation failed (diagnostics at %s): %v", prefix, err)
			}
			capture := nativeCapture(t, runner.events, scenario.Name, digest)
			capture.Result = outcome.Payload
			capture.Report = readCaptureFile(t, filepath.Join(root, ".goobers", "authoring-report.json"))
			capture.Files = map[string]string{}
			for _, path := range diffNames(t, root) {
				if !filepath.IsLocal(path) || strings.HasPrefix(filepath.ToSlash(path), ".git/") {
					t.Fatalf("unsafe captured output path %q", path)
				}
				capture.Files[path] = string(readCaptureFile(t, filepath.Join(root, path)))
			}
			// Save evidence before assertions so an invalid live response remains
			// diagnosable, but must still pass the normal replay before adoption.
			encoded, err := json.Marshal(normalizeNativeCapture(t, capture, root, target, binary))
			if err != nil {
				t.Fatal(err)
			}
			// Keep native events compact: indenting RawMessages changes the
			// bytes whose normalized digest is checked by the replay suite.
			if err := os.WriteFile(prefix+".json", append(encoded, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			var report authoringReport
			if err := json.Unmarshal(capture.Report, &report); err != nil {
				t.Fatal(err)
			}
			evaluateAuthoringResult(t, scenario, root, target, loadedPaths, &binary, capture, report)
		})
	}
}

type nativeCaptureRunner struct {
	prefix string
	events []byte
}

// TestAssembleNativeCaptures mechanically packages reviewed native captures.
// It never repairs their contents or digests; ordinary replay must pass after
// adoption. Both the exact input files and output path are explicit opt-ins.
func TestAssembleNativeCaptures(t *testing.T) {
	output := os.Getenv("GOOBERS_AUTHORING_CAPTURE_OUTPUT")
	if output == "" {
		t.Skip("capture assembly requires an explicit output path")
	}
	if !filepath.IsAbs(output) {
		t.Fatal("capture output must be absolute")
	}
	inputs := filepath.SplitList(os.Getenv("GOOBERS_AUTHORING_CAPTURE_INPUTS"))
	scenarios := loadScenarios(t)
	all := append(scenarios.Scenarios, scenarios.Unresolved...)
	if len(inputs) != len(all) {
		t.Fatal("assembly requires exactly one input for every scenario, in scenario order")
	}
	document := captureDocument{Schema: captureSchema}
	for i, path := range inputs {
		if !filepath.IsAbs(path) {
			t.Fatal("capture input must be absolute")
		}
		var capture invocationCapture
		if err := json.Unmarshal(readCaptureFile(t, path), &capture); err != nil {
			t.Fatal(err)
		}
		var report authoringReport
		if err := json.Unmarshal(capture.Report, &report); err != nil {
			t.Fatal(err)
		}
		if capture.Name != all[i].Name || report.Request != all[i].Request || capture.SkillSHA256 != recordedAuthoringPathSHA256 {
			t.Fatalf("input %q does not match the current scenario and pinned authoring path", path)
		}
		assertCaptureProvenanceVersion(t, capture, recordedAuthoringPathSHA256, "GitHub Copilot CLI 1.0.83")
		document.Captures = append(document.Captures, capture)
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Review an already-recorded candidate without another model invocation.
// The committed fixture version and digest remain pinned until all candidates
// have been reviewed and intentionally adopted together.
func TestReviewNativeCapture(t *testing.T) {
	output := os.Getenv("GOOBERS_AUTHORING_CAPTURE_DIR")
	if !filepath.IsAbs(output) {
		t.Fatal("GOOBERS_AUTHORING_CAPTURE_DIR must name an absolute capture directory")
	}
	binaryPath := buildSelectedBinary(t)
	scenarios := loadScenarios(t)
	for _, scenario := range append(scenarios.Scenarios, scenarios.Unresolved...) {
		t.Run(scenario.Name, func(t *testing.T) {
			var capture invocationCapture
			path := filepath.Join(output, strings.ReplaceAll(scenario.Name, " ", "-")+".json")
			if err := json.Unmarshal(readCaptureFile(t, path), &capture); err != nil {
				t.Fatal(err)
			}
			binary := openSelectedBinary(t, binaryPath)
			root, target, loadedPaths, digest := prepareWorkspace(t, scenario, &binary)
			if capture.Name != scenario.Name || capture.SkillSHA256 != digest {
				t.Fatal("candidate does not match the scenario or installed authoring path")
			}
			assertCaptureProvenanceVersion(t, capture, digest, "GitHub Copilot CLI 1.0.83")
			capture = expandCapture(t, capture, root, target, binary)
			for path, body := range capture.Files {
				if !filepath.IsLocal(path) {
					t.Fatalf("unsafe candidate file %q", path)
				}
				writeFile(t, filepath.Join(root, path), body)
				runGit(t, root, "add", "-N", "--", path)
			}
			var report authoringReport
			if err := json.Unmarshal(capture.Report, &report); err != nil {
				t.Fatal(err)
			}
			evaluateAuthoringResult(t, scenario, root, target, loadedPaths, &binary, capture, report)
		})
	}
}

func (r *nativeCaptureRunner) Run(ctx context.Context, request harness.ProcessRequest) (harness.ProcessResult, error) {
	result, runErr := (harness.ExecProcessRunner{}).Run(ctx, request)
	if err := os.WriteFile(r.prefix+".transcript", result.Transcript, 0o600); err != nil {
		return result, err
	}
	path, err := replaySessionLogPath(request)
	if err != nil {
		return result, err
	}
	r.events, err = os.ReadFile(path)
	if err != nil {
		return result, fmt.Errorf("read native session after invocation (%v): %w", runErr, err)
	}
	if err := os.WriteFile(r.prefix+".events.jsonl", r.events, 0o600); err != nil {
		return result, err
	}
	return result, runErr
}

func nativeCapture(t *testing.T, events []byte, name, digest string) invocationCapture {
	t.Helper()
	capture := invocationCapture{Name: name, Model: "gpt-5.6-sol", SkillSHA256: digest, RawEventsSHA256: fmt.Sprintf("%x", sha256.Sum256(events))}
	for _, line := range bytes.Split(bytes.TrimSpace(events), []byte{'\n'}) {
		var event struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Data      struct {
				SessionID      string `json:"sessionId"`
				CopilotVersion string `json:"copilotVersion"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		capture.Events = append(capture.Events, append(json.RawMessage(nil), line...))
		if event.Type == "session.start" {
			capture.SessionID, capture.CapturedAt = event.Data.SessionID, event.Timestamp
			capture.CapturedWith = "GitHub Copilot CLI " + event.Data.CopilotVersion
		}
	}
	return capture
}

func normalizeNativeCapture(t *testing.T, capture invocationCapture, root string, target resolvedTarget, binary selectedBinary) invocationCapture {
	t.Helper()
	data, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	pairs := []string{binary.path, "{binaryPath}", root, "{workspace}"}
	if target.Root != "" {
		pairs = append(pairs, target.Root, "{targetRoot}")
	}
	pairs = append(pairs, strings.TrimSpace(string(runGit(t, root, "rev-parse", "HEAD"))), "{workspaceCommit}", target.Commit, "{targetCommit}")
	if home := os.Getenv("HOME"); home != "" {
		pairs = append(pairs, home, "{home}")
	}
	for index := 0; index < len(pairs); index += 2 {
		pairs[index] = jsonStringBody(t, pairs[index])
	}
	var normalized invocationCapture
	if err := json.Unmarshal([]byte(strings.NewReplacer(pairs...).Replace(string(data))), &normalized); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	for _, event := range normalized.Events {
		hash.Write(event)
		hash.Write([]byte{'\n'})
	}
	normalized.NormalizedEventsSHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	return normalized
}

func readCaptureFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNativeCaptureNormalizationPreservesEvidenceDigests(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "baseline")
	binary := selectedBinary{path: filepath.Join(root, "selected-binary")}
	event, err := json.Marshal(map[string]any{
		"type": "session.start", "timestamp": "2026-09-09T00:00:00Z",
		"data": map[string]string{"sessionId": "01234567-89ab-cdef-0123-456789abcdef", "copilotVersion": "1.0.83", "workspace": root, "binary": binary.path},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := append(event, '\n')
	capture := nativeCapture(t, raw, "fixture", strings.Repeat("a", 64))
	normalized := normalizeNativeCapture(t, capture, root, resolvedTarget{Commit: strings.Repeat("b", 40)}, binary)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var decoded invocationCapture
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	for _, event := range decoded.Events {
		hash.Write(event)
		hash.Write([]byte{'\n'})
	}
	if decoded.RawEventsSHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) || decoded.NormalizedEventsSHA256 != fmt.Sprintf("%x", hash.Sum(nil)) {
		t.Fatal("serializing capture changed raw or normalized evidence digests")
	}
	if decoded.SessionID != capture.SessionID || decoded.CapturedAt != capture.CapturedAt || decoded.CapturedWith != capture.CapturedWith {
		t.Fatal("normalization changed native provenance")
	}
	if strings.Contains(string(encoded), root) || !strings.Contains(string(encoded), "{workspace}") || !strings.Contains(string(encoded), "{binaryPath}") {
		t.Fatal("normalization did not replace environment-specific paths")
	}
}
