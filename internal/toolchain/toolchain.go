// Package toolchain verifies, at stage-start on the executing host, that the
// runner-capability tokens a run declares (RRQ-1/#1101 — e.g. `dotnet@8`,
// `node@20`, `go@1.26`, `os=windows`) are actually satisfied by an installed
// toolchain, and fails closed with a clear diagnostic when they are not (#735,
// docs/design/v1/polyglot-stacks.md §4 P-3).
//
// It is the runtime counterpart to the scheduler's schedule-time set-match:
// #1101 refuses to place a run on a runner that does not *claim* a required
// capability, but explicitly accepts that a runner which *falsely* claims one
// (advertises `dotnet@8` while the SDK is absent or a different version)
// "degrades to a runtime error the scheduler does not prevent." That opaque
// mid-run error is the "host-PATH gambling" #735 names. This package turns it
// into an actionable preflight failure: before any stage runs, it probes the
// declared toolchain through the same base environment the stage will use
// (procenv.BaseEnv, so #736's DOTNET_ROOT/NUGET_PACKAGES/… resolve identically)
// and reports exactly which capability the host does not satisfy and why.
//
// It does NOT install anything — the PO-confirmed model assumes a preinstalled
// toolchain (design §8). Verification only.
//
// A capability token whose family has no registered probe (e.g. `xcode`,
// `netfx@4.8`) is not verified here — the schedule-time match already gated its
// membership, and this package only claims to verify toolchains it knows how to
// probe. New families are added by registering a probe, version-generically
// (the reference version is incidental; swapping `dotnet@8`→`dotnet@10` is a
// config change, never a code change — design §2).
package toolchain

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/procenv"
)

// probeTimeout bounds a single toolchain probe. A version query
// (`dotnet --version`) returns near-instantly; a multi-second ceiling only
// guards against a wedged binary, never normal operation.
const probeTimeout = 15 * time.Second

// ExecFunc runs a version-probe command and returns its combined output. It is
// the seam tests inject to avoid executing real toolchains; the default
// (execWithBaseEnv) runs the binary through procenv.BaseEnv with a timeout.
type ExecFunc func(ctx context.Context, name string, args ...string) (string, error)

// Prober verifies one toolchain family against a version constraint on the host.
// constraint is the token's version part (`8.0` in `dotnet@8.0`, `windows` in
// `os=windows`), or "" when the token names a bare capability (`xcode`).
type Prober interface {
	Verify(ctx context.Context, run ExecFunc, constraint string) error
}

// Verifier holds the registered probes and the exec seam. The zero value is not
// usable; construct with DefaultVerifier.
type Verifier struct {
	probes map[string]Prober
	run    ExecFunc
	goos   string
}

// DefaultVerifier returns the production Verifier: the built-in probe set, real
// host execution through procenv.BaseEnv, and the compiled-in GOOS.
func DefaultVerifier() *Verifier {
	v := &Verifier{run: execWithBaseEnv, goos: runtime.GOOS}
	v.probes = map[string]Prober{
		"dotnet": commandProber{bin: "dotnet", args: []string{"--version"}, parse: parseFirstLine},
		"node":   commandProber{bin: "node", args: []string{"--version"}, parse: parseNodeVersion},
		"python": commandProber{bin: pythonProbeBin(v.goos), args: []string{"--version"}, parse: parseLastField},
		"go":     commandProber{bin: "go", args: []string{"version"}, parse: parseGoVersion},
		"os":     osProber{goos: func() string { return v.goos }},
	}
	return v
}

func pythonProbeBin(goos string) string {
	if goos == "windows" {
		return "python"
	}
	return "python3"
}

// Verify checks every declared capability that names a probeable toolchain and
// returns a single aggregated error naming each unsatisfied one, or nil when all
// are satisfied. An empty required set is trivially satisfied, so a run that
// declares no requirement is never touched (no behavior change).
func (v *Verifier) Verify(ctx context.Context, required []string) error {
	var problems []string
	seen := make(map[string]struct{}, len(required))
	for _, token := range required {
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}

		family, constraint := splitToken(token)
		prober, ok := v.probes[family]
		if !ok {
			// No probe for this family (e.g. `xcode`, `netfx@4.8`): the
			// schedule-time match already gated its membership; we only verify
			// toolchains we know how to probe.
			continue
		}
		if err := prober.Verify(ctx, v.run, constraint); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", token, err))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("runtime preflight failed — the runner claims %d toolchain capabilit%s its host does not satisfy:\n  - %s",
		len(problems), plural(len(problems)), strings.Join(problems, "\n  - "))
}

