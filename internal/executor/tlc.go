package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

const (
	// KindTLC opts into independent bounded model checking.
	KindTLC = "tlc"
	// InputTLCManifest selects a reviewed manifest under the runner config tree.
	InputTLCManifest = "tlcManifest"
	// InputTLCModel selects the agent-editable graph under the stage workspace.
	InputTLCModel = "tlcModel"
	tlcJarDigest  = "sha256:e20447644bfabd29d6bd0fc2c0eb1473fb301c1a0e4dfe1961938ce5a191edbf"
	tlcVersion    = "v1.8.0+2026.11.03.180953.rev309e489"
)

// TLCManifest is runner-owned policy, never a candidate-supplied success report.
// RuntimeDigest hashes the sorted JSON array of {path,digest} for every runtime
// file (slash-relative paths). Jar and runtime paths are absolute operator paths.
type TLCManifest struct {
	Mode          string   `json:"mode,omitempty"`
	Module        string   `json:"module,omitempty"`
	Property      string   `json:"property"`
	Invariant     string   `json:"invariant"`
	Specification string   `json:"specification"`
	SpecDigest    string   `json:"specDigest"`
	Configuration string   `json:"configuration"`
	ConfigDigest  string   `json:"configDigest"`
	Jar           string   `json:"jar"`
	Runtime       string   `json:"runtime"`
	RuntimeDigest string   `json:"runtimeDigest"`
	Java          string   `json:"java"`
	Assumptions   []string `json:"assumptions"`
}

// TLCExecutor uses ShellExecutor for all process/credential/network/timeout
// safeguards. Editable inputs are finite graphs only; native models must be
// entirely reviewed and pinned in the operator-owned configuration tree.
type TLCExecutor struct {
	Shell *ShellExecutor
}

type tlcAttempt struct {
	directory string
	manifest  TLCManifest
	subject   []byte
	spec      []byte
	config    []byte
	module    []byte
	runtime   []tlcRuntimeFile
	command   []string
}

// Run snapshots the selected model and checks its reviewed invariant.
func (e *TLCExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if e == nil || e.Shell == nil || e.Shell.Journal == nil {
		return apiv1.ResultEnvelope{}, errors.New("tlc: requires the runner shell and journal")
	}
	if _, ok := e.Shell.Journal.(credentialRefusalEventAppender); !ok {
		return apiv1.ResultEnvelope{}, errors.New("tlc: journal appender is required")
	}
	if err := validateShellChildWorkspace(ctx, env); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := validateTLCInvocation(env, run); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil)
	}
	attempt, err := e.snapshot(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("tlc: prepare pinned inputs: %w", err)
	}
	defer func() { _ = os.RemoveAll(attempt.directory) }()
	return e.check(ctx, env, run, attempt)
}

func validateTLCInvocation(env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) error {
	if len(run.Command) != 0 || run.Script != "" || len(run.Env) != 0 || run.InjectRunContext || len(env.Capabilities) != 0 {
		return errors.New("tlc: command, script, environment, context injection, and credential capabilities are not supported")
	}
	for _, key := range []string{InputVerificationReport, InputResultFile} {
		if _, exists := env.Inputs[key]; exists {
			return fmt.Errorf("tlc: input %s is not supported", key)
		}
	}
	if stringInput(env, InputTLCManifest) == "" {
		return errors.New("tlc: tlcManifest is required")
	}
	return nil
}

func (e *TLCExecutor) snapshot(env apiv1.InvocationEnvelope) (*tlcAttempt, error) {
	root, err := canonicalTLCPath(e.Shell.ConfigDirectory)
	if err != nil || root == "" {
		return nil, errors.New("reviewed runner config directory is required")
	}
	workspace, err := canonicalTLCPath(env.Workspace)
	if err != nil {
		return nil, err
	}
	if tlcWithin(workspace, root) || tlcWithin(root, workspace) {
		return nil, errors.New("reviewed config and candidate workspace must be disjoint")
	}
	path, err := apiv1.ResolveContainedPath(root, stringInput(env, InputTLCManifest))
	if err != nil {
		return nil, err
	}
	data, err := readTLCFile(path, maxTLCInput)
	if err != nil {
		return nil, err
	}
	attempt := &tlcAttempt{}
	if err := decodeTLCJSON(data, &attempt.manifest); err != nil {
		return nil, err
	}
	if err := attempt.readInputs(root, env); err != nil {
		return nil, err
	}
	scratch, err := canonicalTLCPath(e.Shell.ScratchDir)
	if err != nil || scratch == "" || tlcWithin(workspace, scratch) {
		return nil, errors.New("runner-owned scratch directory outside candidate workspace is required")
	}
	attempt.directory, err = os.MkdirTemp(scratch, "tlc-")
	if err != nil {
		return nil, err
	}
	if err := attempt.materialize(workspace); err != nil {
		_ = os.RemoveAll(attempt.directory)
		return nil, err
	}
	return attempt, nil
}

