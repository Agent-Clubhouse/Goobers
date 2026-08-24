package main

// e2ekillinject.go implements internal/e2e.CellDriver against a real cluster
// (#3513/#3517): the live driver the killmatrix.go doc comment calls
// "TOPOLOGY-PENDING... no implementation exists in this package." Deliberately
// minimal (agreed scope, 2026-08-23): ONE kill cell — pod-kill during a
// builtin-class stage attempt — not the full six-cell matrix. Node-kill and
// the agentic/local-ci stage classes are out of scope here; RunKillMatrix
// (which drives all six cells through one CellDriver) is not wired up by this
// command for the same reason — a driver that only implements one cell would
// make RunKillMatrix's "all six must succeed in one procedure" contract fail
// on the other five by construction, which is a worse signal than not
// claiming to run the matrix at all.
//
// Consumer-side orchestration (goobernetes-smoke.md §5 rule 1: no kubectl
// exec anywhere in the procedure) cannot reach the daemon's local instance
// root the way `goobers e2e verify` does (that command reads on-disk journal
// files, which in mode 3 live on the daemon pod's RWO instance volume that
// nothing else mounts) — so this command reads run/stage state over the
// daemon's own HTTP read API (internal/httpapi, apicontract.RouteStageAttempts
// / RouteRunDetail) instead, and performs the pod delete directly against the
// cluster using the invoking process's own Kubernetes credentials (the same
// in-cluster/kubeconfig resolution internal/dispatcher's own pod API uses —
// see dispatchKubeClient in workerdispatch.go, reused here unchanged). No
// kubectl exec: this is an ordinary pod delete via client-go, the same
// operation the dispatcher itself already has RBAC for.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/e2e"
	"github.com/goobers/goobers/internal/readservice"
)

const e2eKillInjectHelp = `Usage: goobers e2e kill-inject --daemon-url <url> --run <run-id> --stage <stage-name>
       --stage-class builtin --namespace <k8s-namespace> [--token <bearer>]
       [--poll-timeout <duration>] [--out <record.json>]

Perform ONE live S6 kill-matrix cell (#3513/#3517, goobernetes-smoke.md §4
S6) against a real cluster: pod-kill only, one stage class per invocation.
This is the minimal live CellDriver implementation — it does not run the
full six-cell matrix (RunKillMatrix), and it does not implement node-kill.

--stage names the REAL workflow stage (e.g. "probe-builtin") whose currently
executing attempt should be killed; --stage-class records which of S6's
three abstract classes (builtin, agentic, local-ci) that stage plays, for
the emitted record only — it is never used to find the attempt.

Procedure:
  1. Poll --daemon-url's read API (StageAttempts) for --run/--stage until an
     attempt is running with recorded pod placement.
  2. Delete that pod via the Kubernetes API (client-go — in-cluster config
     when running as a pod, else standard kubeconfig loading), in
     --namespace. No kubectl exec.
  3. Poll again for a successor attempt and the run's terminal phase.
  4. Emit one internal/e2e.CellInjectionRecord as JSON — to --out, or
     stdout.

--poll-timeout bounds BOTH polling phases independently (default 10m each) —
this command fails loudly on timeout rather than hanging forever; a partial
record (injection performed, no successor observed yet) is still emitted
with RunCompletedSuccessfully=false, since D5 requires every injection be
recorded even when the retry hasn't landed by the time this command gives up.

Exit codes:
  0 = injection performed and a complete record emitted (successor attempt
      observed, run reached a terminal phase)
  1 = injection performed but the record is incomplete (timed out waiting
      for a successor or run completion) — the record is still emitted
  2 = usage / IO / network error, or the target attempt never appeared
      within --poll-timeout (nothing was injected, no record produced)
`

// pollSleep is the delay between polling attempts — a seam so tests can
// replace it with a no-op instead of a real 5-second wait.
var pollSleep = time.Sleep

