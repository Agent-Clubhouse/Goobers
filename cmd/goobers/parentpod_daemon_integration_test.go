//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Every daemon service, journal writer, authority registry and queue connection
// dies with this process. Restart must reconstruct them through ordinary up.
func TestIntegrationQualificationDaemonProcess(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_QUALIFICATION_DAEMON_ROOT")
	root := os.Getenv("GOOBERS_QUALIFICATION_DAEMON_ROOT")
	if os.Getenv("GOOBERS_CHILD_KUBE_QUALIFICATION") != "1" || !filepath.IsAbs(root) || !strings.Contains(root, "TestIntegrationContainedParentSurvivesDaemonProcessLoss") {
		t.Fatal("explicit disposable qualification root required")
	}
	// The external forge is a local Git fixture; daemon/recovery owners remain real.
	source := os.Getenv("GOOBERS_QUALIFICATION_SOURCE_REPO")
	if !filepath.IsAbs(source) || !strings.Contains(source, "TestIntegrationContainedParentSurvivesDaemonProcessLoss") {
		t.Fatal("explicit local source fixture required")
	}
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Setenv("GOOBERS_DISABLE_FSYNC", "")
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(cfg.API.Listen)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("daemon must bind literal loopback")
	}
	host, _, err = net.SplitHostPort(cfg.Engine.HostPort)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("Temporal must be literal loopback")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if code := runUpContext(ctx, []string{"--quiet", root}, os.Stdout, os.Stderr); code != 0 {
		t.Fatalf("daemon exited %d", code)
	}
}

