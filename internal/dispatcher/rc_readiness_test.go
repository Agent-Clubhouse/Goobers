package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/instance"
)

type cancellationPodAPI struct {
	*fakePodAPI
	cancel          context.CancelFunc
	cleanupDeadline time.Time
	cleanupErr      error
}

func (p *cancellationPodAPI) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	p.cancel()
	return nil, ctx.Err()
}

func (p *cancellationPodAPI) DeletePod(ctx context.Context, namespace, name string) error {
	p.cleanupDeadline, _ = ctx.Deadline()
	p.cleanupErr = ctx.Err()
	if p.cleanupErr != nil {
		return p.cleanupErr
	}
	return p.fakePodAPI.DeletePod(ctx, namespace, name)
}

func TestDispatchCancellationStillDeletesStageWithBoundedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pods := &cancellationPodAPI{fakePodAPI: &fakePodAPI{}, cancel: cancel}
	d, err := New(testConfig(), pods, nil, confirmGate{confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := d.Dispatch(ctx, testAttempt(), []RunnerSpec{linuxRunner()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if !report.Disposed || report.DisposeErr != nil || len(pods.deleted) != 1 {
		t.Fatalf("canceled attempt kept executing: report=%+v deleted=%v", report, pods.deleted)
	}
	remaining := time.Until(pods.cleanupDeadline)
	if pods.cleanupErr != nil || remaining <= 0 || remaining > DefaultDisposalTimeout {
		t.Fatalf("cleanup context is canceled or unbounded: err=%v remaining=%s", pods.cleanupErr, remaining)
	}
}

type sidecarPodAPI struct {
	*fakePodAPI
	exitCode    int32
	stageExited bool
}

func (p *sidecarPodAPI) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod, err := p.fakePodAPI.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}, {
		Name: pod.Spec.Containers[0].Name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if p.stageExited {
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: p.exitCode}}
	}
	return pod, nil
}

func TestDispatchTemplateSettlesStageWithResidentSidecar(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stageExited bool
		exitCode    int32
		confirmed   bool
		want        error
	}{
		{"success", true, 0, true, nil},
		{"stage failure", true, 2, true, ErrStageFailed},
		{"surrender still required", true, 0, false, ErrSurrenderUnconfirmed},
		{"sidecar exit does not settle stage", false, 0, true, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := testDeployment()
			template.Spec.Template.Spec.Containers[0].Name = "consumer-task"
			template.Spec.Template.Spec.Containers = append(template.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar", Image: "sidecar:1"})
			pods := &sidecarPodAPI{fakePodAPI: &fakePodAPI{}, exitCode: tc.exitCode, stageExited: tc.stageExited}
			pods.deployments = map[string]*appsv1.Deployment{template.Name: template}
			runner := linuxRunner()
			runner.HostKind = instance.RunnerHostDeployment
			runner.Host = template.Name
			d, err := New(testConfig(), pods, nil, confirmGate{confirmed: tc.confirmed}, nil)
			if err != nil {
				t.Fatal(err)
			}
			// A completed stage must settle on the first read, before polling again.
			d.sleep = func(context.Context, time.Duration) error { return context.Canceled }
			report, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{runner})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Dispatch error=%v, want %v", err, tc.want)
			}
			if !report.Disposed || len(pods.deleted) != 1 {
				t.Fatalf("pod was not disposed: %+v", report)
			}
			if tc.want == nil && !report.SurrenderConfirmed {
				t.Fatal("success without surrender")
			}
		})
	}
}

// expandPodEnv models the documented kubelet single-pass expansion grammar:
// $$ escapes one dollar, $(NAME) reads only earlier variables, and unknown
// references survive literally. Replacement values are never recursively expanded.
func expandPodEnv(value string, env map[string]string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '$' && i+1 < len(value) {
			if value[i+1] == '$' {
				out.WriteByte('$')
				i++
				continue
			}
			if value[i+1] == '(' {
				if end := strings.IndexByte(value[i+2:], ')'); end >= 0 {
					name := value[i+2 : i+2+end]
					if resolved, ok := env[name]; ok {
						out.WriteString(resolved)
					} else {
						out.WriteString("$(" + name + ")")
					}
					i += end + 2
					continue
				}
			}
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

func TestStageDataDoesNotExpandControlBearersBeforeEnvironmentStrip(t *testing.T) {
	payload := "$(GOOBERS_POD_TOKEN) $(GOOBERS_CLAIMS_TOKEN) $(PATH) $$ $HOME $(date)"
	for _, templatePath := range []bool{false, true} {
		for _, script := range []bool{false, true} {
			t.Run(fmt.Sprintf("template=%t/script=%t", templatePath, script), func(t *testing.T) {
				attempt := testAttempt()
				attempt.PodToken = "pod-secret"
				attempt.WorkspaceBranch = "goobers/$(GOOBERS_POD_TOKEN)"
				attempt.Inputs = map[string]string{"payload": payload}
				attempt.RunContext = map[string]string{"GOOBERS_REPO_NAME": payload}
				if script {
					attempt.Script = "printf '%s' '" + payload + "'"
				} else {
					attempt.Command = []string{"echo", payload}
				}
				var pod *corev1.Pod
				var err error
				if templatePath {
					pod, err = RenderFromTemplate(testConfig(), attempt, linuxRunner(), testDeployment())
				} else {
					pod, err = RenderPod(testConfig(), attempt, linuxRunner())
				}
				if err != nil {
					t.Fatal(err)
				}
				env := map[string]string{"PATH": "/image/bin", "GOOBERS_CLAIMS_TOKEN": "claims-secret"}
				for _, e := range pod.Spec.Containers[0].Env {
					env[e.Name] = expandPodEnv(e.Value, env)
				}
				for _, key := range []string{InputEnvVar("payload"), "GOOBERS_REPO_NAME"} {
					if env[key] != payload {
						t.Errorf("%s expanded data into %q, want literal %q", key, env[key], payload)
					}
				}
				if env[EnvWorkspaceBranch] != attempt.WorkspaceBranch {
					t.Errorf("provider branch expanded: %q, want %q", env[EnvWorkspaceBranch], attempt.WorkspaceBranch)
				}
				if script {
					if env[EnvStageScript] != attempt.Script {
						t.Errorf("script changed: %q", env[EnvStageScript])
					}
				} else {
					var got []string
					if err := json.Unmarshal([]byte(env[EnvStageCommand]), &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, attempt.Command) {
						t.Errorf("command changed: %q", got)
					}
				}
			})
		}
	}
}
