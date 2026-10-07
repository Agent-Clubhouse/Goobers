package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
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

func TestDefectAggregateResponsePreservesFaultAudit(t *testing.T) {
	now := time.Date(2026, time.September, 25, 7, 0, 0, 0, time.UTC)
	audit := &creditgraph.FaultAuditReport{
		Schema: creditgraph.FaultAuditSchemaVersion, Mode: "report-only",
		Since: now.Add(-time.Hour), Until: now, ObservationsScanned: 3,
		ProductFindings: []creditgraph.FaultFinding{{
			ID: "backprop-00000000000000000000", Signature: "runtime-failure",
			Domain: creditgraph.FaultDomainProductRuntime, Confidence: 0.9,
			RunIDs: []string{"run-1"}, Workflows: []string{"implementation"},
			EffectiveVersions: []string{"version-1"}, Environments: []string{"windows"},
			NodePaths: [][]string{{"stage:implement", "node:runtime"}},
			Evidence:  []creditgraph.AttributionEvidenceLink{{RunID: "run-1", JournalSequence: 12}},
			Rationale: "repeated runtime failure", AlternativeDomains: []string{string(creditgraph.FaultDomainUnknown)},
			RecommendedOwner: "product", RecommendedAction: "investigate",
			Verification: creditgraph.VerificationPending,
		}},
		Suppressed: 1, Truncated: true,
	}

	wire := defectAggregateResponse(candidateFindingsArtifact{FaultAudit: audit})
	roundTrip := candidateFindingsFromPlane(time.Hour, audit.Since, wire)
	got, err := json.Marshal(roundTrip.FaultAudit)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("fault audit drifted across defect plane\n got: %s\nwant: %s", got, want)
	}
}

