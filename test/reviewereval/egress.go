package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// egressProbeCommand is the hidden subcommand replay runs inside the
// operator's sandbox to prove that adapters cannot reach the forge.
const egressProbeCommand = "egress-probe"

// egressBlockedToken is the only stdout that verifies a sandbox: a wrapper
// that never runs the probe, or exits 0 without running it, fails closed.
const egressBlockedToken = "reviewereval: egress blocked"

// defaultForbiddenHosts are the forge web, API, and archive endpoints through
// which an adapter could read this repository's later history and fixes.
var defaultForbiddenHosts = []string{"github.com:443", "api.github.com:443", "codeload.github.com:443", "raw.githubusercontent.com:443"}

// egressProbeTimeout bounds each connection attempt; a sandbox that silently
// drops packets counts as blocked once it elapses.
const egressProbeTimeout = 5 * time.Second

// runEgressProbe tries a TCP connection to each host:port and succeeds only
// when none connects, printing egressBlockedToken.
func runEgressProbe(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "reviewereval egress-probe: no hosts to probe")
		return 2
	}
	reachable := false
	for _, host := range args {
		conn, err := net.DialTimeout("tcp", host, egressProbeTimeout)
		if err != nil {
			continue
		}
		_ = conn.Close()
		reachable = true
		_, _ = fmt.Fprintf(stderr, "reviewereval egress-probe: %s is reachable\n", host)
	}
	if reachable {
		return 1
	}
	_, _ = fmt.Fprintln(stdout, egressBlockedToken)
	return 0
}

// verifyEgressBlocked runs probe (the reviewereval binary and its
// egress-probe subcommand) through sandbox with the adapter environment, and
// fails unless the probe ran there and reached none of hosts. Replay refuses
// to start a reviewer until it passes, so recall is never measured with an
// adapter that can look the fix up on the forge.
func verifyEgressBlocked(ctx context.Context, sandbox, probe, hosts []string, workDir string, passthrough []string) error {
	if len(sandbox) == 0 {
		return errors.New("-sandbox-cmd is required: adapters must run where the forge is unreachable")
	}
	if len(hosts) == 0 {
		return errors.New("-forbidden-hosts may not be empty")
	}
	for _, host := range hosts {
		if _, _, err := net.SplitHostPort(host); err != nil {
			return fmt.Errorf("-forbidden-hosts entry %q: %w", host, err)
		}
	}
	scratch, err := os.MkdirTemp(workDir, "reviewer-eval-egress-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	env, err := adapterEnvironment(scratch, passthrough)
	if err != nil {
		return err
	}
	argv := append(append(append([]string{}, sandbox...), probe...), hosts...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = scratch
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr != nil {
		return fmt.Errorf("sandbox does not block egress to %s: %w: %s", strings.Join(hosts, ", "), runErr, strings.TrimSpace(stderr.String()))
	}
	if !strings.Contains(stdout.String(), egressBlockedToken) {
		return fmt.Errorf("sandbox did not run the egress probe (%s): its output was %q", strings.Join(probe, " "), strings.TrimSpace(stdout.String()))
	}
	return nil
}
