package main

// e2ekillinject.go implements internal/e2e.CellDriver against a real cluster
// (#3513/#3517): the live driver the killmatrix.go doc comment calls
// "TOPOLOGY-PENDING... no implementation exists in this package." Deliberately
// minimal (agreed scope, 2026-08-23): ONE kill cell — pod-kill during one
// stage attempt — not the full six-cell matrix. Node-kill and the other two
// stage classes are out of scope here; RunKillMatrix (which drives all six
// cells through one CellDriver) is not wired up by this command for the same
// reason — a driver that only implements one cell would make RunKillMatrix's
// "all six must succeed in one procedure" contract fail on the other five by
// construction, which is a worse signal than not claiming to run the matrix
// at all.
//
// FINDS THE TARGET POD VIA LABEL SELECTOR, NOT THE DAEMON'S HTTP READ API.
// An earlier version of this command polled the daemon's read API
// (RouteStageAttempts) to find the running attempt's pod. That's unreachable
// in practice: the daemon's API listener defaults to (and, confirmed live
// against aks-goobernetes-prod, actually is) loopback-only
// (instance.DefaultAPIListenAddress = "127.0.0.1:8080") — nothing outside
// the daemon's own pod network namespace can reach it, and binding it wider
// with no auth just to support this one command is not a security posture
// worth trading for. Every stage pod the dispatcher creates already carries
// goobers.dev/run / goobers.dev/stage / goobers.dev/attempt labels
// (internal/dispatcher.LabelRun/LabelStage/LabelAttempt,
// podspec.go:stampedLabels) — this command finds its target with a plain
// label-selector ListPods call on the SAME dispatcher.PodAPI/client-go it
// already uses for the delete. No daemon read path, no auth, no kubectl
// exec: two ordinary pod-list/pod-delete calls the invoking process's own
// cluster credentials already permit (the same RBAC shape the dispatcher
// itself has).
//
// WHAT THIS MEANS THE EMITTED RECORD CANNOT CARRY: internal/e2e.
// CellInjectionRecord.InterruptedAttempt/SuccessorAttempt are
// readservice.StageAttempt values, whose rich fields (ID = the journal's own
// opaque attempt identity, Class, Error, Model, Outputs, Artifacts) only the
// daemon's read side (journal-derived) can produce. Pod-level Kubernetes
// data supplies a THINNER attempt view: identity approximated from the pod's
// name/labels, Status approximated from the pod phase, Placement built
// directly from the pod object (Pod/Node/OS) rather than read back from
// anywhere. RunCompletedSuccessfully is INTENTIONALLY LEFT FALSE — no pod
// carries the overall run's terminal phase, only its own stage's outcome,
// and guessing one for the other is exactly the kind of plausible-but-wrong
// substitution this whole effort has been about catching. ClassifyCellResult
// (internal/e2e) needs the real journal-sourced attempt data (Class/Error/
// RunCompletedSuccessfully) to render a real S6 verdict — that has to come
// from a SEPARATE step once the run's journal is reachable (e.g. `goobers
// e2e verify` run against an export of it, or a future daemon-side endpoint
// reachable off-loopback), combined with the record this command emits for
// InjectedAt/InjectedTarget. This command's own job is scoped to "perform
// the injection and record what a pod-only view can prove" — not to render
// S6's pass/fail verdict by itself.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/e2e"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

const e2eKillInjectHelp = `Usage: goobers e2e kill-inject --run <run-id> --stage <stage-name>
       --stage-class builtin --namespace <k8s-namespace>
       [--poll-timeout <duration>] [--out <record.json>]

Perform ONE live S6 kill-matrix cell (#3513/#3517, goobernetes-smoke.md §4
S6) against a real cluster: pod-kill only, one stage class per invocation.
This is the minimal live CellDriver implementation — it does not run the
full six-cell matrix (RunKillMatrix), and it does not implement node-kill.

--stage names the REAL workflow stage (e.g. "probe-builtin") whose currently
running pod should be killed; --stage-class records which of S6's three
abstract classes (builtin, agentic, local-ci) that stage plays, for the
emitted record only.

Finds the target pod by label selector (goobers.dev/run=<run-id>,
goobers.dev/stage=<stage>) — the same labels every dispatcher-created stage
pod carries (internal/dispatcher.LabelRun/LabelStage) — NOT by asking the
daemon: its read API defaults to loopback-only and cannot be reached from
outside its own pod. No kubectl exec anywhere.

Procedure:
  1. Poll for a Running pod matching the label selector.
  2. Delete it via the Kubernetes API (client-go — in-cluster config when
     running as a pod, else standard kubeconfig loading), in --namespace.
  3. Poll the same selector for a DIFFERENT pod (the dispatcher's retry) to
     appear.
  4. Emit one internal/e2e.CellInjectionRecord as JSON — to --out, or
     stdout. InterruptedAttempt/SuccessorAttempt are built from pod data
     only (name, labels, phase, node) — thinner than the journal-sourced
     StageAttempt goobers e2e verify produces, and RunCompletedSuccessfully
     is always false here: no pod carries the overall run's terminal phase,
     only its own stage's outcome, so this command does not guess it.
     Rendering S6's actual pass/fail verdict needs this record combined with
     the run's journal-sourced data separately.

--poll-timeout bounds BOTH polling phases independently (default 10m each) —
this command fails loudly on timeout rather than hanging forever; a partial
record (injection performed, no successor observed yet) is still emitted
with RunCompletedSuccessfully=false, since D5 requires every injection be
recorded even when the retry hasn't landed by the time this command gives up.

Exit codes:
  0 = injection performed and a successor pod was observed
  1 = injection performed but no successor pod appeared within
      --poll-timeout — the record is still emitted
  2 = usage / IO / Kubernetes API error, or the target pod never appeared
      within --poll-timeout (nothing was injected, no record produced)
`

