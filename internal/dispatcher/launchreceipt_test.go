package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
	"github.com/goobers/goobers/internal/podauth"
)

type receiptRecorderFunc func(context.Context, launchreceipt.Receipt) error

func (f receiptRecorderFunc) Record(ctx context.Context, r launchreceipt.Receipt) error {
	return f(ctx, r)
}

func boundLaunchAttempt(a Attempt) Attempt {
	a.LaunchBinding = &launchreceipt.Binding{RunID: a.RunID, Stage: a.Stage, Number: a.Number, Class: a.Class, Review: a.Review, StartedSeq: uint64(a.IdentityAttempt()), AttemptID: journal.StageAttemptID(a.RunID, 0, a.Stage, uint64(a.IdentityAttempt()))}
	return a
}

// Legacy dispatcher fixtures predate canonical controller binding. Supply it
// explicitly here; production Dispatch has no missing-binding compatibility bypass.
func dispatchFixture(d *Dispatcher, ctx context.Context, a Attempt, eligible []RunnerSpec) (Report, error) {
	return d.Dispatch(ctx, boundLaunchAttempt(a), eligible)
}

func TestPreparedLaunchMustPersistBeforeCreate(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "stored", true: "failed"}[failure], func(t *testing.T) {
			pods := &fakePodAPI{terminalPhase: corev1.PodSucceeded}
			cfg := testConfig()
			persisted := false
			cfg.LaunchReceipts = receiptRecorderFunc(func(_ context.Context, r launchreceipt.Receipt) error {
				if len(pods.created) != 0 {
					t.Fatal("pod created before durable receipt")
				}
				if r.Binding.AttemptID == "" || r.Facts.ResolvedModel != "unknown" {
					t.Fatal("missing identity/fidelity")
				}
				if failure {
					return errors.New("storage unavailable")
				}
				persisted = true
				return nil
			})
			d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.Dispatch(t.Context(), boundLaunchAttempt(testAttempt()), []RunnerSpec{linuxRunner()})
			if failure {
				if err == nil || len(pods.created) != 0 {
					t.Fatalf("launched after failed persistence: %v", err)
				}
				return
			}
			if err != nil || !persisted || len(pods.created) != 1 {
				t.Fatalf("successful persistence/launch: %v", err)
			}
		})
	}
}

func TestPreparedLaunchRefusesMissingAndCrossAttemptBinding(t *testing.T) {
	for _, kind := range []string{"missing recorder", "missing binding", "cross attempt", "wrong review"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig()
			a := boundLaunchAttempt(testAttempt())
			switch kind {
			case "missing recorder":
				cfg.LaunchReceipts = nil
			case "missing binding":
				a.LaunchBinding = nil
			case "cross attempt":
				a.LaunchBinding.Number++
			case "wrong review":
				a.LaunchBinding.Review = true
			}
			pods := &fakePodAPI{}
			d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Dispatch(t.Context(), a, []RunnerSpec{linuxRunner()}); err == nil || len(pods.created) != 0 {
				t.Fatal("unbound launch admitted")
			}
		})
	}
}

func TestPreparedPodFactsExcludeSecretsPathsAndUnobservedActuals(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatal(err)
	}
	stage := stageContainerIn(pod.Spec.Containers)
	const secret = "sensitive-token-and-host-path"
	stage.Env = append(stage.Env, corev1.EnvVar{Name: "TOKEN", Value: secret})
	stage.Command = []string{secret}
	stage.Args = []string{secret}
	stage.Image = secret
	stage.VolumeMounts = append(stage.VolumeMounts, corev1.VolumeMount{Name: secret, MountPath: "/" + secret})
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[secret] = secret
	pod.Labels[secret] = secret
	facts, err := preparedPodFacts(pod)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || facts.ImageReferenceDigest != launchreceipt.Digest([]byte(secret)) {
		t.Fatalf("unsafe facts: %s", raw)
	}
	if facts.NetworkEnforcement != "unknown" || facts.SandboxEnforcement != "unknown" || facts.ResolvedModel != "unknown" || facts.ResolvedEffort != "unknown" {
		t.Fatal("prepared pod falsely claimed in-pod observation")
	}
	if facts.RunAsNonRoot == nil || !*facts.RunAsNonRoot || !facts.DropAllCapabilities {
		t.Fatal("rendered policy omitted")
	}
}

func TestPreparedLaunchRealTransportFailureAndReplayCreateNoPod(t *testing.T) {
	key, err := podauth.NewSignedKey(bytes.Repeat([]byte{7}, podauth.MinSignedKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := launchreceipt.NewStore(root, key)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(stubReadService{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithLaunchReceiptService(store))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	cfg := testConfig()
	cfg.LaunchReceipts = launchreceipt.Client{BaseURL: server.URL, Minter: key}
	pods := &fakePodAPI{terminalPhase: corev1.PodSucceeded}
	d, err := New(cfg, pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := boundLaunchAttempt(testAttempt())
	if _, err := d.Dispatch(t.Context(), a, []RunnerSpec{linuxRunner()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, a.LaunchBinding.AttemptID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(t.Context(), a, []RunnerSpec{linuxRunner()}); err == nil || len(pods.created) != 1 {
		t.Fatal("replayed authority created another pod")
	}
	// A different attempt with valid fresh authority still must not launch
	// when the daemon can no longer persist its immutable receipt.
	a.Number++
	a = boundLaunchAttempt(a)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(t.Context(), a, []RunnerSpec{linuxRunner()}); err == nil || len(pods.created) != 1 {
		t.Fatal("receipt persistence failure created a pod")
	}
}
