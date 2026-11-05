package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
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

// runEgressProbe tries a direct TCP connection to each host:port and, when
// the probe's own environment (HTTPS_PROXY, NO_PROXY, ...) routes https://host
// through a proxy the way curl, gh, node, and python would, an HTTP CONNECT to
// host:port through that proxy. It succeeds only when no path connects,
// printing egressBlockedToken.
func runEgressProbe(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "reviewereval egress-probe: no hosts to probe")
		return 2
	}
	proxyFor := httpproxy.FromEnvironment().ProxyFunc()
	reachable := false
	for _, host := range args {
		if conn, err := net.DialTimeout("tcp", host, egressProbeTimeout); err == nil {
			_ = conn.Close()
			reachable = true
			_, _ = fmt.Fprintf(stderr, "reviewereval egress-probe: %s is reachable\n", host)
		}
		proxyURL, err := proxyFor(&url.URL{Scheme: "https", Host: host})
		if err != nil {
			reachable = true
			_, _ = fmt.Fprintf(stderr, "reviewereval egress-probe: cannot resolve the proxy for %s: %v\n", host, err)
			continue
		}
		if proxyURL == nil {
			continue
		}
		ok, err := connectThroughProxy(proxyURL, host)
		if err != nil {
			reachable = true
			_, _ = fmt.Fprintf(stderr, "reviewereval egress-probe: cannot verify %s through proxy %s: %v\n", host, proxyURL.Redacted(), err)
			continue
		}
		if ok {
			reachable = true
			_, _ = fmt.Fprintf(stderr, "reviewereval egress-probe: %s is reachable through proxy %s\n", host, proxyURL.Redacted())
		}
	}
	if reachable {
		return 1
	}
	_, _ = fmt.Fprintln(stdout, egressBlockedToken)
	return 0
}

// connectThroughProxy asks proxy to CONNECT to host and reports whether it
// agreed with a 2xx. An unreachable proxy, or one that refuses or times out,
// counts as blocked; a proxy scheme the probe cannot speak is an error so the
// sandbox fails closed rather than being certified unchecked.
func connectThroughProxy(proxy *url.URL, host string) (bool, error) {
	var conn net.Conn
	var err error
	switch proxy.Scheme {
	case "http":
		conn, err = net.DialTimeout("tcp", proxyAddress(proxy, "80"), egressProbeTimeout)
	case "https":
		dialer := &net.Dialer{Timeout: egressProbeTimeout}
		conn, err = tls.DialWithDialer(dialer, "tcp", proxyAddress(proxy, "443"), &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
	default:
		return false, fmt.Errorf("unsupported proxy scheme %q", proxy.Scheme)
	}
	if err != nil {
		return false, nil
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(egressProbeTimeout))
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: host},
		Host:   host,
		Header: make(http.Header),
	}
	if user := proxy.User; user != nil {
		password, _ := user.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		return false, nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return false, nil
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

func proxyAddress(proxy *url.URL, defaultPort string) string {
	if port := proxy.Port(); port != "" {
		return proxy.Host
	}
	return net.JoinHostPort(proxy.Hostname(), defaultPort)
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
