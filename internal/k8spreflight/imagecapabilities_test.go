package k8spreflight

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRenderedImageCapabilitiesIncludeSourceGateAndConfiguredScript(t *testing.T) {
	sha := strings.Repeat("a", 40)
	data := []byte(`kind: Deployment
spec:
  template:
    spec:
      initContainers:
      - name: initialize
        image: registry.invalid/goobers:` + sha + `
        command: [git]
      containers:
      - name: trim
        image: registry.invalid/goobers:` + sha + `
        command: [/usr/local/bin/gocache-trim]
        env:
        - name: GOCACHE_ORPHAN_ROOT
          value: /workspace
        - name: GOCACHE_ORPHAN_AGE_MIN
          value: "30"
      - name: api
        image: registry.invalid/goobers:` + sha + `
        env:
        - name: GOOBERS_MEMORY_HIGH_WATER
          value: "85%"
`)
	requirements, err := renderedImageRequirements(data, []overlayPin{{Commit: sha}}, nil, nil)
	if err != nil || len(requirements) != 1 {
		t.Fatalf("requirements=%+v err=%v", requirements, err)
	}
	r := requirements[0]
	if r.MinimumCommit != memoryGateMinimumCommit || !slices.Equal(r.Tools, []string{"git"}) || !slices.Equal(r.Executables, []string{"/usr/local/bin/gocache-trim"}) || !slices.Equal(r.ScriptVariables, []string{"GOCACHE_ORPHAN_AGE_MIN", "GOCACHE_ORPHAN_ROOT"}) {
		t.Fatalf("rendered requirements lost: %+v", r)
	}
	for _, malformed := range []string{"command: git", "command: [123]", "env: invalid"} {
		bad := []byte("kind: Pod\nspec:\n  containers:\n  - image: goobers:" + sha + "\n    " + malformed + "\n")
		if _, err := renderedImageRequirements(bad, []overlayPin{{Commit: sha}}, nil, nil); err == nil {
			t.Fatalf("accepted malformed %s", malformed)
		}
	}
}

func TestImageSourceAncestryNeverPassesUnknownOrOlderPins(t *testing.T) {
	for _, status := range []string{"ahead", "identical", "behind", "diverged", "", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			run := func(_ context.Context, cmd overlayCommand) ([]byte, error) {
				if cmd.Program != "gh" || !strings.Contains(cmd.Args[1], memoryGateMinimumCommit+"...") {
					t.Fatalf("wrong ancestry query: %+v", cmd)
				}
				if status == "unavailable" {
					return nil, errors.New("API unavailable")
				}
				return []byte(`{"status":"` + status + `"}`), nil
			}
			err := probeImageSourceRequirement(context.Background(), run, imageRequirements{Commit: strings.Repeat("a", 40), MinimumCommit: memoryGateMinimumCommit})
			if (err == nil) != (status == "ahead" || status == "identical") {
				t.Fatalf("status=%q err=%v", status, err)
			}
		})
	}
}

func TestImageSourceAncestryPinsAuthoritativeHost(t *testing.T) {
	t.Setenv("GH_HOST", "unrelated.example")
	t.Setenv("GH_REPO", "unrelated/example")
	commit := strings.Repeat("a", 40)
	run := func(_ context.Context, cmd overlayCommand) ([]byte, error) {
		want := []string{"api", "repos/Agent-Clubhouse/Goobers/compare/" + memoryGateMinimumCommit + "..." + commit, "--hostname", "github.com", "--jq", "{status: .status}"}
		if cmd.Program != "gh" || !slices.Equal(cmd.Args, want) {
			t.Fatalf("source authority can be redirected by ambient gh settings: %+v", cmd)
		}
		return []byte(`{"status":"ahead"}`), nil
	}
	if err := probeImageSourceRequirement(context.Background(), run, imageRequirements{Commit: commit, MinimumCommit: memoryGateMinimumCommit}); err != nil {
		t.Fatal(err)
	}
}

func TestImageScriptCapabilityActuallyChecksEachConfiguredVariable(t *testing.T) {
	var checked []string
	invoke := func(program string, args []string, _ []byte) ([]byte, error) {
		if program != "/bin/sh" || !strings.Contains(args[1], "/usr/local/bin/gocache-trim") {
			t.Fatalf("wrong script probe: %s %v", program, args)
		}
		checked = append(checked, args[len(args)-1])
		if checked[len(checked)-1] == "GOCACHE_ORPHAN_AGE_MIN" {
			return nil, errors.New("variable absent")
		}
		return nil, nil
	}
	variables := []string{"GOCACHE_ORPHAN_ROOT", "GOCACHE_ORPHAN_AGE_MIN"}
	if err := probeImageScriptVariables(invoke, "linux", variables); err == nil || !slices.Equal(checked, variables) {
		t.Fatalf("err=%v checked=%v", err, checked)
	}
}

func TestRunnerHostImageIsInspectedWithoutAStaticPod(t *testing.T) {
	sha := strings.Repeat("a", 40)
	image := "registry.invalid/goobers:" + sha
	got, err := renderedImageRequirements([]byte("kind: ConfigMap\n"), []overlayPin{{Kind: "host", Image: image, Commit: sha}}, nil, nil)
	if err != nil || len(got) != 1 || got[0].Image != image {
		t.Fatalf("host image missing: %+v %v", got, err)
	}
}
