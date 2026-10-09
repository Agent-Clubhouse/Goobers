package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"maps"
	"sync"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

type isolatedChildKey struct{}

func isolatedChildWorkspace(ctx context.Context) bool {
	v, _ := ctx.Value(isolatedChildKey{}).(bool)
	return v
}

func runChildDispatchContext(ctx context.Context, stdout, stderr io.Writer) int {
	if err := childpod.VerifyEntrypoint(); err != nil {
		pf(stderr, "dispatch-child: %v\n", err)
		return 1
	}
	contract, digest, err := prepareChildPod(ctx)
	if err != nil {
		pf(stderr, "dispatch-child: admission: %v\n", err)
		return 1
	}
	usage := &isolatedObservedUsage{}
	ctx = invoke.WithAgentUsageReporter(ctx, usage.report)
	outcome := runIsolatedChildStage(ctx, contract, stdout, stderr)
	// Foreground completion does not prove custody: detached descendants
	// must stop before the supervisor reads any return-tree files.
	custody, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err = childpod.Quiesce(custody); err != nil {
		pf(stderr, "dispatch-child: writer custody: %v\n", err)
		return 1
	}
	output := childpod.Output{Version: 1, ContractDigest: digest}
	if contract.Workspace != nil {
		carrier, _, captureErr := childpod.CaptureCarrier(custody, ".", contract.Workspace.Snapshot.Record.RepositoryKey, contract.Identity.RunID, contract.StartedAt, contract.Workspace.Snapshot.Policy)
		if captureErr != nil {
			pf(stderr, "dispatch-child: capture: %v\n", captureErr)
			return 1
		}
		output.Workspace = &carrier
	}
	data, err := json.Marshal(output)
	if err != nil || len(data) > childpod.MaxContractBytes {
		pf(stderr, "dispatch-child: output exceeds custody bound\n")
		return 1
	}
	outputDigest := journal.Digest(data)
	if err = podBlobClient().Put(custody, outputDigest, data); err != nil {
		pf(stderr, "dispatch-child: retain output: %v\n", err)
		return 1
	}
	mutations, err := podMutationReceipts()
	if err != nil {
		pf(stderr, "dispatch-child: mutation evidence: %v\n", err)
		return 1
	}
	metrics, reported := usage.snapshot()
	result := dispatcher.SurrenderedResult{ObservedUsage: metrics, ObservedUsageReported: reported, RecoveryAcknowledged: true, Result: outcome.Result, Verdict: outcome.Verdict, Mutations: mutations, ChildWorkspaceDigest: outputDigest}
	data, err = json.Marshal(result)
	if err != nil {
		return 1
	}
	client := dispatcher.SurrenderPutClient{BaseURL: os.Getenv(dispatcher.EnvDaemonAPI), Token: os.Getenv(dispatcher.EnvPodToken), RetryDeadline: time.Minute}
	if err = client.Put(custody, contract.Identity.RunID, contract.Stage, contract.PodAttempt, data); err != nil {
		pf(stderr, "dispatch-child: surrender: %v\n", err)
		return 1
	}
	return 0
}

func runIsolatedChildStage(ctx context.Context, contract childpod.Contract, stdout, stderr io.Writer) stageOutcome {
	token := os.Getenv(dispatcher.EnvPodToken)
	owned, stop, err := remoteChildExecutionFence(context.WithValue(ctx, isolatedChildKey{}, true), os.Getenv(dispatcher.EnvDaemonAPI), token, contract)
	defer stop()
	if err != nil {
		return stageOutcome{Result: failureEnvelope("execution_authority_ended", err.Error())}
	}
	owned, heartbeat := startPodStageHeartbeat(owned, stderr)
	outcome := runStage(owned, stdout, stderr)
	heartbeat.Stop()
	if err := context.Cause(owned); err != nil {
		failed := failureEnvelope("execution_authority_ended", err.Error())
		failed.Transcript, failed.Artifacts = outcome.Result.Transcript, outcome.Result.Artifacts
		return stageOutcome{Result: failed}
	}
	return outcome
}

func prepareChildPod(ctx context.Context) (childpod.Contract, string, error) {
	digest := os.Getenv(dispatcher.EnvChildExecutionDigest)
	client := podBlobClient()
	if client == nil {
		return childpod.Contract{}, digest, fmt.Errorf("child blob custody is unavailable")
	}
	data, err := client.GetBounded(ctx, digest, childpod.MaxContractBytes)
	if err != nil {
		return childpod.Contract{}, digest, err
	}
	c, err := childpod.DecodeContract(data, digest)
	if err != nil {
		return c, digest, err
	}
	if c.KitDigest != os.Getenv(dispatcher.EnvAgenticKitDigest) || c.Identity.RunID != os.Getenv(dispatcher.EnvRunID) || c.Identity.Gaggle != os.Getenv(dispatcher.EnvGaggle) || c.Identity.InstanceID != os.Getenv(dispatcher.EnvInstanceID) || c.Stage != os.Getenv(dispatcher.EnvStage) || strconv.Itoa(c.Attempt) != os.Getenv(dispatcher.EnvAttempt) || strconv.Itoa(c.PodAttempt) != os.Getenv(dispatcher.EnvPodAttempt) {
		return c, digest, fmt.Errorf("child pod identity does not match contract")
	}
	if err = cleanChildPodEnvironment(); err != nil {
		return c, digest, err
	}
	if c.Workspace != nil {
		err = childpod.Materialize(ctx, ".", *c.Workspace)
	}
	return c, digest, err
}

func cleanChildPodEnvironment() error {
	var allowed []string
	if err := json.Unmarshal([]byte(os.Getenv(dispatcher.EnvStageEnvAllow)), &allowed); err != nil {
		return fmt.Errorf("invalid child environment allowlist")
	}
	allowed = append(allowed, dispatcher.DispatcherControlEnv...)
	allowed = append(allowed, "PATH", "LANG", "LC_ALL", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(allowed, name) {
			if err := os.Unsetenv(name); err != nil {
				return err
			}
		}
	}
	for name, value := range map[string]string{"HOME": dispatcher.LinuxHomePath, "TMPDIR": dispatcher.LinuxTmpPath, "XDG_CONFIG_HOME": dispatcher.LinuxHomePath + "/.config", "XDG_CACHE_HOME": dispatcher.LinuxHomePath + "/.cache", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"} {
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	return nil
}

// Usage reports are adapter-owned snapshots, matching the runner's collector.
// The callback runs inside the pod supervisor and never reads Result.Metrics.
type isolatedObservedUsage struct {
	mu       sync.Mutex
	metrics  map[string]float64
	reported bool
}

func (u *isolatedObservedUsage) report(metrics map[string]float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reported = true
	if u.metrics == nil {
		u.metrics = make(map[string]float64)
	}
	for key, value := range metrics {
		if telemetry.IsCanonicalAgentUsageMetric(key) {
			u.metrics[key] = value
		}
	}
}
func (u *isolatedObservedUsage) snapshot() (map[string]float64, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return maps.Clone(u.metrics), u.reported
}
