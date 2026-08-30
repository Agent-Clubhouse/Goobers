package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/telemetryclient"
	"github.com/goobers/goobers/providers"
)

// telemetryReadPlane is the daemon's REAL telemetry read plane: podauth in
// front of a deny-all human fallback, httpapi.RequireRoles as the authorizer,
// the read service over the instance's own rollup, and the run-to-gaggle
// resolver cmd/goobers/up.go wires — the construction the daemon performs.
type telemetryReadPlane struct {
	server   *httptest.Server
	layout   instance.Layout
	registry *podauth.Registry
}

func newTelemetryReadPlane(t *testing.T, root string) *telemetryReadPlane {
	t.Helper()
	layout := instance.NewLayout(root)
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	telemetry, err := readservice.NewTelemetry(db)
	if err != nil {
		t.Fatal(err)
	}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(
		&telemetryParityReader{Telemetry: telemetry},
		httpapi.RequireRoles(),
		log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithPodRunGaggle(podRunGaggleResolver(layout)),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &telemetryReadPlane{server: server, layout: layout, registry: registry}
}

// admitRun creates runID's journal under gaggle and mints its pod token — the
// stage pod's identity as the daemon sees it.
func (p *telemetryReadPlane) admitRun(t *testing.T, gaggle, runID string) string {
	t.Helper()
	run, err := journal.Create(p.layout.ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "backlog-curation", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	token, err := p.registry.Mint(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// stamp is the environment a goobers-CLI stage pod carries for the telemetry
// read plane, so the production selection seam picks the plane backend.
func (p *telemetryReadPlane) stamp(t *testing.T, token, gaggle string) {
	t.Helper()
	t.Setenv(telemetryclient.EnvEndpoint, p.server.URL)
	t.Setenv(telemetryclient.EnvToken, token)
	t.Setenv(telemetryclient.EnvGaggle, gaggle)
}

// seedImplementationOutcomes writes the terminal implementation runs the
// feedback evidence is computed from and rebuilds the rollup they project
// into.
func seedImplementationOutcomes(t *testing.T, root string, now time.Time) {
	t.Helper()
	writeImplementationOutcomeRun(t, root, "fail-7-a", "7", journal.PhaseFailed, now.Add(-7*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-7-b", "7", journal.PhaseEscalated, now.Add(-6*time.Hour))
	writeImplementationOutcomeRun(t, root, "fail-8-a", "8", journal.PhaseFailed, now.Add(-5*time.Hour))
	rebuildTelemetryQueryRollup(t, root)
}

// TestStageImplementationOutcomesParityAcrossTheSeam is the parity proof
// decision 005 R4 turns on: the evidence a stage pod reads over the plane is
// the SAME evidence the same stage reads off the instance's rollup outside a
// pod. Byte for byte, through the real router, authorizer, and gaggle
// containment.
func TestStageImplementationOutcomesParityAcrossTheSeam(t *testing.T) {
	root := initDemo(t)
	now := time.Now().UTC()
	seedImplementationOutcomes(t, root, now)
	since := now.Add(-8 * time.Hour)

	t.Setenv("GOOBERS_GAGGLE", "example")
	local, err := stageImplementationOutcomes(context.Background(), root, since)
	if err != nil {
		t.Fatalf("local rollup read: %v", err)
	}
	if len(local) == 0 {
		t.Fatal("fixture produced no local evidence; the parity claim would be vacuous")
	}

	plane := newTelemetryReadPlane(t, root)
	token := plane.admitRun(t, "example", "curation-run-1")
	plane.stamp(t, token, "example")

	overPlane, err := stageImplementationOutcomes(context.Background(), root, since)
	if err != nil {
		t.Fatalf("plane read: %v", err)
	}
	if !reflect.DeepEqual(overPlane, local) {
		t.Fatalf("plane evidence = %+v\nlocal evidence = %+v", overPlane, local)
	}
}

// TestStageImplementationOutcomesFailsClosed proves the seam never degrades a
// refusal into "no evidence". Every case below has a perfectly readable local
// rollup sitting right there, so a silent fall-through would look like
// success and quietly stop de-readying anything forever.
func TestStageImplementationOutcomesFailsClosed(t *testing.T) {
	root := initDemo(t)
	now := time.Now().UTC()
	seedImplementationOutcomes(t, root, now)
	since := now.Add(-8 * time.Hour)
	plane := newTelemetryReadPlane(t, root)

	t.Run("endpoint without a bearer", func(t *testing.T) {
		t.Setenv("GOOBERS_GAGGLE", "example")
		t.Setenv(telemetryclient.EnvEndpoint, plane.server.URL)
		t.Setenv(telemetryclient.EnvToken, "")
		if _, err := stageImplementationOutcomes(context.Background(), root, since); !errors.Is(err, telemetryclient.ErrEndpointWithoutToken) {
			t.Fatalf("error = %v, want ErrEndpointWithoutToken", err)
		}
	})

	t.Run("endpoint without a gaggle", func(t *testing.T) {
		t.Setenv("GOOBERS_GAGGLE", "")
		t.Setenv(telemetryclient.EnvEndpoint, plane.server.URL)
		t.Setenv(telemetryclient.EnvToken, plane.admitRun(t, "example", "curation-run-2"))
		if _, err := stageImplementationOutcomes(context.Background(), root, since); !errors.Is(err, telemetryclient.ErrEndpointWithoutGaggle) {
			t.Fatalf("error = %v, want ErrEndpointWithoutGaggle", err)
		}
	})

	t.Run("another gaggle's evidence", func(t *testing.T) {
		token := plane.admitRun(t, "other", "curation-run-3")
		plane.stamp(t, token, "example")
		_, err := stageImplementationOutcomes(context.Background(), root, since)
		var planeErr *telemetryclient.Error
		if !errors.As(err, &planeErr) || planeErr.Code != "gaggle_mismatch" {
			t.Fatalf("error = %v, want a gaggle_mismatch refusal", err)
		}
	})

	t.Run("an unminted bearer", func(t *testing.T) {
		plane.stamp(t, "goobers-pod.deadbeef", "example")
		_, err := stageImplementationOutcomes(context.Background(), root, since)
		var planeErr *telemetryclient.Error
		if !errors.As(err, &planeErr) || planeErr.Status != 401 {
			t.Fatalf("error = %v, want a 401 refusal", err)
		}
	})
}

// TestBacklogHealthFeedbackOverTheReadPlane is the stage-level proof: the same
// --feedback run that de-readies a chronically failing issue off the local
// rollup does exactly the same thing when its evidence arrives over the plane,
// with no rollup file reachable at all.
//
// The instance root's rollup is REMOVED before the plane run, so the stage
// cannot be quietly reading it: the only evidence source left is the daemon.
func TestBacklogHealthFeedbackOverTheReadPlane(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	readyAt := now.Add(-8 * time.Hour)
	for _, number := range []int{7, 8} {
		server.addIssue(number, "Issue", "goobers:approved", "goobers:ready")
		server.setLabelEventTime(number, providers.LabelReady, true, readyAt)
	}
	seedImplementationOutcomes(t, root, now)

	plane := newTelemetryReadPlane(t, root)
	token := plane.admitRun(t, "example", "curation-run-feedback")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "feedback-run")
	t.Setenv(providersnapshot.EnvVar, "feedback-tick")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_IMPLEMENTATIONFAILURETHRESHOLD", "2")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "implementation-feedback.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	plane.stamp(t, token, "example")

	// The rollup the daemon serves stays open behind the plane; the copy the
	// stage could have read is gone.
	if err := os.Remove(instance.NewLayout(root).TelemetryDB()); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "backlog-health", "--feedback", root)
	if code != 0 {
		t.Fatalf("backlog-health --feedback: code = %d, stderr = %q", code, stderr)
	}

	server.mu.Lock()
	sevenLabels := append([]string(nil), server.issues[7].labels...)
	eightLabels := append([]string(nil), server.issues[8].labels...)
	comments := append([]string(nil), server.issues[7].comments...)
	server.mu.Unlock()
	if hasAllLabels(sevenLabels, []string{providers.LabelReady}) {
		t.Fatalf("issue 7 labels = %v, want ready removed from the plane's evidence", sevenLabels)
	}
	if !hasAllLabels(eightLabels, []string{providers.LabelReady}) {
		t.Fatalf("issue 8 labels = %v, want a one-off failure preserved", eightLabels)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "fail-7-a") {
		t.Fatalf("issue 7 comments = %v, want the plane's evidence quoted", comments)
	}

	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var report implementationFeedbackReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Recurated != 1 || len(report.Items) != 1 ||
		report.Items[0].ItemID != "7" || report.Items[0].ConsecutiveFailures != 2 {
		t.Fatalf("feedback report = %#v", report)
	}
}

// TestTelemetryQueryStaysRefusedOnTheEnginePath pins the half of decision 005
// R4 that did NOT move: the read plane serves the daemon's own rollup to a
// contained pod, and `telemetry-query` — which reaches EXTERNAL connectors
// (executor.KindExternalTelemetry) and is used only by the nomination lanes —
// is untouched by it and still refuses to dispatch.
func TestTelemetryQueryStaysRefusedOnTheEnginePath(t *testing.T) {
	if !executor.StageRequiresInstanceRoot([]string{"goobers", "telemetry-query"}, "") {
		t.Fatal("telemetry-query must stay refused on the dispatch path; the telemetry READ plane serves the daemon rollup, not external connectors")
	}
	if !executor.StageRequiresInstanceRoot(
		[]string{"goobers", "external-telemetry"},
		executor.KindExternalTelemetry,
	) {
		t.Fatal("kind=external-telemetry must stay refused on the dispatch path")
	}
	// The stage that this issue's read route DOES serve is unchanged too:
	// backlog-health's own refusal was never C3's to lift, and C2 has since
	// lifted it (Goobers#3948, its ready-transition ledger joining the
	// scheduler-state namespace). What must not drift is the boundary this
	// test draws: `--feedback` reads the telemetry plane, and that read alone
	// never required the instance root.
	if executor.StageRequiresInstanceRoot([]string{"goobers", "backlog-health", "--feedback"}, "") {
		t.Fatal("backlog-health is fully plane-served since Goobers#3948; a refusal here means a file dependency came back")
	}
}
