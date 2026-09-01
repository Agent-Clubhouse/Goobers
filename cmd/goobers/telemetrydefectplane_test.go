package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/telemetryclient"
)

// telemetrydefectplane_test.go is the end-to-end half of Goobers#4001: what a
// dispatched `defect-nomination` stage actually gets when it runs
// `telemetry-query` in a pod, measured against what the same command produces
// against the instance root.
//
// The parity test is the important one. Two derivations of one aggregate
// eventually answer two different things, and a nomination lane cannot tell
// which one is right — so the daemon calls the SAME function the CLI does,
// and this test is what would notice if that ever stopped being true.

// nominationArgs is the exact aggregate set gather-telemetry asks for
// (config-examples/.../work-nomination.yaml), which is the workload this
// whole plane exists to serve.
func nominationArgs(extra ...string) []string {
	args := []string{
		"telemetry-query",
		"--window", "168h",
		"--gaggle", "example",
		"--aggregate", "stage-failure-rate",
		"--aggregate", "error-signature",
		"--aggregate", "gate-noise",
		"--aggregate", "credit-assignment",
		"--threshold", "min-samples=1",
		"--threshold", "max-failure-rate=1",
		"--threshold", "min-error-signature-count=1",
		"--format", "candidate-findings",
	}
	return append(args, extra...)
}

func decodeCandidateFindings(t *testing.T, stdout string) candidateFindingsArtifact {
	t.Helper()
	var artifact candidateFindingsArtifact
	if err := json.Unmarshal([]byte(stdout), &artifact); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, stdout)
	}
	return artifact
}

// TestTelemetryQueryPlaneMatchesTheLocalResult is the parity check. Same
// instance, same window, same thresholds, same aggregates: the artifact a
// dispatched pod receives must be the artifact the daemon would have written
// locally, modulo the ONE difference the ruling asks for — normalized error
// signatures.
func TestTelemetryQueryPlaneMatchesTheLocalResult(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	rebuildTelemetryQueryRollup(t, root)

	code, localOut, stderr := runArgs(t, nominationArgs(root)...)
	if code != 0 {
		t.Fatalf("local: code = %d, stderr = %q", code, stderr)
	}
	local := decodeCandidateFindings(t, localOut)
	if len(local.Findings) == 0 {
		t.Fatal("the fixture produced no local findings, so parity would be vacuous")
	}

	plane := newTelemetryReadPlane(t, root)
	token := plane.admitRun(t, "example", "pod-run-1")
	plane.stamp(t, token, "example")

	code, planeOut, stderr := runArgs(t, nominationArgs()...)
	if code != 0 {
		t.Fatalf("plane: code = %d, stderr = %q", code, stderr)
	}
	validateCandidateFindings(t, []byte(planeOut))
	planeArtifact := decodeCandidateFindings(t, planeOut)

	if planeArtifact.Schema != local.Schema || planeArtifact.Window != local.Window {
		t.Fatalf("schema/window drifted: %q/%q vs %q/%q",
			planeArtifact.Schema, planeArtifact.Window, local.Schema, local.Window)
	}
	if planeArtifact.NoWork != local.NoWork {
		t.Fatalf("noWork = %v, want %v", planeArtifact.NoWork, local.NoWork)
	}
	if len(planeArtifact.Findings) != len(local.Findings) {
		t.Fatalf("plane findings = %d, local findings = %d\nplane: %s\nlocal: %s",
			len(planeArtifact.Findings), len(local.Findings), planeOut, localOut)
	}
	expected := make([]rollup.Finding, 0, len(local.Findings))
	for _, finding := range local.Findings {
		expected = append(expected, redactFindingForPlane(finding))
	}
	planeJSON, err := json.Marshal(planeArtifact.Findings)
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if string(planeJSON) != string(expectedJSON) {
		t.Fatalf("findings drifted from the local derivation\nplane:    %s\nexpected: %s", planeJSON, expectedJSON)
	}
	// The fixture's error code (`fixture_error`) is identifier-shaped, so
	// normalization is the identity for it. That is the common case for real
	// rollup data and it is why "preserve current output semantics" is
	// achievable at all — pinned here so a change to the normalizer that
	// starts redacting ordinary codes is caught as the semantic break it is.
	for _, finding := range planeArtifact.Findings {
		if finding.Kind == rollup.FindingErrorSignature && finding.Subject != "fixture_error" {
			t.Fatalf("an identifier-shaped code was redacted: %+v", finding)
		}
	}
}

