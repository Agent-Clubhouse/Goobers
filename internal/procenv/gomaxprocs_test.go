package procenv

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// helperEnvVar marks the re-executed test binary as the child half of
// TestDerivedGOMAXPROCSReachesTheChildProcessRuntime.
const helperEnvVar = "GOOBERS_PROCENV_GOMAXPROCS_HELPER_PROCESS"

// helperMarker prefixes the one line the child prints, so the assertion reads
// the child's answer rather than the test framework's own output around it.
const helperMarker = "GOMAXPROCS-IN-CHILD="

// stubCPUProcs points the derived-budget resolver at a fixed answer for the
// duration of one test. The cgroup-file half of the chain — a real cpu.max
// parsed into this integer — is asserted in internal/platform/cpustat; this
// package owns the half that turns it into an environment.
func stubCPUProcs(t *testing.T, procs int, ok bool) {
	t.Helper()
	previous := cpuProcs
	cpuProcs = func() (int, bool) { return procs, ok }
	t.Cleanup(func() { cpuProcs = previous })
}

// unsetGOMAXPROCS makes the variable genuinely absent from the test process,
// restoring whatever the environment had afterwards. t.Setenv registers the
// restore; the unset that follows is what the "operator has not set it" branch
// actually needs, and an empty value would not be the same thing.
func unsetGOMAXPROCS(t *testing.T) {
	t.Helper()
	t.Setenv(GOMAXPROCS, "restored-by-cleanup")
	if err := os.Unsetenv(GOMAXPROCS); err != nil {
		t.Fatal(err)
	}
}

func valuesOf(env []string, name string) []string {
	var values []string
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok && key == name {
			values = append(values, value)
		}
	}
	return values
}

// The defect (#3963): a stage subprocess in a 3 CPU pod sized itself for the
// node's 4 CPUs because nothing told it otherwise. The daemon knows the quota,
// so the environment it composes has to state it.
func TestBaseEnvDerivesGOMAXPROCSFromTheContainerCPUQuota(t *testing.T) {
	unsetGOMAXPROCS(t)
	stubCPUProcs(t, 3, true)

	for name, env := range map[string][]string{
		"BaseEnv":     BaseEnv(),
		"BaseEnvWith": BaseEnvWith([]string{"OPERATOR_DECLARED_VAR"}),
	} {
		got := valuesOf(env, GOMAXPROCS)
		if len(got) != 1 || got[0] != "3" {
			t.Fatalf("%s() carried %s=%v, want exactly one value \"3\"", name, GOMAXPROCS, got)
		}
	}
}

// An operator who sets GOMAXPROCS on the daemon has stated an intent the
// container cannot infer — a pod deliberately under-subscribed to leave room
// for a sidecar, say. The derived value is a default, never an override.
func TestBaseEnvHonorsAnOperatorSetGOMAXPROCS(t *testing.T) {
	t.Setenv(GOMAXPROCS, "6")
	stubCPUProcs(t, 3, true)

	got := valuesOf(BaseEnv(), GOMAXPROCS)
	if len(got) != 1 || got[0] != "6" {
		t.Fatalf("BaseEnv() carried %s=%v, want exactly one value \"6\" — the operator's, not the derived one", GOMAXPROCS, got)
	}
}

// A GOMAXPROCS set to whitespace states nothing. The Go runtime ignores it, so
// treating it as an operator override would silently disable the derivation on
// exactly the environments most likely to have been mis-templated.
func TestBaseEnvDerivesOverAnEmptyGOMAXPROCS(t *testing.T) {
	t.Setenv(GOMAXPROCS, "  ")
	stubCPUProcs(t, 3, true)

	got := valuesOf(BaseEnv(), GOMAXPROCS)
	if len(got) == 0 || got[len(got)-1] != "3" {
		t.Fatalf("BaseEnv() carried %s=%v, want the derived \"3\" to win over an empty value", GOMAXPROCS, got)
	}
}

// A developer machine, a CI host, and any pod whose quota is not narrower than
// its node must be left exactly as they were. Setting GOMAXPROCS permanently
// disables the Go runtime's automatic re-detection of the limit, so pinning it
// where nothing constrains the host would remove a capability for nothing.
func TestBaseEnvDerivesNothingWhereTheCgroupDoesNotConstrain(t *testing.T) {
	unsetGOMAXPROCS(t)
	stubCPUProcs(t, 0, false)

	if got := valuesOf(BaseEnv(), GOMAXPROCS); len(got) != 0 {
		t.Fatalf("BaseEnv() carried %s=%v on an unconstrained host, want it absent", GOMAXPROCS, got)
	}
}

// The end of the chain, measured rather than inferred: a real child process
// launched with this environment reports the derived width as its own
// runtime.GOMAXPROCS. That is the number `go build -p` and `go test -p` take
// their default process fan-out from, so this is the property the fix is for.
func TestDerivedGOMAXPROCSReachesTheChildProcessRuntime(t *testing.T) {
	unsetGOMAXPROCS(t)
	// Deliberately not runtime.NumCPU(): a value the host could have produced
	// on its own would not prove the environment is what carried it.
	const want = 3
	stubCPUProcs(t, want, true)

	cmd := exec.Command(os.Args[0], "-test.run=^TestGOMAXPROCSHelperProcess$")
	cmd.Env = append(BaseEnv(), helperEnvVar+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, output)
	}

	var reported string
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), helperMarker); ok {
			reported = value
		}
	}
	if reported == "" {
		t.Fatalf("helper process printed no %s line:\n%s", helperMarker, output)
	}
	if reported != strconv.Itoa(want) {
		t.Fatalf("child runtime.GOMAXPROCS(0) = %s, want %d (host has %d CPUs)", reported, want, runtime.NumCPU())
	}
}

// TestGOMAXPROCSHelperProcess is the child half of the test above: it is inert
// under an ordinary `go test` run and prints its own scheduler width when
// re-executed by it.
func TestGOMAXPROCSHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvVar) != "1" {
		t.Skip("child half of TestDerivedGOMAXPROCSReachesTheChildProcessRuntime")
	}
	if _, err := os.Stdout.WriteString(helperMarker + strconv.Itoa(runtime.GOMAXPROCS(0)) + "\n"); err != nil {
		t.Fatal(err)
	}
}