// TestTelemetryQueryPlaneMatchesTheLocalResult is the parity check. Same
// instance, same window, same thresholds, same aggregates: the artifact a
// dispatched pod receives must be the artifact the daemon would have written
// locally, modulo the ONE difference the ruling asks for — normalized error
// signatures.
func TestTelemetryQueryPlaneMatchesTheLocalResult(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	writeAttributedCreditRun(t, root, "attribution-run-1")
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
	localCohorts, err := json.Marshal(local.AttributionCohorts)
	if err != nil {
		t.Fatal(err)
	}
	planeCohorts, err := json.Marshal(planeArtifact.AttributionCohorts)
	if err != nil {
		t.Fatal(err)
	}
	if string(planeCohorts) != string(localCohorts) {
		t.Fatalf("attribution cohorts drifted from the local derivation\nplane:    %s\nlocal:    %s", planeCohorts, localCohorts)
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
			args: nominationArgs("--aggregate", "workflow-untriggered"),
			want: "workflow-untriggered",
		},
		{
			name: "the learning-episode aggregate",
			args: nominationArgs("--aggregate", "learning-episode"),
			want: "learning-episode",
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
			args: nominationArgs("--threshold", "min-learning-episode-runs=9"),
			want: "min-learning-episode-runs",
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

// ciCheckArgs is the exact invocation the pod-placed goobers/test-suite-quality
// workflow's first stage makes (#6707).
func ciCheckArgs(extra ...string) []string {
	args := []string{
		"telemetry-query",
		"--window", "72h",
		"--gaggle", "example",
		"--aggregate", "ci-check-failure",
		"--threshold", "min-ci-check-failure-runs=2",
		"--format", "candidate-findings",
	}
	return append(args, extra...)
}

// writeFixtureRunWithCICheckFailures writes a run whose ci-poll stage finished
// with the named checks failing, in the shape insertCICheckFailures ingests.
func writeFixtureRunWithCICheckFailures(t *testing.T, root, runID string, checks ...string) {
	t.Helper()
	dir := filepath.Join(instance.NewLayout(root).RunsDir(), runID)
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Hour)
	runYAML := fmt.Sprintf("schema: goobers.dev/journal/run/v1\nrunId: %s\nworkflow: merge-review\nworkflowVersion: 1\ngaggle: example\ntrigger:\n  kind: item\n  ref: issue-42\nstartedAt: %s\n",
		runID, started.Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "run.yaml"), []byte(runYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	type check struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	var list []check
	for _, name := range checks {
		list = append(list, check{Name: name, State: "failing"})
	}
	artifact, err := json.Marshal(map[string]any{"checks": list})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "artifacts", "ci-checks.json"), artifact, 0o644); err != nil {
		t.Fatal(err)
	}
	ts := func(offset int) string {
		return started.Add(time.Duration(offset) * time.Second).Format(time.RFC3339Nano)
	}
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`, ts(0)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"stage.started","stage":"ci-poll","attempt":1,"attemptClass":"policy"}`, ts(1)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"stage.finished","stage":"ci-poll","attempt":1,"status":"failure","outputs":{"ciStatus":"failing"},"artifacts":[{"path":"artifacts/ci-checks.json","digest":%q,"size":%d,"mediaType":"application/json"}]}`,
			ts(2), journal.Digest(artifact), len(artifact)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":4,"branch":0,"time":%q,"type":"run.finished","status":"failed"}`, ts(3)),
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTelemetryQueryPlaneServesCICheckFailure is #6707's acceptance test: the
// exact test-suite-quality invocation answers from the plane, identically to
// the local path, with the threshold honoured and the findings carrying only
// derived data (check name, distinct-run count, flagged run ids).
func TestTelemetryQueryPlaneServesCICheckFailure(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithCICheckFailures(t, root, "ci-run-1", "make ci", "lint")
	writeFixtureRunWithCICheckFailures(t, root, "ci-run-2", "make ci")
	rebuildTelemetryQueryRollup(t, root)

	code, localOut, stderr := runArgs(t, ciCheckArgs(root)...)
	if code != 0 {
		t.Fatalf("local: code = %d, stderr = %q", code, stderr)
	}
	local := decodeCandidateFindings(t, localOut)
	if len(local.Findings) != 1 || local.Findings[0].Subject != "make ci" {
		t.Fatalf("local findings = %+v, want exactly the recurring 'make ci'", local.Findings)
	}

	plane := newTelemetryReadPlane(t, root)
	token := plane.admitRun(t, "example", "pod-run-1")
	plane.stamp(t, token, "example")

	code, planeOut, stderr := runArgs(t, ciCheckArgs()...)
	if code != 0 {
		t.Fatalf("plane: code = %d, stderr = %q", code, stderr)
	}
	validateCandidateFindings(t, []byte(planeOut))
	planeArtifact := decodeCandidateFindings(t, planeOut)
	planeJSON, err := json.Marshal(planeArtifact.Findings)
	if err != nil {
		t.Fatal(err)
	}
	localJSON, err := json.Marshal(local.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if string(planeJSON) != string(localJSON) {
		t.Fatalf("ci-check-failure drifted from the local derivation\nplane: %s\nlocal: %s", planeJSON, localJSON)
	}
	finding := planeArtifact.Findings[0]
	if finding.Kind != rollup.FindingCICheckFailure || finding.Metrics["distinctRuns"] != 2 || finding.Threshold != 2 || len(finding.FlaggedRuns) != 2 {
		t.Fatalf("unexpected finding shape: %+v", finding)
	}

	// The threshold crosses the wire: raising it above the observed count
	// must clear the finding on the plane, as it does locally.
	code, planeOut, stderr = runArgs(t, ciCheckArgs("--threshold", "min-ci-check-failure-runs=3")...)
	if code != 0 {
		t.Fatalf("plane (raised threshold): code = %d, stderr = %q", code, stderr)
	}
	if got := decodeCandidateFindings(t, planeOut); len(got.Findings) != 0 || !got.NoWork {
		t.Fatalf("raised threshold was not honoured on the plane: %s", planeOut)
	}
}

// TestDefectAggregateSanitizesCICheckNames pins the redaction choice for the
// one new subject: check names pass through, but control characters are
// dropped and the length is bounded, so a hostile check name cannot smuggle
// a multi-line or unbounded string across the plane.
func TestDefectAggregateSanitizesCICheckNames(t *testing.T) {
	if got := sanitizeCICheckSubject("build (ubuntu-latest)"); got != "build (ubuntu-latest)" {
		t.Fatalf("an ordinary check name was rewritten: %q", got)
	}
	if got := sanitizeCICheckSubject("lint\n\x1b[31mINJECT\x00"); strings.ContainsAny(got, "\n\x1b\x00") {
		t.Fatalf("control characters survived: %q", got)
	}
	long := strings.Repeat("x", 5000)
	if got := sanitizeCICheckSubject(long); len([]rune(got)) != maxCICheckSubjectRunes {
		t.Fatalf("length = %d, want %d", len([]rune(got)), maxCICheckSubjectRunes)
	}
	redacted := redactFindingForPlane(rollup.Finding{Kind: rollup.FindingCICheckFailure, Subject: "a\nb"})
	if redacted.Subject != "ab" {
		t.Fatalf("redactFindingForPlane subject = %q", redacted.Subject)
	}
	// Other families are untouched by the ci-check branch.
	other := redactFindingForPlane(rollup.Finding{Kind: rollup.FindingGateNeverFails, Subject: "gate\nx"})
	if other.Subject != "gate\nx" {
		t.Fatalf("a non-ci finding was rewritten: %q", other.Subject)
	}
}