// TestTelemetryQueryPlaneRedactsHostileErrorCodes is the other side of that
// coin: a code that is not identifier-shaped — a message, a path, an address —
// must NOT cross the plane, even though the local path prints it. This is the
// exact clause decision 005 R4 kept when #4001 amended it.
func TestTelemetryQueryPlaneRedactsHostileErrorCodes(t *testing.T) {
	root := initDemo(t)
	hostile := "failed to read /Users/alice/.config/goobers/credentials.json"
	writeFixtureRunWithErrorCode(t, root, "hostile-run-1", "example", hostile)
	rebuildTelemetryQueryRollup(t, root)

	code, localOut, stderr := runArgs(t, nominationArgs(root)...)
	if code != 0 {
		t.Fatalf("local: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(localOut, hostile) {
		t.Fatalf("the local path did not carry the raw code, so the plane check is vacuous: %s", localOut)
	}

	plane := newTelemetryReadPlane(t, root)
	token := plane.admitRun(t, "example", "pod-run-1")
	plane.stamp(t, token, "example")
	code, planeOut, stderr := runArgs(t, nominationArgs()...)
	if code != 0 {
		t.Fatalf("plane: code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(planeOut, hostile) || strings.Contains(planeOut, "/Users/alice") {
		t.Fatalf("a raw error signature crossed the plane: %s", planeOut)
	}
	if !strings.Contains(planeOut, telemetryclient.RedactedSignatureSubject) {
		t.Fatalf("the redacted subject is missing, so the finding was dropped rather than normalized: %s", planeOut)
	}
	validateCandidateFindings(t, []byte(planeOut))
}

// TestTelemetryQueryRefusesWhatThePlaneCannotServe pins the loud half of
// "preserve output semantics as far as SAFELY possible". Every invocation the
// plane cannot answer faithfully is refused with a usage error, never served
// as a quietly narrower answer.
func TestTelemetryQueryRefusesWhatThePlaneCannotServe(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	rebuildTelemetryQueryRollup(t, root)
	plane := newTelemetryReadPlane(t, root)
	token := plane.admitRun(t, "example", "pod-run-1")
	plane.stamp(t, token, "example")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no aggregate means every family",
			args: []string{"telemetry-query", "--window", "24h", "--gaggle", "example"},
			want: "Name the aggregates you want",
		},
		{
			name: "an unadmitted aggregate",
			args: nominationArgs("--aggregate", "ci-check-failure"),
			want: "ci-check-failure",
		},
		{
			name: "the all aggregate",
			args: []string{"telemetry-query", "--gaggle", "example", "--aggregate", "all"},
			want: "all",
		},
		{
			name: "a learning-action filter",
			args: nominationArgs("--learning-action", "code-issue"),
			want: "--learning-action",
		},
		{
			name: "the effective-version format",
			args: []string{"telemetry-query", "--format", "effective-version-efficacy", "--workflow", "tutor", "--gaggle", "example"},
			want: "--format effective-version-efficacy",
		},
		{
			name: "the tutor-live-verification format",
			args: []string{"telemetry-query", "--format", "tutor-live-verification", "--gaggle", "example", "--aggregate", "gate-noise"},
			want: "--format tutor-live-verification",
		},
		{
			name: "a threshold for an unserved family",
			args: nominationArgs("--threshold", "min-ci-check-failure-runs=9"),
			want: "min-ci-check-failure-runs",
		},
		{
			name: "an instance path argument",
			args: nominationArgs(root),
			want: "has no meaning there",
		},
		{
			name: "another gaggle",
			args: []string{
				"telemetry-query", "--gaggle", "platform",
				"--aggregate", "gate-noise",
			},
			want: "contained to this stage's own gaggle",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, test.args...)
			if code == 0 {
				t.Fatalf("the invocation was served: %s", stdout)
			}
			if !strings.Contains(stderr, test.want) {
				t.Fatalf("stderr = %q, want it to name %q", stderr, test.want)
			}
			if stdout != "" {
				t.Fatalf("a refused invocation still emitted an artifact: %s", stdout)
			}
		})
	}
}

// TestTelemetryQueryFailsClosedOnPartialPlaneConfiguration pins that a pod
// holding half a plane configuration refuses rather than falling through to a
// local read of its own worktree, which would report no defects at all.
func TestTelemetryQueryFailsClosedOnPartialPlaneConfiguration(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	rebuildTelemetryQueryRollup(t, root)

	for _, test := range []struct {
		name string
		env  map[string]string
	}{
		{name: "endpoint without a token", env: map[string]string{
			telemetryclient.EnvEndpoint: "https://daemon.internal",
			telemetryclient.EnvGaggle:   "example",
		}},
		{name: "endpoint and token without a gaggle", env: map[string]string{
			telemetryclient.EnvEndpoint: "https://daemon.internal",
			telemetryclient.EnvToken:    "t",
		}},
		{name: "a hostile endpoint", env: map[string]string{
			telemetryclient.EnvEndpoint: "file:///etc/passwd",
			telemetryclient.EnvToken:    "t",
			telemetryclient.EnvGaggle:   "example",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			if _, ok := test.env[telemetryclient.EnvGaggle]; !ok {
				t.Setenv(telemetryclient.EnvGaggle, "")
			}
			code, stdout, stderr := runArgs(t, nominationArgs(root)...)
			if code == 0 {
				t.Fatalf("a partial plane configuration was served locally: %s", stdout)
			}
			if stdout != "" {
				t.Fatalf("a refused invocation still emitted an artifact: %s", stdout)
			}
			if !strings.Contains(stderr, "telemetry aggregate plane") {
				t.Fatalf("stderr = %q, want it to name the plane", stderr)
			}
		})
	}
}