func runE2EKillInject(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("e2e kill-inject", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runID := fs.String("run", "", "run id whose stage attempt to kill (required)")
	stage := fs.String("stage", "", "the real workflow stage name to target (required)")
	stageClass := fs.String("stage-class", "", "which S6 stage class this stage plays: builtin, agentic, or local-ci (required)")
	namespace := fs.String("namespace", "", "Kubernetes namespace the target pod runs in (required)")
	pollTimeout := fs.Duration("poll-timeout", 10*time.Minute, "bound on each polling phase (finding the target pod, and waiting for a successor)")
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
		{"run", *runID}, {"stage", *stage}, {"stage-class", *stageClass}, {"namespace", *namespace},
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

	ctx := context.Background()
	kubeClient, err := dispatchKubeClient()
	if err != nil {
		pf(stderr, "error: build kubernetes client: %v\n", err)
		return 2
	}
	podAPI := dispatcher.NewKubernetesPodAPI(kubeClient)
	selector := map[string]string{dispatcher.LabelRun: *runID, dispatcher.LabelStage: *stage}

	pf(stderr, "waiting for a running pod matching %s=%s, %s=%s in %s...\n", dispatcher.LabelRun, *runID, dispatcher.LabelStage, *stage, *namespace)
	target, err := waitForRunningPod(ctx, podAPI, *namespace, selector, "", *pollTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stderr, "found running pod %s (node %s); deleting...\n", target.Name, target.Spec.NodeName)

	injectedAt := time.Now()
	if err := podAPI.DeletePod(ctx, *namespace, target.Name); err != nil {
		pf(stderr, "error: delete pod %s/%s: %v\n", *namespace, target.Name, err)
		return 2
	}

	record := e2e.CellInjectionRecord{
		Cell:               cell,
		RunID:              *runID,
		InjectedAt:         injectedAt,
		InjectedTarget:     target.Name,
		InterruptedAttempt: attemptFromPod(target, *stage),
		// RunCompletedSuccessfully deliberately left false — see the package
		// doc comment above: no pod carries the run's overall terminal
		// phase, and this command does not substitute the stage's own
		// outcome for it.
	}

	pf(stderr, "waiting for a successor pod...\n")
	successorPod, waitErr := waitForRunningPod(ctx, podAPI, *namespace, selector, target.Name, *pollTimeout)
	if waitErr == nil {
		successor := attemptFromPod(successorPod, *stage)
		record.SuccessorAttempt = &successor
	}

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

// attemptFromPod builds a THIN readservice.StageAttempt from a pod alone —
// see the package doc comment for exactly which fields this can and cannot
// populate honestly. ID uses the pod name (the closest available identity;
// NOT the journal's own opaque attempt ID, which this command has no way to
// read). Number comes from the dispatcher's own LabelAttempt label when
// present and parseable; 0 otherwise (never guessed).
func attemptFromPod(pod corev1.Pod, stage string) readservice.StageAttempt {
	number := 0
	if raw, ok := pod.Labels[dispatcher.LabelAttempt]; ok {
		if n, err := strconv.Atoi(raw); err == nil {
			number = n
		}
	}
	status := string(pod.Status.Phase)
	return readservice.StageAttempt{
		ID:     pod.Name,
		Number: number,
		Status: status,
		Placement: &journal.Placement{
			Node: pod.Spec.NodeName,
			Host: pod.Spec.NodeName,
			Pod:  pod.Name,
		},
	}
}

// pollSleep is the delay between polling attempts — a seam so tests can
// replace it with a no-op instead of a real 5-second wait.
var pollSleep = time.Sleep

// waitForRunningPod polls ListPods on selector until a Running pod appears
// whose name is not excludeName (used to find a successor distinct from the
// pod just deleted; pass "" to accept any match), or timeout elapses. If
// more than one match is running, the first one found wins — the label
// selector is scoped to one run+stage, so more than one running match would
// itself be a S1 (fresh-pod) violation this command is not responsible for
// diagnosing.
func waitForRunningPod(ctx context.Context, podAPI dispatcher.PodAPI, namespace string, selector map[string]string, excludeName string, timeout time.Duration) (corev1.Pod, error) {
	deadline := time.Now().Add(timeout)
	for {
		pods, err := podAPI.ListPods(ctx, namespace, selector)
		if err != nil {
			return corev1.Pod{}, fmt.Errorf("list pods: %w", err)
		}
		for _, pod := range pods {
			if pod.Name == excludeName {
				continue
			}
			if pod.Status.Phase == corev1.PodRunning {
				return pod, nil
			}
		}
		if time.Now().After(deadline) {
			return corev1.Pod{}, fmt.Errorf("no running pod matching %v (excluding %q) appeared within %s", selector, excludeName, timeout)
		}
		pollSleep(5 * time.Second)
	}
}
