package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	stageexecutor "github.com/goobers/goobers/internal/executor"
)

const wslPreflightTimeout = 20 * time.Second

const wslPreflightProbe = `set -u
distro="${WSL_DISTRO_NAME:-default}"
kernel="$(uname -r 2>/dev/null)" || {
	printf '%s\n' 'GOOBERS_WSL_ERROR=linux'
	exit 20
}
printf 'GOOBERS_WSL_DISTRO=%s\n' "$distro"
printf 'GOOBERS_WSL_KERNEL=%s\n' "$kernel"
goobers_path="$(command -v goobers 2>/dev/null)" || {
	printf '%s\n' 'GOOBERS_WSL_ERROR=goobers'
	exit 21
}
printf 'GOOBERS_WSL_GOOBERS=%s\n' "$goobers_path"
if ! network_detail="$("$goobers_path" __wsl-network-preflight 2>&1)"; then
	printf '%s\n' 'GOOBERS_WSL_ERROR=namespaces'
	printf '%s\n' "$network_detail"
	exit 22
fi
bwrap_path="$(command -v bwrap 2>/dev/null)" || {
	printf '%s\n' 'GOOBERS_WSL_ERROR=bwrap'
	exit 23
}
printf 'GOOBERS_WSL_BWRAP=%s\n' "$bwrap_path"
if ! sandbox_detail="$("$bwrap_path" --die-with-parent --unshare-pid --ro-bind / / --dev /dev --proc /proc -- /bin/true 2>&1)"; then
	printf '%s\n' 'GOOBERS_WSL_ERROR=bwrap-runtime'
	printf '%s\n' "$sandbox_detail"
	exit 24
fi
printf '%s\n' 'GOOBERS_WSL_READY=1'
`

const wslNetworkPreflightCommand = "__wsl-network-preflight"
const wslLaunchScript = `exec "$@"`

const wslPreflightHelp = `Usage: goobers preflight [--distro <name>] [--launch-wsl -- <goobers-command> [args...]]

On Windows, verify that the selected or default WSL distro can run the full
isolated Goobers workflow. Readiness requires WSL 2, a runnable distro, a Linux
goobers binary, Bubblewrap, and working unprivileged user + network namespaces.

On Linux, report whether this host can create the unprivileged user + network
namespaces network:none isolation relies on (#4267); takes no flags there.

With --launch-wsl, run the trailing Goobers command through that distro after
the checks pass. Arguments are forwarded directly without shell evaluation,
the WSL process starts in the current Windows directory, and its exit code is
returned. Use paths relative to that directory or Linux paths in forwarded
arguments.

Exit codes: 0 = ready/forwarded command succeeded, 1 = not ready or forwarded
command failed, 2 = usage error. A forwarded command's non-zero exit code is
preserved.
`

type wslReadiness struct {
	distro     string
	kernel     string
	goobers    string
	bubblewrap string
	failure    string
}

type wslDistribution struct {
	name    string
	version string
}

type wslPreflightDeps struct {
	hostOS           string
	lookPath         func(string) (string, error)
	probe            func(context.Context, string, []string) ([]byte, error)
	launch           func(string, []string, io.Reader, io.Writer, io.Writer) (int, error)
	getwd            func() (string, error)
	stdin            io.Reader
	probeNetworkNone func(context.Context) error
}

func realWSLPreflightDeps() wslPreflightDeps {
	return wslPreflightDeps{
		hostOS:   runtime.GOOS,
		lookPath: exec.LookPath,
		probe: func(ctx context.Context, executable string, args []string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, executable, args...)
			cmd.Env = append(os.Environ(), "WSL_UTF8=1")
			return cmd.CombinedOutput()
		},
		launch: func(executable string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			cmd := exec.Command(executable, args...)
			cmd.Env = append(os.Environ(), "WSL_UTF8=1")
			cmd.Stdin = stdin
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					if code := exitErr.ExitCode(); code >= 0 {
						return code, nil
					}
					return 1, nil
				}
				return 0, err
			}
			return 0, nil
		},
		getwd:            os.Getwd,
		stdin:            os.Stdin,
		probeNetworkNone: stageexecutor.ProbeNoNetwork,
	}
}

func runOnboardingPreflight(args []string, stdout, stderr io.Writer) int {
	return runOnboardingPreflightWith(args, stdout, stderr, realWSLPreflightDeps())
}

func runWSLNetworkPreflight(args []string, _ io.Writer, stderr io.Writer) int {
	if len(args) != 0 {
		pf(stderr, "error: %s takes no arguments\n", wslNetworkPreflightCommand)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), wslPreflightTimeout)
	defer cancel()
	if err := stageexecutor.ProbeNoNetwork(ctx); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// runLinuxNetworkPreflight reports whether this Linux host can create the
