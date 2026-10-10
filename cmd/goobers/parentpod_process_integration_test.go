//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/workerhost"
)

type qualificationHostConfig struct {
	HostPort                                                       string
	Queues                                                         []string
	InstanceID, Namespace, Image, Endpoint, KeyPath, SurrenderRoot string
}

type qualificationHostProcess struct {
	command *exec.Cmd
	done    chan error
}

func startQualificationHostProcess(t *testing.T, cfg qualificationHostConfig) *qualificationHostProcess {
	t.Helper()
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestQualificationDispatchHostProcess$", "-test.v")
	command.Env = append(os.Environ(), "GOOBERS_QUALIFICATION_HOST_CONFIG="+path)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	p := &qualificationHostProcess{command: command, done: make(chan error, 1)}
	go func() { p.done <- command.Wait() }()
	return p
}

func (p *qualificationHostProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Success() {
			t.Fatal("worker was not killed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("killed worker did not exit")
	}
}

func (p *qualificationHostProcess) stop(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Error("worker host exited unsuccessfully", err)
		}
	case <-time.After(workerhost.ChildSettlementTimeout + 10*time.Second):
		_ = p.command.Process.Kill()
		<-p.done
		t.Error("worker host did not stop within its custody window")
	}
}

// This helper runs in a separate OS process so Kill cannot execute Host.Run's
// deferred cleanup. All runtime owners are production implementations; only
// their wiring targets the disposable local qualification infrastructure.
func TestQualificationDispatchHostProcess(t *testing.T) {
	path := os.Getenv("GOOBERS_QUALIFICATION_HOST_CONFIG")
	if path == "" {
		t.Skip("subprocess helper")
	}
	if os.Getenv("GOOBERS_CHILD_KUBE_QUALIFICATION") != "1" {
		t.Fatal("explicit qualification opt-in required")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg qualificationHostConfig
	if err := json.Unmarshal(payload, &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.Namespace, "haw-child-qualification-") || !strings.HasPrefix(cfg.Image, "localhost:45081/goobers:haw-parent-") {
		t.Fatal("non-qualification target")
	}
	kubePath := os.Getenv("GOOBERS_CHILD_QUALIFICATION_KUBECONFIG")
	if kubePath == "" {
		t.Fatal("explicit qualification kubeconfig required")
	}
	raw, err := clientcmd.LoadFromFile(kubePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw.CurrentContext, "kind-haw-child-") {
		t.Fatal("only disposable kind context allowed")
	}
	rest, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(rest.Host)
	if err != nil || target.Hostname() != "127.0.0.1" {
		t.Fatal("Kubernetes must be literal loopback")
	}
	api, err := kubernetes.NewForConfig(rest)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := podauth.NewSignedKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	plane, err := dispatcher.NewSurrenderDir(cfg.SurrenderRoot)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := dispatcher.New(dispatcher.Config{InstanceID: cfg.InstanceID, Owner: "parent-qualification", GaggleNamespaces: map[string]string{"example": cfg.Namespace}, GaggleServiceAccounts: map[string]string{"example": "qualification-stage"}, EmbeddedVersion: strings.TrimPrefix(cfg.Image, "localhost:45081/goobers:"), TokenMinter: key, BlobEndpoint: "http://" + cfg.Endpoint, WriteAPIBase: "http://" + cfg.Endpoint, SupervisionInterval: 100 * time.Millisecond, LinuxScheduleToStart: 30 * time.Second}, dispatcher.NewKubernetesPodAPI(api), nil, dispatcher.PlaneSurrenderGate{Plane: plane}, nil)
	if err != nil {
		t.Fatal(err)
	}
	host, err := workerhost.New(workerhost.Config{HostPort: cfg.HostPort, Namespace: "default", TaskQueues: cfg.Queues, DrainTimeout: time.Second, Deps: bootstrap.EngineDeps{Dispatcher: dispatch, Surrenders: plane}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := host.Run(ctx); err != nil {
		t.Fatal(fmt.Errorf("qualification host: %w", err))
	}
}

// Kill after Temporal has actually retained the API-observed UID, rather than
// pretending a process-local callback was durable. The separate missing-receipt
// tests require refusal and never discover a replacement by pod name.
func awaitQualificationChildCustody(t *testing.T, ctx context.Context, transport *observedChildTemporal) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		for id, input := range transport.snapshot() {
			// The generated child fixture uses stage check; parent invocations use plan.
			if input.Attempt.Stage != "check" {
				continue
			}
			description, err := transport.DescribeWorkflowExecution(deadline, id, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, pending := range description.PendingActivities {
				var custody engine.ChildDispatchCustody
				if pending.HeartbeatDetails != nil && converter.GetDefaultDataConverter().FromPayloads(pending.HeartbeatDetails, &custody) == nil && custody.BindingDigest == input.BindingDigest() && custody.Pod.UID != "" {
					t.Logf("kill dispatch process after durable custody for pod %s UID %s", custody.Pod.Name, custody.Pod.UID)
					return
				}
			}
		}
		select {
		case <-deadline.Done():
			t.Fatal("worker never retained child custody heartbeat", deadline.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