// TestTelemetryQueryRefusesARootThatIsNotAnInstance is what replaced the
// dispatch refusal. Without it, the local path's "." fallback answers for a
// stage's own worktree with a well-formed artifact reporting no defects —
// the silent wrong result the refusal was standing in for.
func TestTelemetryQueryRefusesARootThatIsNotAnInstance(t *testing.T) {
	notAnInstance := t.TempDir()
	t.Setenv(telemetryclient.EnvEndpoint, "")
	t.Setenv(telemetryclient.EnvToken, "")
	t.Setenv(telemetryclient.EnvGaggle, "")

	t.Run("via the path argument", func(t *testing.T) {
		code, stdout, stderr := runArgs(t, "telemetry-query", "--window", "24h",
			"--aggregate", "stage-failure-rate", notAnInstance)
		if code == 0 {
			t.Fatalf("a non-instance root was served: %s", stdout)
		}
		if stdout != "" {
			t.Fatalf("a refused invocation still emitted an artifact: %s", stdout)
		}
		if !strings.Contains(stderr, "not a goobers instance") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("via the instance root environment", func(t *testing.T) {
		t.Setenv("GOOBERS_INSTANCE_ROOT", notAnInstance)
		code, stdout, stderr := runArgs(t, "telemetry-query", "--window", "24h",
			"--aggregate", "stage-failure-rate")
		if code == 0 {
			t.Fatalf("a non-instance root was served: %s", stdout)
		}
		if stdout != "" {
			t.Fatalf("a refused invocation still emitted an artifact: %s", stdout)
		}
		if !strings.Contains(stderr, "not a goobers instance") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("a real instance root is still served", func(t *testing.T) {
		root := initDemo(t)
		writeFixtureRunWithError(t, root)
		rebuildTelemetryQueryRollup(t, root)
		if code, _, stderr := runArgs(t, nominationArgs(root)...); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr)
		}
	})
}

// TestDefectAggregateServiceAnswersAnInstanceWithNoRollup pins the no-work
// answer. The local path emits an empty artifact carrying "no telemetry
// rollup yet"; the plane must say the same thing rather than 503, so the two
// artifacts stay identical.
func TestDefectAggregateServiceAnswersAnInstanceWithNoRollup(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if _, err := os.Stat(layout.TelemetryDB()); !os.IsNotExist(err) {
		t.Fatalf("the fixture is not rollup-free: %v", err)
	}
	service := newDaemonTelemetryDefectAggregateService(layout)
	response, err := service.DefectAggregates(t.Context(), planeRequestFor("example"))
	if err != nil {
		t.Fatalf("DefectAggregates() = %v, want the no-work answer", err)
	}
	if !response.NoWork || response.Note != telemetryQueryNoRollupNote {
		t.Fatalf("response = %+v, want the local path's no-rollup answer", response)
	}
	if response.Findings == nil || response.PromotionCandidates == nil {
		t.Fatal("the no-work answer must carry empty arrays, not nulls")
	}
}

// TestDefectAggregateServiceRevalidatesItsOwnScope pins that the derivation
// does not depend on its transport for containment: a hostile scope name is
// refused by the service itself.
func TestDefectAggregateServiceRevalidatesItsOwnScope(t *testing.T) {
	service := newDaemonTelemetryDefectAggregateService(instance.NewLayout(initDemo(t)))
	hostile := []struct {
		name     string
		gaggle   string
		workflow string
	}{
		{name: "traversal gaggle", gaggle: "../../etc"},
		{name: "empty gaggle", gaggle: ""},
		{name: "traversal workflow", gaggle: "example", workflow: "../../etc"},
	}
	for _, test := range hostile {
		t.Run(test.name, func(t *testing.T) {
			request := planeRequestFor(test.gaggle)
			request.Workflow = test.workflow
			if _, err := service.DefectAggregates(t.Context(), request); err == nil {
				t.Fatal("a hostile scope was accepted by the derivation")
			}
		})
	}
}

// planeRequestFor is a minimal valid request: the derivation's own bounds are
// what these tests are about, not the handler's parsing.
func planeRequestFor(gaggle string) httpapi.TelemetryDefectAggregateRequest {
	return httpapi.TelemetryDefectAggregateRequest{
		Gaggle:     gaggle,
		Since:      time.Now().UTC().Add(-24 * time.Hour),
		Aggregates: telemetryclient.AdmittedAggregates(),
	}
}

// writeFixtureRunWithErrorCode is writeFixtureRunWithError with a caller-chosen
// error code, so a test can plant a code that MUST NOT survive normalization.
func writeFixtureRunWithErrorCode(t *testing.T, root, runID, gaggle, code string) {
	t.Helper()
	l := instance.NewLayout(root)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "default-implement",
		WorkflowVersion: 1,
		Gaggle:          gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create fixture run: %v", err)
	}
	defer func() { _ = jr.Close() }()
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventError, Stage: "implement", Attempt: 1,
		Error: &journal.ErrorDetail{Code: code, Message: "fixture-injected failure"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultFailure),
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
}