func startQualificationDaemon(t *testing.T, root, address string) *qualificationHostProcess {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestIntegrationQualificationDaemonProcess$", "-test.v")
	command.Env = append(os.Environ(), "GOOBERS_QUALIFICATION_DAEMON_ROOT="+root)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &qualificationHostProcess{command: command, done: make(chan error, 1)}
	go func() { process.done <- command.Wait() }()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+apicontract.InstanceReadinessPath, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		ready := false
		if response != nil {
			var state httpapi.InstanceReadiness
			ready = err == nil && response.StatusCode == http.StatusOK && json.NewDecoder(response.Body).Decode(&state) == nil && state.Ready
			_ = response.Body.Close()
		}
		cancel()
		if ready {
			return process
		}
		select {
		case err := <-process.done:
			t.Fatal("daemon exited before readiness", err)
		case <-deadline.C:
			process.kill(t)
			t.Fatal("daemon never became ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func qualificationDaemonJSON(ctx context.Context, address, method, path string, body, out any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "daemon-restart-qualification")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, response.StatusCode, data)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out)
}

func TestIntegrationContainedParentSurvivesDaemonProcessLoss(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	image := os.Getenv("GOOBERS_PARENT_QUALIFICATION_IMAGE")
	if !strings.HasPrefix(image, "localhost:45081/goobers:haw-parent-") {
		t.Fatal("explicit local image required")
	}
	_, namespace := childQualificationKubernetes(t)
	keyPath := filepath.Join(t.TempDir(), "pod.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("q", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	probe := "#!/bin/sh\ncase \"$*\" in\n --version) echo '2.1.0 (qualification preflight)' ;;\n 'auth status') echo '{\"loggedIn\":true}' ;;\n *) echo 'parent execution reached the host' >&2; exit 70 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(probe), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QUALIFICATION_MODEL_TOKEN", "qualification-model-only")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "qualification-for-local-git-only")
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "qualification forge does not implement this operation", http.StatusNotFound)
	}))
	t.Cleanup(forge.Close)
	t.Setenv("GOOBERS_TEST_GITHUB_API_URL", forge.URL)
	f := realParentQualificationFixture(t, image, keyPath)
	// The shared compiler fixture creates a synthetic parent for unit tests.
	// Remove only that fixture journal before the real daemon admits any work.
	fake, err := f.layout.FindRunDir(f.parent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(fake); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	writeFileContent(t, filepath.Join(source, "source.txt"), "parent source\n")
	writeFileContent(t, filepath.Join(source, "qualification-mode"), "daemon-restart")
	childStarted := parentQualificationCancellationProbe(t, source)
	recoveryCLIGit(t, source, "add", ".")
	recoveryCLIGit(t, source, "commit", "-m", "qualification base")
	t.Setenv("GOOBERS_QUALIFICATION_SOURCE_REPO", source)
	dev, err := temporaltest.StartDevServer(t.Context(), t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dev.Stop() })
	address := freeLoopbackAddress(t)
	f.cfg.API.Listen = address
	f.cfg.Engine.HostPort = dev.FrontendHostPort()
	if err := instance.WriteConfig(f.layout.ConfigFile(), f.cfg); err != nil {
		t.Fatal(err)
	}
	_, machine, err := childStageCatalog(f.cfg, f.applied, childworkflow.ParentSelection{Gaggle: "example", Workflow: f.parent.Workflow, Stage: "plan"}, childworkflow.BackendRunner)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := bootstrap.PinStagePlacements(f.cfg, f.applied, "example", machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	queues := []string{f.cfg.EffectiveEngineConfig().TaskQueue}
	for _, pin := range pins {
		queues = append(queues, pin.Queue)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	host := startQualificationHostProcess(t, qualificationHostConfig{HostPort: dev.FrontendHostPort(), Queues: queues, InstanceID: f.parent.InstanceID, Namespace: namespace, Image: image, Endpoint: net.JoinHostPort("host.docker.internal", port), KeyPath: keyPath, SurrenderRoot: filepath.Join(f.layout.BlobStoreDir(), "surrender")})
	t.Cleanup(func() { host.stop(t) })
	daemon := startQualificationDaemon(t, f.layout.Root, address)
	t.Cleanup(func() {
		if daemon != nil {
			daemon.stop(t)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	var accepted httpapi.TriggerResponse
	if err := qualificationDaemonJSON(ctx, address, http.MethodPost, apicontract.TriggerIngestPath, httpapi.TriggerRequest{Gaggle: "example", Workflow: f.parent.Workflow, RequestID: "daemon-restart-qualification"}, &accepted); err != nil {
		t.Fatal(err)
	}
	runID := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
	runPath := apicontract.RunsPath + "/" + runID
	var original readservice.ChildHistoryItem
	restarted := false
	for {
		var detail readservice.RunDetail
		err := qualificationDaemonJSON(ctx, address, http.MethodGet, runPath, nil, &detail)
		if err == nil {
			if detail.Phase != journal.PhaseRunning && detail.Phase != "" {
				if detail.Phase != journal.PhaseCompleted {
					t.Fatalf("parent did not complete: phase=%s cause=%+v", detail.Phase, detail.TerminalCause)
				}
				if !restarted {
					t.Fatal("parent completed without daemon loss")
				}
				var history readservice.ChildHistoryPage
				if err := qualificationDaemonJSON(ctx, address, http.MethodGet, runPath+"/children", nil, &history); err != nil {
					t.Fatal(err)
				}
				if len(history.Items) != 1 || history.Items[0].ChildID != original.ChildID || history.Items[0].RunID != original.RunID || history.Items[0].StageOccurrence != original.StageOccurrence || !history.Items[0].AcceptedAt.Equal(original.AcceptedAt) || history.Items[0].State != triggerqueue.ChildCompleted || history.Items[0].AcknowledgedAt == nil {
					t.Fatalf("restart changed accepted child: before=%+v after=%+v", original, history)
				}
				if detail.ChildActivity != nil {
					t.Fatal("completed parent retained wait")
				}
				var childDetail readservice.RunDetail
				if err := qualificationDaemonJSON(ctx, address, http.MethodGet, apicontract.RunsPath+"/"+original.RunID, nil, &childDetail); err != nil {
					t.Fatal(err)
				}
				if childDetail.ChildActivity == nil || childDetail.ChildActivity.Parent == nil || childDetail.ChildActivity.Parent.RunID != runID {
					t.Fatal("recovered child lost its Portal parent link")
				}
				assertDaemonQualificationCustody(t, ctx, f.layout, dev.Client(), runID, 3)
				assertDaemonQualificationCustody(t, ctx, f.layout, dev.Client(), original.RunID, 2)
				break
			}
			if !restarted && detail.ChildActivity != nil && detail.ChildActivity.Parked {
				select {
				case <-childStarted:
					var history readservice.ChildHistoryPage
					if err := qualificationDaemonJSON(ctx, address, http.MethodGet, runPath+"/children", nil, &history); err != nil {
						t.Fatal(err)
					}
					if len(history.Items) != 1 || history.Items[0].State.Terminal() {
						t.Fatal("child was not live before crash", history)
					}
					original = history.Items[0]
					t.Logf("kill daemon with parked parent %s and live child %s", runID, original.RunID)
					daemon.kill(t)
					daemon = nil
					daemon = startQualificationDaemon(t, f.layout.Root, address)
					restarted = true
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("daemon recovery journey did not finish", ctx.Err(), err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Read exact retained inputs from host-authored markers, then verify every
// immutable worker result. A terminal Portal card alone is not stopped proof.
func assertDaemonQualificationCustody(t *testing.T, ctx context.Context, layout instance.Layout, transport childpod.TemporalClient, runID string, want int) {
	t.Helper()
	dir, err := layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	joined := map[string]bool{}
	for _, event := range events {
		if event.Runner["kind"] == childPodWriterJoined || event.Runner["kind"] == childpod.ParentWriterJoined {
			digest, _ := event.Runner["contractDigest"].(string)
			joined[digest] = true
		}
	}
	pods := map[string]bool{}
	attempts := 0
	for _, event := range events {
		if event.Runner["kind"] != childPodWriterStarted && event.Runner["kind"] != childpod.ParentWriterStarted {
			continue
		}
		attempts++
		digest, _ := event.Runner["contractDigest"].(string)
		if digest == "" || !joined[digest] {
			t.Fatal("physical writer was not durably joined")
		}
		data, err := json.Marshal(event.Runner["retainedAttempt"])
		if err != nil {
			t.Fatal(err)
		}
		var ref journal.Ref
		if err := json.Unmarshal(data, &ref); err != nil {
			t.Fatal(err)
		}
		retained, err := childpod.ReadRetainedAttempt(reader, ref)
		if err != nil {
			t.Fatal(err)
		}
		if retained.Input.Attempt.RunID != runID || retained.Input.Attempt.ChildExecutionDigest != digest {
			t.Fatal("retained physical attempt changed")
		}
		var result engine.ChildDispatchResult
		if err := transport.GetWorkflow(ctx, engine.ChildDispatchWorkflowID(retained.Input.Attempt), "").Get(ctx, &result); err != nil {
			t.Fatal(err)
		}
		if result.BindingDigest != retained.Input.BindingDigest() || result.DisposalFailed || result.Report.ChildPodUID == "" || !result.Report.WorkspaceWritersStopped || !result.Report.SurrenderConfirmed {
			t.Fatal("worker result lacks exact stopped/surrender/disposal proof")
		}
		if pods[result.Report.ChildPodUID] {
			t.Fatal("different physical attempts share a pod UID")
		}
		pods[result.Report.ChildPodUID] = true
	}
	if attempts != want {
		t.Fatalf("physical invocations for %s: got %d, want %d", runID, attempts, want)
	}
}