func (a *tlcAttempt) readInputs(root string, env apiv1.InvocationEnvelope) error {
	if err := a.validateMode(env); err != nil {
		return err
	}
	if a.manifest.Assumptions == nil || len(a.manifest.Assumptions) > 13 {
		return errors.New("reviewed manifest requires an explicit array of at most 13 assumptions")
	}
	config, err := tlcConfiguration(a.manifest.Invariant)
	if err != nil {
		return err
	}
	for _, input := range []struct {
		path, digest string
		target       *[]byte
	}{
		{a.manifest.Specification, a.manifest.SpecDigest, &a.spec},
		{a.manifest.Configuration, a.manifest.ConfigDigest, &a.config},
	} {
		path, err := apiv1.ResolveContainedPath(root, input.path)
		if err != nil {
			return err
		}
		*input.target, err = readTLCPinned(path, input.digest, maxTLCInput)
		if err != nil {
			return err
		}
	}
	if a.manifest.Mode == tlcReviewedNative {
		return a.readNative()
	}
	if string(config) != string(a.config) {
		return errors.New("configuration must check exactly Init/Next and the requested invariant, without constraints or simulation")
	}
	if !strings.HasPrefix(string(a.spec), "---- MODULE Spec ----\nEXTENDS Candidate\n") {
		return errors.New("specification must be module Spec extending only Candidate")
	}
	if err := validateTLCModuleWords(a.spec); err != nil {
		return err
	}
	model, err := apiv1.ResolveContainedPath(env.Workspace, stringInput(env, InputTLCModel))
	if err != nil {
		return err
	}
	a.subject, err = readTLCFile(model, maxTLCInput)
	if err != nil {
		return err
	}
	a.module, err = compileTLCModel(a.subject)
	return err
}

func (a *tlcAttempt) materialize(workspace string) error {
	if !filepath.IsAbs(a.manifest.Runtime) || !filepath.IsAbs(a.manifest.Jar) {
		return errors.New("runtime and jar must be absolute operator paths")
	}
	for _, path := range []string{a.manifest.Runtime, a.manifest.Jar} {
		actual, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if tlcWithin(workspace, actual) || tlcWithin(actual, workspace) {
			return errors.New("tool dependencies must not be candidate-editable")
		}
	}
	runtimeRoot := filepath.Join(a.directory, "runtime")
	var err error
	a.runtime, err = snapshotTLCRuntime(a.manifest.Runtime, runtimeRoot)
	if err != nil {
		return err
	}
	if len(a.runtime) == 0 || tlcRuntimeDigest(a.runtime) != a.manifest.RuntimeDigest {
		return errors.New("java runtime digest mismatch")
	}
	java, err := apiv1.ResolveContainedPath(runtimeRoot, a.manifest.Java)
	if err != nil {
		return err
	}
	if _, err := readTLCFile(java, 64<<20); err != nil {
		return err
	}
	jar, err := readTLCPinned(a.manifest.Jar, tlcJarDigest, 64<<20)
	if err != nil {
		return err
	}
	sourceName, configName, moduleName := a.sourceNames()
	if a.manifest.Mode == tlcReviewedNative {
		if err := refuseTLCNativeOverride(jar, a.manifest.Module); err != nil {
			return err
		}
	}
	for name, data := range map[string][]byte{sourceName: a.spec, configName: a.config, moduleName: a.module, "tla2tools.jar": jar} {
		if err := os.WriteFile(filepath.Join(a.directory, name), data, 0o600); err != nil {
			return err
		}
	}
	a.command = []string{java, "-Xmx256m", "-XX:+UseParallelGC", "-Djava.io.tmpdir=" + a.directory, "-cp", filepath.Join(a.directory, "tla2tools.jar"), "tlc2.TLC",
		"-tool", "-workers", "1", "-seed", "1", "-fp", "0", "-metadir", filepath.Join(a.directory, "states"),
		"-config", configName, strings.TrimSuffix(sourceName, ".tla")}
	return nil
}