// unprivileged user + network namespaces network:none isolation relies on
// (internal/executor/network_linux.go), instead of unconditionally exiting 2
// as if `goobers preflight` simply didn't exist on Linux (#4267). Linux has
// no WSL/distro/bwrap chain to check — just this one capability — so the
// Windows-only flags are rejected here.
func runLinuxNetworkPreflight(distro string, launch bool, forwarded []string, probe func(context.Context) error, stdout, stderr io.Writer) int {
	if distro != "" || launch || len(forwarded) != 0 {
		pf(stderr, "error: --distro and --launch-wsl are Windows-only; `goobers preflight` on Linux takes no flags\n")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), wslPreflightTimeout)
	defer cancel()
	err := probe(ctx)
	if err == nil {
		pln(stdout, "OK: this host supports Goobers' enforced network:none isolation (unprivileged user + network namespaces)")
		return 0
	}
	if errors.Is(err, syscall.EPERM) {
		pf(stderr, "error: unprivileged user namespaces (CLONE_NEWUSER|CLONE_NEWNET) are restricted on "+
			"this host, so network:none isolation cannot be created; enable them (e.g. "+
			"`sysctl -w kernel.unprivileged_userns_clone=1`, or the container/security-profile equivalent), "+
			"or set GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1 to run without enforced isolation\n")
		return 1
	}
	pf(stderr, "error: network isolation capability probe failed: %v\n", err)
	return 1
}

func runOnboardingPreflightWith(args []string, stdout, stderr io.Writer, deps wslPreflightDeps) int {
	fs := newCLIFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "preflight")
	distro := fs.String("distro", "", "WSL distro to check (default: the configured default distro)")
	launch := fs.Bool("launch-wsl", false, "run the trailing Goobers command inside WSL after a successful check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if deps.hostOS == "linux" {
		return runLinuxNetworkPreflight(*distro, *launch, fs.Args(), deps.probeNetworkNone, stdout, stderr)
	}
	if deps.hostOS != "windows" {
		pf(stderr, "error: WSL preflight is only available on Windows hosts\n")
		return 2
	}
	forwarded := fs.Args()
	if (*launch && len(forwarded) == 0) || (!*launch && len(forwarded) != 0) {
		fs.Usage()
		return 2
	}

	wslPath, err := deps.lookPath("wsl.exe")
	if err != nil {
		pf(stderr, "error: WSL is not installed or wsl.exe is not on PATH; install WSL 2 and a distro with `wsl.exe --install`\n")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), wslPreflightTimeout)
	listOutput, listErr := deps.probe(ctx, wslPath, []string{"--list", "--verbose"})
	contextErr := ctx.Err()
	cancel()
	listOutput = normalizeWSLOutput(listOutput)
	if contextErr != nil {
		pf(stderr, "error: WSL distro check timed out after %s\n", wslPreflightTimeout)
		return 1
	}
	distribution, distributionErr := parseWSLDistribution(listOutput, *distro)
	if listErr != nil || distributionErr != nil {
		pf(stderr, "error: could not select an installed WSL distro")
		if listErr == nil {
			pf(stderr, ": %v", distributionErr)
		} else if detail := wslProbeDetail(listOutput); detail != "" {
			pf(stderr, ": %s", detail)
		}
		pln(stderr, "; install one with `wsl.exe --install -d <distro>`")
		return 1
	}
	if distribution.version != "2" {
		pf(stderr, "error: distro %q is registered as WSL %s; convert it with `wsl.exe --set-version %q 2`\n",
			distribution.name, distribution.version, distribution.name)
		return 1
	}

	probeArgs := wslDistroArgs(*distro)
	probeArgs = append(probeArgs, "--exec", "/bin/sh", "-lc", wslPreflightProbe)
	ctx, cancel = context.WithTimeout(context.Background(), wslPreflightTimeout)
	output, probeErr := deps.probe(ctx, wslPath, probeArgs)
	contextErr = ctx.Err()
	cancel()
	output = normalizeWSLOutput(output)

	ready := parseWSLReadiness(output)
	if err := reportWSLProbeFailure(contextErr, probeErr, ready, output, *distro, stderr); err != nil {
		return 1
	}

	pf(stdout, "OK: WSL 2 distro %q is ready for full Goobers isolation\n", ready.distro)
	pf(stdout, "  kernel: %s\n", ready.kernel)
	pf(stdout, "  goobers: %s\n", ready.goobers)
	pf(stdout, "  isolation: network:none user + network namespaces; sandbox: %s\n", ready.bubblewrap)

	if !*launch {
		pln(stdout, "Run through WSL with:")
		command := "  goobers preflight --launch-wsl -- run <workflow> ."
		if *distro != "" {
			command = fmt.Sprintf("  goobers preflight --distro %q --launch-wsl -- run <workflow> .", *distro)
		}
		pln(stdout, command)
		return 0
	}

	cwd, err := deps.getwd()
	if err != nil {
		pf(stderr, "error: resolve current directory for WSL handoff: %v\n", err)
		return 1
	}
	launchArgs := wslDistroArgs(*distro)
	launchArgs = append(launchArgs, "--cd", cwd, "--exec", "/bin/sh", "-lc", wslLaunchScript, "goobers-wsl")
	launchArgs = append(launchArgs, ready.goobers)
	launchArgs = append(launchArgs, forwarded...)
	pf(stdout, "Launching in WSL distro %q...\n", ready.distro)
	code, err := deps.launch(wslPath, launchArgs, deps.stdin, stdout, stderr)
	if err != nil {
		pf(stderr, "error: launch Goobers through WSL: %v\n", err)
		return 1
	}
	return code
}

