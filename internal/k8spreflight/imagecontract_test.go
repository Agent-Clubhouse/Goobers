package k8spreflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOverlayImageContractOrchestratesRenderAndArtifactEvidence(t *testing.T) {
	sha := strings.Repeat("a", 40)
	image := "registry.invalid/goobers:" + sha
	for _, tc := range []struct {
		name, failure string
		want          Status
	}{
		{name: "complete", want: StatusPass},
		{name: "render fails", failure: "render", want: StatusFail},
		{name: "nothing inspected", failure: "empty", want: StatusFail},
		{name: "missing sidecar", failure: "sidecar", want: StatusFail},
		{name: "CA unspecified", failure: "ca", want: StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeOverlayFixture(t, root, "kustomization.yaml", "images:\n- name: goobers\n  newName: registry.invalid/goobers\n  newTag: "+sha+"\n")
			caPath := filepath.Join(root, "root.pem")
			if err := os.WriteFile(caPath, imageFixtureRoot(t), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.failure == "ca" {
				caPath = ""
			}
			rendered := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n      - name: trim\n        image: " + image + "\n        command: [/usr/local/bin/gocache-trim]\n"
			runs, removed := 0, 0
			run := func(ctx context.Context, cmd overlayCommand) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded command")
				}
				if cmd.Program == "kubectl" {
					if !slices.Equal(cmd.Args, []string{"kustomize", root}) {
						t.Fatalf("render arguments: %v", cmd.Args)
					}
					if tc.failure == "render" {
						return nil, errors.New("render fixture failed")
					}
					if tc.failure == "empty" {
						return []byte("kind: ConfigMap\n"), nil
					}
					return []byte(rendered), nil
				}
				if cmd.Program != "docker" {
					t.Fatalf("runtime: %s", cmd.Program)
				}
				switch cmd.Args[0] {
				case "pull":
					return nil, nil
				case "image":
					return []byte(`[{"Id":"sha256:` + strings.Repeat("b", 64) + `","Os":"linux"}]`), nil
				case "rm":
					removed++
					return nil, nil
				case "run":
					runs++
					if tc.failure == "sidecar" && slices.Contains(cmd.Args, "/usr/local/bin/gocache-trim") {
						return nil, errors.New("missing executable")
					}
					if slices.Contains(cmd.Args, "--version") {
						return []byte(`{"commit":"` + sha + `"}`), nil
					}
					return nil, nil
				default:
					t.Fatalf("unexpected operation: %v", cmd.Args)
					return nil, nil
				}
			}
			result := checkOverlayImageContract(context.Background(), nil, Options{OverlayDir: root, ImageCAFile: caPath, ImageTools: []string{"git"}, runOverlayCommand: run})
			if result.Status != tc.want {
				t.Fatalf("result=%+v, want %s", result, tc.want)
			}
			if runs != removed {
				t.Fatalf("created %d probes but cleaned %d", runs, removed)
			}
			if tc.want == StatusPass && !strings.Contains(result.Detail, "checked 4") {
				t.Fatalf("evidence missing: %+v", result)
			}
		})
	}
}

func TestOverlayChecksRegisteredAndExplicitlyUncheckedWithoutInput(t *testing.T) {
	report := Run(context.Background(), newFakeCluster(t), Options{})
	for _, id := range []string{"overlay-pin-agreement", "overlay-image-contract"} {
		result := resultByID(t, report, id)
		if result.Status != StatusWarn || !strings.Contains(result.Detail, "checked 0") {
			t.Fatalf("silent success: %+v", result)
		}
	}
}

func TestImageRootCAInputIsBoundedAndRegular(t *testing.T) {
	root := t.TempDir()
	if _, err := readImageRootCA(root); err == nil {
		t.Fatal("accepted a directory as CA")
	}
	path := filepath.Join(root, "oversized.pem")
	if err := os.WriteFile(path, make([]byte, (64<<10)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageRootCA(path); err == nil {
		t.Fatal("accepted oversized CA input")
	}
}