func runE2EKillInject(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("e2e kill-inject", flag.ContinueOnError)
	fs.SetOutput(stderr)
	daemonURL := fs.String("daemon-url", "", "base URL of the daemon's HTTP read API, e.g. http://localhost:8080 (required)")
	token := fs.String("token", "", "bearer token for the daemon's read API, if it requires one")
	runID := fs.String("run", "", "run id whose stage attempt to kill (required)")
	stage := fs.String("stage", "", "the real workflow stage name to target (required)")
	stageClass := fs.String("stage-class", "", "which S6 stage class this stage plays: builtin, agentic, or local-ci (required)")
	namespace := fs.String("namespace", "", "Kubernetes namespace the target pod runs in (required)")
	pollTimeout := fs.Duration("poll-timeout", 10*time.Minute, "bound on each polling phase (finding the target attempt, and waiting for a successor)")
	outPath := fs.String("out", "", "write the injection record here instead of stdout")
	fs.Usage = helpUsage(stderr, "e2e kill-inject")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	var missing []string
	for _, f := range []struct{ name, value string }{
		{"daemon-url", *daemonURL}, {"run", *runID}, {"stage", *stage},
		{"stage-class", *stageClass}, {"namespace", *namespace},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		pf(stderr, "error: --%s is required\n\n", strings.Join(missing, ", --"))
		fs.Usage()
		return 2
	}

	cell := e2e.KillMatrixCell{Failure: e2e.FailureKindPodKill}
	switch e2e.StageClass(*stageClass) {
	case e2e.StageClassBuiltin, e2e.StageClassAgentic, e2e.StageClassLocalCI:
		cell.Stage = e2e.StageClass(*stageClass)
	default:
		pf(stderr, "error: --stage-class %q is not one of builtin, agentic, local-ci\n", *stageClass)
		return 2
	}

	client := &e2eDaemonClient{baseURL: strings.TrimRight(*daemonURL, "/"), token: *token, http: &http.Client{Timeout: 30 * time.Second}}
	ctx := context.Background()

	pf(stderr, "waiting for a running attempt of stage %q (run %s)...\n", *stage, *runID)
	target, err := waitForRunningAttempt(ctx, client, *runID, *stage, *pollTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if target.Placement == nil || target.Placement.Pod == "" {
		pf(stderr, "error: stage %q attempt %s is running but carries no pod placement — nothing to kill\n", *stage, target.ID)
		return 2
	}
	pf(stderr, "found running attempt %s on pod %s; deleting...\n", target.ID, target.Placement.Pod)

	kubeClient, err := dispatchKubeClient()
	if err != nil {
		pf(stderr, "error: build kubernetes client: %v\n", err)
		return 2
	}
	podAPI := dispatcher.NewKubernetesPodAPI(kubeClient)
	injectedAt := time.Now()
	if err := podAPI.DeletePod(ctx, *namespace, target.Placement.Pod); err != nil {
		pf(stderr, "error: delete pod %s/%s: %v\n", *namespace, target.Placement.Pod, err)
		return 2
	}

	record := e2e.CellInjectionRecord{
		Cell:               cell,
		RunID:              *runID,
		InjectedAt:         injectedAt,
		InjectedTarget:     target.Placement.Pod,
		InterruptedAttempt: target,
	}

	pf(stderr, "waiting for a successor attempt and run completion...\n")
	successor, runCompleted, waitErr := waitForSuccessorAndCompletion(ctx, client, *runID, *stage, target, *pollTimeout)
	if successor != nil {
		record.SuccessorAttempt = successor
	}
	record.RunCompletedSuccessfully = runCompleted

	out := stdout
	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			pf(stderr, "error: create --out %s: %v\n", *outPath, err)
			return 2
		}
		out = outFile
	}
	encodeErr := json.NewEncoder(out).Encode(record)
	if outFile != nil {
		closeErr := outFile.Close()
		if encodeErr == nil {
			encodeErr = closeErr
		}
	}
	if encodeErr != nil {
		pf(stderr, "error: write injection record: %v\n", encodeErr)
		return 2
	}

	if waitErr != nil {
		pf(stderr, "warning: %v (record emitted with what was observed)\n", waitErr)
		return 1
	}
	return 0
}

// e2eDaemonClient is a minimal HTTP client over the daemon's read API
// (internal/httpapi) — the routes apicontract.RouteStageAttempts and
// RouteRunDetail expose, decoded directly into their readservice response
// types since the wire shape is that type's JSON tags verbatim (see
// internal/httpapi/router.go's writeJSON(w, http.StatusOK, attempts) calls).
type e2eDaemonClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func (c *e2eDaemonClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *e2eDaemonClient) stageAttempts(ctx context.Context, runID, stage string) (readservice.AttemptList, error) {
	var out readservice.AttemptList
	err := c.get(ctx, fmt.Sprintf("/api/v1/runs/%s/stages/%s/attempts", runID, stage), &out)
	return out, err
}

func (c *e2eDaemonClient) runDetail(ctx context.Context, runID string) (readservice.RunDetail, error) {
	var out readservice.RunDetail
	err := c.get(ctx, fmt.Sprintf("/api/v1/runs/%s", runID), &out)
	return out, err
}

// waitForRunningAttempt polls stageAttempts until one is Status=="running"
// with a non-nil Placement, or timeout elapses.
func waitForRunningAttempt(ctx context.Context, client *e2eDaemonClient, runID, stage string, timeout time.Duration) (readservice.StageAttempt, error) {
	deadline := time.Now().Add(timeout)
	for {
		list, err := client.stageAttempts(ctx, runID, stage)
		if err != nil {
			return readservice.StageAttempt{}, fmt.Errorf("poll stage attempts: %w", err)
		}
		for _, attempt := range list.Attempts {
			if attempt.Status == "running" && attempt.Placement != nil && attempt.Placement.Pod != "" {
				return attempt, nil
			}
		}
		if time.Now().After(deadline) {
			return readservice.StageAttempt{}, fmt.Errorf("no running attempt of stage %q with pod placement appeared within %s", stage, timeout)
		}
		pollSleep(5 * time.Second)
	}
}

// waitForSuccessorAndCompletion polls for an attempt after interrupted (a
// later Visit/Number, or the same identity resolved differently once it
// settles) and for the run to reach a terminal phase. Bounded by timeout;
// returns whatever was observed plus a non-nil error on timeout rather than
// blocking forever (D5 still wants the injection recorded even if the retry
// never lands within this command's patience).
func waitForSuccessorAndCompletion(ctx context.Context, client *e2eDaemonClient, runID, stage string, interrupted readservice.StageAttempt, timeout time.Duration) (*readservice.StageAttempt, bool, error) {
	deadline := time.Now().Add(timeout)
	var successor *readservice.StageAttempt
	for {
		list, err := client.stageAttempts(ctx, runID, stage)
		if err == nil {
			for i := range list.Attempts {
				a := list.Attempts[i]
				if a.ID == interrupted.ID {
					continue
				}
				if a.Placement != nil && a.Placement.Pod != "" && a.Placement.Pod != interrupted.Placement.Pod {
					successor = &a
				}
			}
		}
		detail, detailErr := client.runDetail(ctx, runID)
		if detailErr == nil && detail.Terminal {
			return successor, detail.Phase == "completed", nil
		}
		if time.Now().After(deadline) {
			return successor, false, fmt.Errorf("run %s did not reach a terminal phase within %s of the injection", runID, timeout)
		}
		pollSleep(5 * time.Second)
	}
}
