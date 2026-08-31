package main

import (
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"context"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/providers"
)

// fileissuesplane_test.go is the stage-side evidence for Goobers#3996
// blocker 2.
//
// file-issues is the one stage whose OUTPUT depends on a journal read: under
// decision 004 it may apply goobers:approved only to a nomination whose
// evidence matches a tool finding in the signals stage's stdout artifact of
// this run, byte for byte. Before this change that read opened a run
// directory under GOOBERS_INSTANCE_ROOT by path. The dispatcher does not
// stamp that variable into a stage pod, so in a pod the read failed, the
// stage still exited 0, and every nomination was filed unapproved — a silent
// wrong result, not a refusal (#3996's "fails soft, not loud").
//
// So the property under test is not "the read works". It is that the pod and
// the daemon reach the SAME approval decision on the same run, and that every
// way of not being allowed to look is loud.

// fileIssuesPlane is the daemon a stage pod talks to: the real
// readservice over the instance layout behind the real httpapi router, with
// real podauth bearers. Nothing about the journal read is faked, because the
// question is precisely whether the daemon's scrubbed projection still
// answers the byte-for-byte comparison the approval bar performs.
type fileIssuesPlane struct {
	server   *httptest.Server
	registry *podauth.Registry
}

// journalBackedReader serves the three run-scoped journal reads from the REAL
// readservice over an instance layout, and refuses everything else. The
// journal reads must be real: the daemon answers them from its own scrubbed
// projection, and whether that projection still supports a byte-for-byte
// evidence comparison is the entire question.
type journalBackedReader struct {
	*telemetryParityReader
	offline readservice.OfflineRuns
}

func (r *journalBackedReader) RunEvents(ctx context.Context, run string) (readservice.EventList, error) {
	return r.offline.RunEvents(ctx, run)
}

func (r *journalBackedReader) StageAttempts(ctx context.Context, run, stage string) (readservice.AttemptList, error) {
	return r.offline.StageAttempts(ctx, run, stage)
}

func (r *journalBackedReader) Artifact(ctx context.Context, run, digest string) (readservice.ArtifactContent, error) {
	return r.offline.Artifact(ctx, run, digest)
}