func wslDistroArgs(distro string) []string {
	if distro == "" {
		return nil
	}
	return []string{"--distribution", distro}
}

func parseWSLDistribution(output []byte, requested string) (wslDistribution, error) {
	for _, rawLine := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(rawLine)
		isDefault := strings.HasPrefix(line, "*")
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		version := fields[len(fields)-1]
		if version != "1" && version != "2" {
			continue
		}
		name := strings.Join(fields[:len(fields)-2], " ")
		if requested != "" && strings.EqualFold(name, requested) || requested == "" && isDefault {
			return wslDistribution{name: name, version: version}, nil
		}
	}
	if requested != "" {
		return wslDistribution{}, fmt.Errorf("distro %q is not installed", requested)
	}
	return wslDistribution{}, errors.New("no default WSL distro is installed")
}

func parseWSLReadiness(output []byte) wslReadiness {
	var readiness wslReadiness
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "GOOBERS_WSL_DISTRO":
			readiness.distro = value
		case "GOOBERS_WSL_KERNEL":
			readiness.kernel = value
		case "GOOBERS_WSL_GOOBERS":
			readiness.goobers = value
		case "GOOBERS_WSL_BWRAP":
			readiness.bubblewrap = value
		case "GOOBERS_WSL_ERROR":
			readiness.failure = value
		case "GOOBERS_WSL_READY":
			if value != "1" {
				readiness.failure = "incomplete"
			}
		}
	}
	if readiness.failure == "" &&
		(readiness.distro == "" || readiness.kernel == "" || readiness.goobers == "" || readiness.bubblewrap == "" ||
			!strings.Contains(string(output), "GOOBERS_WSL_READY=1")) {
		readiness.failure = "incomplete"
	}
	return readiness
}

func reportWSLProbeFailure(
	contextErr error,
	probeErr error,
	ready wslReadiness,
	output []byte,
	requestedDistro string,
	stderr io.Writer,
) error {
	if contextErr != nil {
		pf(stderr, "error: WSL readiness check timed out after %s; ensure the distro can start non-interactively\n", wslPreflightTimeout)
		return contextErr
	}
	if probeErr == nil && ready.failure == "" {
		return nil
	}

	distro := ready.distro
	if distro == "" {
		distro = requestedDistro
	}
	if distro == "" {
		distro = "the default distro"
	} else {
		distro = fmt.Sprintf("distro %q", distro)
	}

	switch ready.failure {
	case "goobers":
		pf(stderr, "error: Linux `goobers` is not installed on PATH in %s; install the Linux build inside the distro\n", distro)
	case "bwrap":
		pf(stderr, "error: Bubblewrap (`bwrap`) is not installed in %s; install it in the distro (for Ubuntu/Debian: `sudo apt-get install bubblewrap`)\n", distro)
	case "namespaces":
		pf(stderr, "error: Goobers cannot create the user and network namespaces required by network:none in %s; update WSL and enable unprivileged user namespaces", distro)
		if detail := wslProbeDetail(output); detail != "" {
			pf(stderr, ": %s", detail)
		}
		pln(stderr, "")
	case "bwrap-runtime":
		pf(stderr, "error: Bubblewrap cannot start the agentic-stage sandbox in %s", distro)
		if detail := wslProbeDetail(output); detail != "" {
			pf(stderr, ": %s", detail)
		}
		pln(stderr, "")
	case "linux":
		pf(stderr, "error: %s did not start a Linux environment; repair or reinstall the WSL distro\n", distro)
	default:
		pf(stderr, "error: could not start a ready WSL distro; install/select a WSL 2 distro")
		if detail := wslProbeDetail(output); detail != "" {
			pf(stderr, ": %s", detail)
		}
		pln(stderr, "")
	}
	if probeErr != nil {
		return probeErr
	}
	return errors.New("WSL readiness check failed")
}

func normalizeWSLOutput(output []byte) []byte {
	if len(output) < 2 {
		return output
	}
	hasBOM := output[0] == 0xff && output[1] == 0xfe
	nulBytes := 0
	for _, b := range output {
		if b == 0 {
			nulBytes++
		}
	}
	if !hasBOM && nulBytes*4 < len(output) {
		return output
	}

	start := 0
	if hasBOM {
		start = 2
	}
	codeUnits := make([]uint16, 0, (len(output)-start)/2)
	for i := start; i+1 < len(output); i += 2 {
		codeUnits = append(codeUnits, uint16(output[i])|uint16(output[i+1])<<8)
	}
	return []byte(string(utf16.Decode(codeUnits)))
}

func wslProbeDetail(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	details := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "GOOBERS_WSL_") {
			continue
		}
		details = append(details, line)
	}
	detail := strings.Join(details, "; ")
	const maxDetailBytes = 512
	if len(detail) > maxDetailBytes {
		detail = detail[:maxDetailBytes] + "..."
	}
	return detail
}