func (e *TLCExecutor) check(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun, attempt *tlcAttempt) (apiv1.ResultEnvelope, error) {
	capture := &tlcCapture{ArtifactRecorder: e.Shell.Journal, stdoutName: env.TaskID + "/stdout.log", stderrName: env.TaskID + "/stderr.log"}
	shell := *e.Shell
	shell.Journal = capture
	shell.DefaultEnv = nil
	shell.ExtraEnvAllowlist = nil
	// Java option variables must never turn a reviewed checker into an agent
	// launch hook. Remaining shell environment and process safeguards are kept.
	run.Env = map[string]string{"JAVA_TOOL_OPTIONS": "", "_JAVA_OPTIONS": "", "JDK_JAVA_OPTIONS": "", "CLASSPATH": "", "JAVA_HOME": filepath.Join(attempt.directory, "runtime")}
	run.Command = attempt.command
	child := env
	child.Workspace = attempt.directory
	child.Inputs = map[string]any{InputTimeout: "30s", InputMaxOutputBytes: "1048576"}
	for _, key := range []string{InputTimeout, InputMaxOutputBytes} {
		if value, ok := env.Inputs[key]; ok {
			child.Inputs[key] = value
		}
	}
	timeout, err := shell.resolveTimeout(child)
	if err != nil || timeout.Duration > time.Minute {
		return apiv1.ResultEnvelope{}, errors.New("tlc: timeout must be positive and at most one minute")
	}
	limit, err := shell.maxOutputFor(child)
	if err != nil || limit > DefaultMaxOutputBytes {
		return apiv1.ResultEnvelope{}, errors.New("tlc: output limit must be at most 1 MiB")
	}
	started := time.Now().UTC()
	result, err := shell.Run(ctx, child, run)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if ctx.Err() != nil {
		return result, nil
	}
	if capture.source.Digest == "" {
		capture.source, err = e.Shell.Journal.RecordArtifact(env.TaskID+"/stdout.log", nil)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
	}
	outcome, reason := classifyTLC(capture.stdout, capture.stderr, result, attempt.manifest.Invariant)
	execution := map[string]any{"argv": attempt.command, "started": started, "finished": time.Now().UTC(), "result": result,
		"timeout": timeout.Duration.String(), "outputLimit": limit, "network": run.Network}
	return e.publishTLC(env, attempt, capture, execution, outcome, reason)
}

type tlcCapture struct {
	ArtifactRecorder
	stdoutName, stderrName string
	stdout, stderr         []byte
	source                 journal.Ref
}

func (c *tlcCapture) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	ref, err := c.ArtifactRecorder.RecordArtifact(name, data)
	switch name {
	case c.stdoutName:
		c.stdout, c.source = append([]byte(nil), data...), ref
	case c.stderrName:
		c.stderr = append([]byte(nil), data...)
	}
	return ref, err
}

func (e *TLCExecutor) publishTLC(env apiv1.InvocationEnvelope, a *tlcAttempt, capture *tlcCapture, execution map[string]any, outcome creditgraph.VerificationOutcome, reason string) (apiv1.ResultEnvelope, error) {
	appender, ok := e.Shell.Journal.(credentialRefusalEventAppender)
	if !ok {
		return apiv1.ResultEnvelope{}, errors.New("tlc: journal appender is required")
	}
	report := creditgraph.VerificationReport{
		Schema: creditgraph.VerificationSchemaVersion, Property: a.manifest.Property,
		Checker:     creditgraph.VerificationChecker{Name: "TLC", Version: tlcVersion, Digest: tlcJarDigest},
		Assumptions: append([]string{"finite graph only; no Go refinement or unbounded proof", "deadlocks and liveness are not checked", "TLC 64-bit fingerprints may collide"}, a.manifest.Assumptions...),
		Outcome:     outcome, Reason: reason,
	}
	sourceName, configName, moduleName := a.sourceNames()
	adapter, subjectName := "tlc-finite-graph-v1", "subject.json"
	if a.manifest.Mode == tlcReviewedNative {
		adapter, subjectName = "tlc-reviewed-native-v1", sourceName
		report.Assumptions[0] = "reviewed finite TLA+ model only; no Go refinement or unbounded proof"
	}
	spec, _ := json.Marshal(map[string]string{sourceName: string(a.spec), configName: string(a.config)})
	contextData, _ := json.Marshal(map[string]any{"adapter": adapter, "manifest": a.manifest, "runtime": a.runtime})
	executionData, _ := json.Marshal(execution)
	for _, artifact := range []struct {
		name string
		data []byte
		ref  *string
	}{
		{subjectName, a.subject, &report.Subject},
		{"specification.json", spec, &report.Specification},
		{"context.json", contextData, &report.Context},
	} {
		ref, err := recordTLCPinnedArtifact(e.Shell.Journal, env.TaskID+"/"+artifact.name, artifact.data)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		*artifact.ref = ref.Digest
	}
	for _, artifact := range []struct {
		name string
		data []byte
	}{{moduleName, a.module}, {"execution.json", executionData}} {
		ref, err := recordTLCPinnedArtifact(e.Shell.Journal, env.TaskID+"/"+artifact.name, artifact.data)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		report.Evidence = append(report.Evidence, ref.Digest)
	}
	report.Evidence = append(report.Evidence, capture.source.Digest)
	encoded, err := json.Marshal(report)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if _, err := creditgraph.DecodeVerificationReport(encoded); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if _, err := e.Shell.Journal.RecordArtifact(env.TaskID+"/verification.json", encoded); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := appender.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: env.TaskID, Attempt: int(env.Attempt),
		Runner: creditgraph.VerificationAnnotationPayload(report, capture.source.Digest)}); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "TLC finite-model check: " + string(outcome),
		Outputs: map[string]any{"verificationOutcome": string(outcome)}}, nil
}

func recordTLCPinnedArtifact(recorder ArtifactRecorder, name string, data []byte) (journal.Ref, error) {
	ref, err := recorder.RecordArtifact(name, data)
	if err == nil && ref.Digest != tlcDigest(data) {
		err = errors.New("tlc: artifact recording changed bound input or execution bytes")
	}
	return ref, err
}
