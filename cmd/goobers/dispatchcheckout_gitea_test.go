package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/testgit"
)

type checkoutDispatchFunc func(context.Context, dispatcher.Attempt, []dispatcher.RunnerSpec) (dispatcher.Report, error)

func (f checkoutDispatchFunc) Dispatch(ctx context.Context, a dispatcher.Attempt, r []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	return f(ctx, a, r)
}

// Exercise the actual activity projection and pod checkout against a local git
// HTTP origin. No checkoutCloneURL override or git URL rewrite is involved.
func TestGiteaOriginDispatchCheckout(t *testing.T) {
	bare := newBareRepoWithCommit(t, "main")
	cmd := testgit.Command("-C", bare, "-c", "safe.bareRepository=all", "update-server-info")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare HTTP origin: %v: %s", err, out)
	}
	server := httptest.NewServer(http.StripPrefix("/forge/acme/", http.FileServer(http.Dir(filepath.Dir(bare)))))
	t.Cleanup(server.Close)
	baseURL := server.URL + "/forge/"
	for _, template := range []bool{false, true} {
		name := "image"
		if template {
			name = "deployment"
		}
		t.Run(name, func(t *testing.T) {
			for _, mode := range []apiv1.WorkspaceMode{apiv1.WorkspaceRepo, apiv1.WorkspaceRepoReadOnly} {
				t.Run(string(mode), func(t *testing.T) {
					store, err := dispatcher.NewSurrenderDir(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					var pod *corev1.Pod
					d := checkoutDispatchFunc(func(ctx context.Context, a dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
						cfg := dispatcher.Config{Namespace: "local-test"}
						r := dispatcher.RunnerSpec{Name: "local", OS: "linux", HostKind: instance.RunnerHostImage, Host: "local:fixture", Restrictions: []string{"env:default-deny"}}
						var renderErr error
						if template {
							deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "stage", Image: "local:fixture", Env: []corev1.EnvVar{{Name: "GOOBERS_REPO_BASE_URL", Value: "http://wrong.invalid"}}, EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "ambient"}}}}}}}}}}
							pod, renderErr = dispatcher.RenderFromTemplate(cfg, a, r, deployment)
						} else {
							pod, renderErr = dispatcher.RenderPod(cfg, a, r)
						}
						if renderErr != nil {
							return dispatcher.Report{}, renderErr
						}
						data, marshalErr := json.Marshal(dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
						if marshalErr != nil {
							return dispatcher.Report{}, marshalErr
						}
						if putErr := store.Put(ctx, a.RunID, a.Stage, a.IdentityAttempt(), data); putErr != nil {
							return dispatcher.Report{}, putErr
						}
						return dispatcher.Report{Runner: r.Name, Phase: corev1.PodSucceeded, SurrenderConfirmed: true}, nil
					})
					a := &engine.Activities{Dispatcher: d, Surrenders: store}
					_, err = a.DispatchStage(context.Background(), engine.DispatchStageInput{
						Envelope: apiv1.InvocationEnvelope{RunID: "gitea-run", TaskID: "checkout", Gaggle: "local", WorkflowID: "continuity", Attempt: 1, BaseBranch: "main", BranchNamespace: "probe/", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitea, BaseURL: baseURL, Owner: "acme", Name: "origin"}},
						Run:      &apiv1.DeterministicRun{Command: []string{"make", "ci"}, Workspace: mode},
					})
					if err != nil {
						t.Fatalf("dispatch: %v", err)
					}
					for _, key := range dispatcher.DispatcherControlEnv {
						t.Setenv(key, "")
					}
					t.Setenv("GOOBERS_REPO_BASE_URL", "")
					for _, e := range pod.Spec.Containers[0].Env {
						t.Setenv(e.Name, e.Value)
					}
					ws := t.TempDir()
					var stderr strings.Builder
					if err := checkoutRepoWorkspace(context.Background(), ws, &stderr, nil); err != nil {
						t.Fatalf("checkout: %v\n%s", err, stderr.String())
					}
					if got := os.Getenv("GOOBERS_REPO_BASE_URL"); got != baseURL {
						t.Fatalf("declared base URL changed: %q, want %q", got, baseURL)
					}
					data, err := os.ReadFile(filepath.Join(ws, "README.md"))
					if err != nil || string(data) != "probe\n" {
						t.Fatalf("checkout content = %q, %v", data, err)
					}
					remote, err := testgit.Command("-C", ws, "remote", "get-url", "origin").Output()
					if err != nil {
						t.Fatal(err)
					}
					if want := server.URL + "/forge/acme/origin.git"; strings.TrimSpace(string(remote)) != want {
						t.Fatalf("origin = %q, want %q", remote, want)
					}
					for _, kv := range stageEnvironment() {
						if strings.HasPrefix(kv, "GOOBERS_REPO_BASE_URL=") {
							t.Fatal("checkout identity leaked into non-CLI stage")
						}
					}
				})
			}
		})
	}
}
