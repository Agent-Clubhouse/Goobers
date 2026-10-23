package chaos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FreezeSmokeImage is the pinned helper and target image of the freeze smoke.
const FreezeSmokeImage = "docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

const (
	smokeNamespace = "goobers-chaos-target"
	smokePod       = "freeze-target"
	smokeContainer = "beat"
)

const smokeTargetManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  containers:
  - name: %[3]s
    image: %[4]s
    command: ["sh", "-c", "while true; do date +%%s; sleep 1; done"]
`

// FreezeSmoke proves CgroupFreeze on a real cluster: it freezes a container
// that logs a heartbeat every second, requires the heartbeat to stall while
// frozen, thaws it, requires the heartbeat to resume, and requires the sweep
// to leave no helper pod. Each stage is appended to the returned evidence.
func FreezeSmoke(ctx context.Context, exec Executor, sleep func(time.Duration)) (evidence []string, err error) {
	note := func(format string, args ...any) { evidence = append(evidence, fmt.Sprintf(format, args...)) }
	if err := Sweep(ctx, exec, DefaultChaosNamespace); err != nil {
		return evidence, fmt.Errorf("sweep at start: %w", err)
	}
	note("sweep at start: clean")
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_, derr := exec.Run(cleanup, "", "kubectl", "delete", "namespace", smokeNamespace, "--ignore-not-found", "--wait=false")
		err = errors.Join(err, derr)
	}()
	if _, err := exec.Run(ctx, fmt.Sprintf(smokeTargetManifest, smokeNamespace, smokePod, smokeContainer, FreezeSmokeImage), "kubectl", "apply", "-f", "-"); err != nil {
		return evidence, err
	}
	if _, err := exec.Run(ctx, "", "kubectl", "wait", "--for=condition=Ready", "pod/"+smokePod, "-n", smokeNamespace, "--timeout=180s"); err != nil {
		return evidence, err
	}
	beat := func() (string, error) {
		out, err := exec.Run(ctx, "", "kubectl", "logs", smokePod, "-n", smokeNamespace, "-c", smokeContainer, "--tail=1")
		return strings.TrimSpace(out), err
	}
	sleep(3 * time.Second)
	running, err := beat()
	if err != nil {
		return evidence, err
	}
	if running == "" {
		return evidence, errors.New("target produced no heartbeat before the freeze")
	}
	note("heartbeat running: %s", running)

	fault := &CgroupFreeze{Exec: exec, Namespace: smokeNamespace, Pod: smokePod, Container: smokeContainer, HelperImage: FreezeSmokeImage}
	defer func() {
		if rerr := fault.Revert(context.WithoutCancel(ctx)); rerr != nil {
			err = errors.Join(err, fmt.Errorf("revert: %w", rerr))
		}
	}()
	if err := fault.Apply(ctx); err != nil {
		return evidence, fmt.Errorf("freeze: %w", err)
	}
	note("cgroup.events: frozen 1 (verified by Apply) for %s", fault.cgroup)
	sleep(2 * time.Second) // let the last pre-freeze line reach the kubelet log
	first, err := beat()
	if err != nil {
		return evidence, err
	}
	sleep(4 * time.Second)
	second, err := beat()
	if err != nil {
		return evidence, err
	}
	if first != second {
		return evidence, fmt.Errorf("heartbeat advanced while frozen (%s -> %s)", first, second)
	}
	note("heartbeat stalled while frozen at %s", second)

	if err := fault.Revert(ctx); err != nil {
		return evidence, fmt.Errorf("thaw: %w", err)
	}
	note("cgroup.events: frozen 0 (verified by Revert)")
	sleep(4 * time.Second)
	resumed, err := beat()
	if err != nil {
		return evidence, err
	}
	if resumed == second {
		return evidence, fmt.Errorf("heartbeat did not resume after thaw (still %s)", resumed)
	}
	note("heartbeat resumed at %s", resumed)

	if err := Sweep(ctx, exec, DefaultChaosNamespace); err != nil {
		return evidence, fmt.Errorf("sweep at end: %w", err)
	}
	// Sweep deletes with --wait=false, so give terminating helpers time to go.
	var left string
	for attempt := 0; ; attempt++ {
		out, err := exec.Run(ctx, "", "kubectl", "get", "pods", "-n", DefaultChaosNamespace, "-l", helperLabel, "--ignore-not-found", "-o", "name")
		if err != nil {
			return evidence, err
		}
		if left = strings.TrimSpace(out); left == "" {
			break
		}
		if attempt >= 30 {
			return evidence, fmt.Errorf("helper pods remain after sweep: %s", left)
		}
		sleep(2 * time.Second)
	}
	note("sweep at end: no helper pods remain")
	return evidence, nil
}
