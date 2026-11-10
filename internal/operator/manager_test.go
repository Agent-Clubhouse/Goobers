package operator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDefaultOptions(t *testing.T) {
	want := Options{MetricsAddr: ":8080", HealthAddr: ":8081"}
	if got := DefaultOptions(); got != want {
		t.Fatalf("DefaultOptions() = %+v, want %+v", got, want)
	}
}

func TestManagerReconcilerWorkerImage(t *testing.T) {
	for _, image := range []string{"", "registry.example/worker:v2"} {
		t.Run(image, func(t *testing.T) {
			r, c := newReconciler(t, nil, gaggleFixture(), gooberFixture("coder", "web", 1))
			opts := DefaultOptions()
			opts.WorkerImage = image
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			r = newGaggleReconciler(c, r.Scheme, logger, opts)
			reconcileOnce(t, r)

			var dep appsv1.Deployment
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gaggle-web", Name: "goober-coder"}, &dep); err != nil {
				t.Fatal(err)
			}
			want := image
			if want == "" {
				want = defaultWorkerImage
			}
			if got := dep.Spec.Template.Spec.Containers[0].Image; got != want {
				t.Fatalf("worker image = %q, want %q", got, want)
			}
		})
	}
}

func TestRunRejectsInvalidKubeconfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "invalid-kubeconfig")
	if err := os.WriteFile(configPath, []byte("[invalid yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", configPath)
	err := Run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultOptions())
	if err == nil || !strings.Contains(err.Error(), "load kube config:") {
		t.Fatalf("Run() error = %v, want kube config error", err)
	}
}