func newFileIssuesPlane(t *testing.T, root string) *fileIssuesPlane {
	t.Helper()
	offline, err := readservice.NewOfflineRuns(layoutFor(root))
	if err != nil {
		t.Fatal(err)
	}
	reader := &journalBackedReader{telemetryParityReader: &telemetryParityReader{}, offline: offline}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(reader, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &fileIssuesPlane{server: server, registry: registry}
}

// stampPodEnv puts the stage in the shape the dispatcher actually produces:
// the four journal-plane variables stamped, a bearer scoped to the journal
// plane and to this run, and NO instance root — the pod has no run directory
// and must not be able to pretend it does.
func (p *fileIssuesPlane) stampPodEnv(t *testing.T, runID, gaggle string) {
	t.Helper()
	p.stampPodEnvFor(t, runID, runID, gaggle)
}

// stampPodEnvFor stamps a bearer minted for bearerRun while the stage claims
// to be runID, so a cross-run attempt can be driven end to end.
func (p *fileIssuesPlane) stampPodEnvFor(t *testing.T, runID, bearerRun, gaggle string) {
	t.Helper()
	token, err := p.registry.MintScoped(bearerRun, 0, httpapi.ScopeJournal)
	if err != nil {
		t.Fatal(err)
	}
	setPlaneEnv(t, p.server.URL, token, runID, gaggle)
	// A pod's GOOBERS_INSTANCE_ROOT is unstamped, so the CLI's root argument
	// falls back to the working directory. Pointing it at an empty directory
	// is the honest reproduction: any path-based arm would find nothing.
	t.Setenv(executor.InstanceRootEnvVar, t.TempDir())
}

// TestFileIssuesApprovesTheSameNominationsInAPodAsOnTheDaemon is the parity
// claim, and it is deliberately an EQUALITY between two runs of the real
// command rather than a list of expected numbers: the bar is that moving a
// stage into a pod changes nothing an operator can observe, so the daemon's
// own answer is the oracle.
func TestFileIssuesApprovesTheSameNominationsInAPodAsOnTheDaemon(t *testing.T) {
	daemonResult := func() fileIssuesResult {
		f := newFileIssuesFixture(t)
		clearPlaneEnv(t)
		f.recordSignals(fileIssuesTestRunID, fileIssuesTestSignals)
		f.enableAutoApprove(true)
		f.writeArtifact(confirmed("vet-close"), lowRisk("unconfirmed"))
		result := f.mustRun()
		if result.Approved != 1 || result.Unapproved != 1 {
			t.Fatalf("the daemon shape is not the oracle this test assumes: %+v", result)
		}
		return result
	}()

	f := newFileIssuesFixture(t)
	f.recordSignals(fileIssuesTestRunID, fileIssuesTestSignals)
	f.enableAutoApprove(true)
	f.writeArtifact(confirmed("vet-close"), lowRisk("unconfirmed"))
	// The journal the daemon serves is the fixture's own; the pod then loses
	// its path to it.
	plane := newFileIssuesPlane(t, f.root)
	plane.stampPodEnv(t, fileIssuesTestRunID, "goobers")

	podResult := f.mustRun()

	if podResult.Approved != daemonResult.Approved || podResult.Unapproved != daemonResult.Unapproved ||
		podResult.Created != daemonResult.Created || podResult.Filed != daemonResult.Filed {
		t.Fatalf("pod and daemon disagree on what was approved:\n pod    = %+v\n daemon = %+v", podResult, daemonResult)
	}
	if !reflect.DeepEqual(podResult.Findings, daemonResult.Findings) {
		t.Fatalf("pod and daemon disagree on the findings read:\n pod    = %+v\n daemon = %+v",
			podResult.Findings, daemonResult.Findings)
	}
	if !podResult.Findings.Available || podResult.Findings.Vet != 2 {
		t.Fatalf("the pod did not read the signals artifact: %+v", podResult.Findings)
	}
	if len(podResult.Issues) != len(daemonResult.Issues) {
		t.Fatalf("filed %d issues in the pod, %d on the daemon", len(podResult.Issues), len(daemonResult.Issues))
	}
	for i := range podResult.Issues {
		pod, daemon := podResult.Issues[i], daemonResult.Issues[i]
		if pod.Key != daemon.Key || pod.Approved != daemon.Approved ||
			strings.Join(pod.ApprovalUnmet, "|") != strings.Join(daemon.ApprovalUnmet, "|") {
			t.Fatalf("issue %d differs:\n pod    = %+v\n daemon = %+v", i, pod, daemon)
		}
	}
	// And the approval is real on the provider side, applied by the approve
	// credential — a pod that merely reported Approved:1 without labelling
	// would satisfy every assertion above.
	for _, issue := range podResult.Issues {
		if !issue.Approved {
			continue
		}
		number := f.issueNumber(issue.IssueID)
		if !f.server.issueHasLabel(number, providers.LabelApproved) {
			t.Fatalf("issue #%s reported approved but carries labels %v", issue.IssueID, f.server.issueLabels(number))
		}
	}
}

// TestFileIssuesInAPodCannotReadAnotherRunsSignals is the containment claim.
// The interesting attack is not "read /etc/passwd" — there is no path
// argument to abuse — it is a stage that names a SIBLING run whose signals
// output happens to contain the finding its nomination needs. Confirming
// against another run's tool output would approve a defect nobody observed in
// this run.
func TestFileIssuesInAPodCannotReadAnotherRunsSignals(t *testing.T) {
	f := newFileIssuesFixture(t)
	// The sibling run in the same gaggle DOES contain the vet finding.
	f.recordSignals("run-someone-else", fileIssuesTestSignals)
	f.enableAutoApprove(true)
	f.writeArtifact(confirmed("cross-run"))

	plane := newFileIssuesPlane(t, f.root)
	// The bearer is this run's. This run's journal has no signals artifact.
	plane.stampPodEnv(t, fileIssuesTestRunID, "goobers")

	result := f.mustRun()
	if result.Approved != 0 || result.Unapproved != 1 {
		t.Fatalf("result = %+v; want the nomination filed unapproved", result)
	}
	if result.Findings.Available {
		t.Fatalf("the stage read findings it has no run for: %+v", result.Findings)
	}
	// Explicit, not empty: an "available with zero findings" answer would be
	// indistinguishable from a clean repository.
	if result.Findings.Reason == "" {
		t.Fatal("no reason was recorded for the unreadable artifact")
	}
	if f.server.issueHasLabel(f.issueNumber(result.Issues[0].IssueID), providers.LabelApproved) {
		t.Fatal("approved against another run's signals output")
	}
}

// clearPlaneEnv puts the stage off the plane entirely — the type-1/type-2
// host shape — WITHOUT clearing GOOBERS_RUN_ID, which is the stage's own
// identity rather than a plane variable and is required either way.
func clearPlaneEnv(t *testing.T) {
	t.Helper()
	t.Setenv(journalclient.EnvEndpoint, "")
	t.Setenv(journalclient.EnvToken, "")
	t.Setenv(journalclient.EnvGaggle, "")
}

// TestFileIssuesInAPodCannotUseAnotherRunsBearer drives the cross-run refusal
// through the real daemon: the stage claims this run and presents a bearer
// minted for a sibling, and the plane's containment check refuses. It is
// listed separately from the containment test above because it exercises the
// SERVER's decision rather than the client's — the two must both hold, since
// either one alone can be bypassed by a stage that constructs its own client.
func TestFileIssuesInAPodCannotUseAnotherRunsBearer(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.recordSignals(fileIssuesTestRunID, fileIssuesTestSignals)
	f.enableAutoApprove(true)
	f.writeArtifact(confirmed("wrong-bearer"))

	plane := newFileIssuesPlane(t, f.root)
	plane.stampPodEnvFor(t, fileIssuesTestRunID, "run-someone-else", "goobers")

	result := f.mustRun()
	if result.Findings.Available {
		t.Fatalf("a bearer for another run read this run's signals: %+v", result.Findings)
	}
	if result.Approved != 0 || result.Unapproved != 1 {
		t.Fatalf("result = %+v; want nothing approved", result)
	}
	if !strings.Contains(result.Findings.Reason, "403") && !strings.Contains(result.Findings.Reason, "run_mismatch") {
		t.Fatalf("reason = %q; want the plane's refusal, not a silent absence", result.Findings.Reason)
	}
	if f.server.issueHasLabel(f.issueNumber(result.Issues[0].IssueID), providers.LabelApproved) {
		t.Fatal("approved on a read the plane refused")
	}
}

// TestFileIssuesFailsClosedOnAPlaneItCannotUse pins the half-wired shapes as
// FATAL rather than merely unapproved. The distinction matters: "the signals
// artifact records nothing" is a fact about the run and files unapproved,
// while "I was not allowed to look" is a fact about this stage's own
// configuration, and treating the second as the first is how a lane files a
// whole backlog unapproved forever without anyone noticing (#3996).
func TestFileIssuesFailsClosedOnAPlaneItCannotUse(t *testing.T) {
	t.Run("endpoint without a bearer", func(t *testing.T) {
		f := newFileIssuesFixture(t)
		f.recordSignals(fileIssuesTestRunID, fileIssuesTestSignals)
		f.enableAutoApprove(true)
		f.writeArtifact(confirmed("half-wired"))
		// A reachable run directory is present, so a fall-through to the file
		// backend would succeed and APPROVE. That is the failure this pins.
		setPlaneEnv(t, "http://daemon.invalid:7777", "", fileIssuesTestRunID, "goobers")

		code, _, stderr := f.run()
		if code != 1 {
			t.Fatalf("code = %d, want a hard failure\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "journal plane is configured but unusable") {
			t.Fatalf("stderr = %q, want it to name the unusable plane", stderr)
		}
		if f.issueCount() != 0 {
			t.Fatalf("filed %d issue(s) while the plane was unusable", f.issueCount())
		}
	})

	t.Run("endpoint without a run identity", func(t *testing.T) {
		f := newFileIssuesFixture(t)
		f.enableAutoApprove(true)
		f.writeArtifact(confirmed("no-run"))
		setPlaneEnv(t, "http://daemon.invalid:7777", "tok", "", "goobers")

		code, _, stderr := f.run()
		if code != 1 {
			t.Fatalf("code = %d, want a hard failure\nstderr: %s", code, stderr)
		}
		if f.issueCount() != 0 {
			t.Fatalf("filed %d issue(s) with no run identity", f.issueCount())
		}
	})
}

// TestFileIssuesIsDispatchable is the other half of #3996: the fix is
// worthless if the stage still cannot be pinned to a pod. file-issues must
// require neither the instance root nor the instance config, because it now
// asks for everything it needs over the plane.
//
// It is stated here, next to the parity evidence, because the two claims are
// only safe TOGETHER — declaring the stage dispatchable while its journal
// read was path-based is exactly the state that produced the silent
// unapproved backlog.
func TestFileIssuesIsDispatchable(t *testing.T) {
	command := []string{"goobers", "file-issues"}
	if executor.StageRequiresInstanceRoot(command, "") {
		t.Fatal("file-issues still demands the instance root, so it cannot be pinned to a pod")
	}
	if executor.StageRequiresInstanceConfig(command) {
		t.Fatal("file-issues still demands the instance config, so it cannot be pinned to a pod")
	}
}

// TestFileIssuesPodReadIsBoundedAndVerified proves the two content bounds
// hold on the real command, not only in the client's unit tests: an artifact
// the daemon serves with substituted bytes is refused rather than parsed for
// findings. A tampered signals artifact is the one input that could make the
// approval bar approve something no tool reported.
func TestFileIssuesPodReadIsBoundedAndVerified(t *testing.T) {
	f := newFileIssuesFixture(t)
	f.recordSignals(fileIssuesTestRunID, fileIssuesTestSignals)
	f.enableAutoApprove(true)
	f.writeArtifact(confirmed("tampered"))

	plane := newFileIssuesPlane(t, f.root)
	plane.stampPodEnv(t, fileIssuesTestRunID, "goobers")

	// Corrupt the blob the daemon will serve, leaving the journal's recorded
	// digest untouched: the classic substitution.
	corruptRunBlobs(t, layoutFor(f.root), fileIssuesTestRunID)

	result := f.mustRun()
	if result.Findings.Available {
		t.Fatalf("a tampered signals artifact was parsed: %+v", result.Findings)
	}
	if result.Approved != 0 || result.Unapproved != 1 {
		t.Fatalf("result = %+v; want nothing approved", result)
	}
	// The daemon catches it first (it verifies before serving) and the client
	// would catch it again if it did not; either way the stage must be told
	// integrity failed rather than handed the substituted bytes.
	if !strings.Contains(result.Findings.Reason, "integrity") && !strings.Contains(result.Findings.Reason, "digest") {
		t.Fatalf("reason = %q; want it to name the integrity failure", result.Findings.Reason)
	}
	if strings.Contains(result.Findings.Reason, "substituted content") {
		t.Fatal("the substituted bytes reached the stage")
	}
}

// corruptRunBlobs rewrites every artifact blob in runID's journal with bytes
// that do not hash to their recorded digest.
func corruptRunBlobs(t *testing.T, layout instance.Layout, runID string) {
	t.Helper()
	runDir, err := layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	touched := 0
	err = filepath.WalkDir(filepath.Join(runDir, "artifacts"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		touched++
		return os.WriteFile(path, []byte("substituted content"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if touched == 0 {
		t.Fatal("no artifact blob was corrupted; the fixture recorded none")
	}
}

// TestFileIssuesLocalReaderIsUnchangedOffThePlane is the compatibility claim.
// Type-1 and type-2 hosts run this stage as a subprocess with no plane
// variables at all, and the change must be invisible to them: the same file
// backend, the same approval, and no network.
func TestFileIssuesLocalReaderIsUnchangedOffThePlane(t *testing.T) {
	f := newFileIssuesFixture(t)
	clearPlaneEnv(t)
	f.recordSignals(fileIssuesTestRunID, fileIssuesTestSignals)
	f.enableAutoApprove(true)
	f.writeArtifact(confirmed("local"))

	reader, err := stageRunJournal(f.root, fileIssuesTestRunID)
	if err != nil {
		t.Fatalf("stageRunJournal: %v", err)
	}
	if _, isFile := reader.(*journalclient.File); !isFile {
		t.Fatalf("reader = %T, want the file backend off the plane", reader)
	}

	result := f.mustRun()
	if result.Approved != 1 || !result.Findings.Available || result.Findings.Vet != 2 {
		t.Fatalf("result = %+v; want the unchanged local approval", result)
	}
}