// splitToken splits a capability token into its family and version/value
// constraint. It splits on the first `@` (`dotnet@8.0`) or `=` (`os=windows`);
// a bare token (`xcode`) has an empty constraint.
func splitToken(token string) (family, constraint string) {
	if i := strings.IndexAny(token, "@="); i >= 0 {
		return token[:i], token[i+1:]
	}
	return token, ""
}

// commandProber verifies a toolchain by running a version command and comparing
// the parsed version against the constraint.
type commandProber struct {
	bin   string
	args  []string
	parse func(output string) (string, error)
}

func (p commandProber) Verify(ctx context.Context, run ExecFunc, constraint string) error {
	out, err := run(ctx, p.bin, p.args...)
	if err != nil {
		return fmt.Errorf("%q is not runnable on the host PATH: %w", p.bin, err)
	}
	got, perr := p.parse(out)
	if perr != nil {
		return fmt.Errorf("could not read %q version: %w", p.bin, perr)
	}
	if constraint != "" && !versionSatisfies(constraint, got) {
		return fmt.Errorf("host has version %s but the run requires %s", got, constraint)
	}
	return nil
}

// osProber verifies an `os=<goos>` capability against the host OS without
// executing anything.
type osProber struct {
	goos func() string
}

func (p osProber) Verify(_ context.Context, _ ExecFunc, constraint string) error {
	if constraint == "" {
		return fmt.Errorf("os capability needs a value, e.g. os=linux")
	}
	if host := p.goos(); host != constraint {
		return fmt.Errorf("host OS is %s but the run requires %s", host, constraint)
	}
	return nil
}

// versionSatisfies reports whether the found version satisfies the requested
// constraint by dotted-component prefix: every component the constraint names
// must equal the corresponding component of the found version. So `8` is
// satisfied by `8.0.412`, `8.0` by `8.0.412`, but `8.1` is not satisfied by
// `8.0.412`. This is version-generic — it encodes no specific version and needs
// no code change to accept a new one (design §8).
func versionSatisfies(want, got string) bool {
	wantParts := strings.Split(want, ".")
	gotParts := strings.Split(got, ".")
	if len(wantParts) > len(gotParts) {
		return false
	}
	for i, w := range wantParts {
		if w != gotParts[i] {
			return false
		}
	}
	return true
}

// parseFirstLine returns the first non-empty trimmed line — the shape of
// `dotnet --version` ("8.0.412").
func parseFirstLine(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("no output")
}

// parseNodeVersion strips node's leading "v" ("v20.11.1" -> "20.11.1").
func parseNodeVersion(output string) (string, error) {
	s, err := parseFirstLine(output)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(s, "v"), nil
}

// parseLastField returns the last whitespace-separated field of the first line —
// the shape of `python3 --version` ("Python 3.11.4" -> "3.11.4").
func parseLastField(output string) (string, error) {
	s, err := parseFirstLine(output)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(s)
	return fields[len(fields)-1], nil
}

// parseGoVersion pulls the dotted version out of `go version`
// ("go version go1.26.0 darwin/arm64" -> "1.26.0").
func parseGoVersion(output string) (string, error) {
	for _, f := range strings.Fields(output) {
		if v, ok := strings.CutPrefix(f, "go1."); ok {
			return "1." + v, nil
		}
	}
	return "", fmt.Errorf("no go1.x token in %q", strings.TrimSpace(output))
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// execWithBaseEnv runs name+args with the stage base environment
// (procenv.BaseEnv, so the probe resolves the toolchain exactly as a stage
// would) and a timeout, returning combined stdout+stderr — some tools print
// their version to stderr.
func execWithBaseEnv(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = procenv.BaseEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
