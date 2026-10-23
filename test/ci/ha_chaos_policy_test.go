package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/test/ha/chaos"
	"github.com/goobers/goobers/test/ha/invariants"
)

type haNightlyStep struct {
	Name  string `yaml:"name"`
	Shell string `yaml:"shell"`
	Run   string `yaml:"run"`
}

func haNightlySteps(t *testing.T) map[string]haNightlyStep {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "ha-nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []haNightlyStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	steps := map[string]haNightlyStep{}
	for _, step := range workflow.Jobs["smoke"].Steps {
		steps[step.Name] = step
	}
	return steps
}

// TestHAChaosPendingScenariosAreCountedAndOpen is the CI policy for the HA
// chaos harness: every scenario and invariant either runs or carries an
// explicit "pending #N" reason, the count is surfaced, and N is recorded as
// open in the checked-in reference map. A merged or closed reference must be
// updated deliberately (and the nightly re-checks it against GitHub).
func TestHAChaosPendingScenariosAreCountedAndOpen(t *testing.T) {
	t.Parallel()
	refs, err := chaos.PendingRefs()
	if err != nil {
		t.Fatal(err)
	}
	cited := map[string]bool{}
	cite := func(kind, id, pending string) {
		t.Logf("pending %s %s: %s", kind, id, pending)
		n, ok := chaos.PendingNumber(pending)
		if !ok {
			t.Errorf("%s %s pending reason %q is not 'pending #N'", kind, id, pending)
			return
		}
		cited[n] = true
		if refs[n] != "open" {
			t.Errorf("%s %s cites #%s whose recorded state is %q, not open: give it a body or repoint it", kind, id, n, refs[n])
		}
	}
	pending := 0
	for _, s := range chaos.Scenarios() {
		if err := s.Validate(); err != nil {
			t.Error(err)
		}
		if s.Pending != "" {
			pending++
			cite("scenario", s.ID, s.Pending)
		}
	}
	t.Logf("HA chaos scenarios pending: %d of %d", pending, len(chaos.Scenarios()))
	pendingInvariants := 0
	for _, inv := range invariants.All() {
		if inv.Pending != "" {
			pendingInvariants++
			cite("invariant", inv.Name, inv.Pending)
		}
	}
	t.Logf("HA invariants pending: %d of %d", pendingInvariants, len(invariants.All()))
	for n := range refs {
		if !cited[n] {
			t.Errorf("pending_refs.json lists #%s but nothing cites it; remove it", n)
		}
	}
}

// TestHANightlyChaosStepsFailTheJob runs the nightly step scripts the way
// GitHub runs them, with the chaos binary replaced by a failing command, and
// requires a non-zero exit: a pipeline into tee must not mask the failure.
func TestHANightlyChaosStepsFailTheJob(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available on this runner; the nightly runs on ubuntu")
	}
	steps := haNightlySteps(t)
	for _, name := range []string{"Run chaos harness", "Verify chaos pending references are still open"} {
		step, ok := steps[name]
		if !ok {
			t.Fatalf("ha-nightly.yml has no step %q", name)
		}
		if !strings.Contains(step.Run, "go run ./test/ha/chaos/cmd/chaos ") {
			t.Fatalf("step %q does not invoke the chaos command", name)
		}
		// GitHub expands "shell: bash" to "bash --noprofile --norc -eo pipefail"; an
		// unset shell is "bash -e" without pipefail.
		flags := []string{"--noprofile", "--norc", "-e"}
		switch step.Shell {
		case "bash":
			flags = append(flags, "-o", "pipefail")
		case "":
		default:
			t.Fatalf("step %q uses unmodelled shell %q", name, step.Shell)
		}
		var script []string
		for _, line := range strings.Split(step.Run, "\n") {
			switch {
			case strings.Contains(line, "go run ./test/ha/chaos/cmd/chaos "):
				line = strings.Replace(line, "go run ./test/ha/chaos/cmd/chaos run", "sh -c 'exit 3'", 1)
				line = strings.Replace(line, "go run ./test/ha/chaos/cmd/chaos verify-pending", "sh -c 'exit 3'", 1)
			case strings.HasPrefix(strings.TrimSpace(line), "go test"):
				line = "true"
			}
			script = append(script, line)
		}
		file := filepath.Join(t.TempDir(), "step.sh")
		if err := os.WriteFile(file, []byte(strings.Join(script, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, append(flags, file)...)
		cmd.Env = append(os.Environ(), "GITHUB_STEP_SUMMARY="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("step %q exits 0 when the chaos command fails (output %q)", name, out)
		}
	}
}
