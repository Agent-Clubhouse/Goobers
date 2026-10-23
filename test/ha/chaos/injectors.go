package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Executor runs an external command (kubectl) and returns its stdout.
type Executor interface {
	Run(ctx context.Context, stdin string, name string, args ...string) (string, error)
}

// OSExecutor runs real commands.
type OSExecutor struct{}

// Run implements Executor.
func (OSExecutor) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

// Fault is a reversible injected failure.
type Fault interface {
	Name() string
	Apply(ctx context.Context) error
	Revert(ctx context.Context) error
}

// Toxiproxy drives a toxiproxy server's HTTP API.
type Toxiproxy struct {
	BaseURL string
	Client  *http.Client
}

func (t *Toxiproxy) do(ctx context.Context, method, path string, body any) error {
	var rdr *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.BaseURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("toxiproxy %s %s: status %d", method, path, resp.StatusCode)
	}
	return nil
}

// ProxyDown disables a proxy (connection refused); Revert re-enables it.
type ProxyDown struct {
	T     *Toxiproxy
	Proxy string
}

// Name implements Fault.
func (f ProxyDown) Name() string { return "toxiproxy-down:" + f.Proxy }

// Apply implements Fault.
func (f ProxyDown) Apply(ctx context.Context) error {
	return f.T.do(ctx, http.MethodPost, "/proxies/"+f.Proxy, map[string]any{"enabled": false})
}

// Revert implements Fault.
func (f ProxyDown) Revert(ctx context.Context) error {
	return f.T.do(ctx, http.MethodPost, "/proxies/"+f.Proxy, map[string]any{"enabled": true})
}

// Latency adds a latency toxic; Revert removes it.
type Latency struct {
	T        *Toxiproxy
	Proxy    string
	Millis   int
	JitterMS int
}

// Name implements Fault.
func (f Latency) Name() string { return "toxiproxy-latency:" + f.Proxy }

// Apply implements Fault.
func (f Latency) Apply(ctx context.Context) error {
	return f.T.do(ctx, http.MethodPost, "/proxies/"+f.Proxy+"/toxics", map[string]any{
		"name": "chaos-latency", "type": "latency", "stream": "downstream", "toxicity": 1.0,
		"attributes": map[string]int{"latency": f.Millis, "jitter": f.JitterMS},
	})
}

// Revert implements Fault.
func (f Latency) Revert(ctx context.Context) error {
	return f.T.do(ctx, http.MethodDelete, "/proxies/"+f.Proxy+"/toxics/chaos-latency", nil)
}

// NetworkPartition applies a deny-all-egress NetworkPolicy to pods matching a
// label selector; it needs an enforcing CNI (the nightly runs Calico).
type NetworkPartition struct {
	Exec      Executor
	Namespace string
	Name_     string
	Selector  string // app label value
}

// Name implements Fault.
func (f NetworkPartition) Name() string { return "netpol-partition:" + f.Name_ }

// Apply implements Fault.
func (f NetworkPartition) Apply(ctx context.Context) error {
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %s
  namespace: %s
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: %s
  policyTypes: [Ingress, Egress]
`, f.Name_, f.Namespace, f.Selector)
	_, err := f.Exec.Run(ctx, manifest, "kubectl", "apply", "-f", "-")
	return err
}

// Revert implements Fault.
func (f NetworkPartition) Revert(ctx context.Context) error {
	_, err := f.Exec.Run(ctx, "", "kubectl", "delete", "networkpolicy", f.Name_, "-n", f.Namespace, "--ignore-not-found")
	return err
}

// CNPGFailover deletes the current primary pod of a CNPG cluster, by its exact
// name, forcing a failover. Revert is a no-op: the operator heals the cluster.
type CNPGFailover struct {
	Exec      Executor
	Namespace string
	Cluster   string
}

// Name implements Fault.
func (f CNPGFailover) Name() string { return "cnpg-failover:" + f.Cluster }

// Apply implements Fault.
func (f CNPGFailover) Apply(ctx context.Context) error {
	out, err := f.Exec.Run(ctx, "", "kubectl", "get", "cluster", f.Cluster, "-n", f.Namespace, "-o", "jsonpath={.status.currentPrimary}")
	if err != nil {
		return err
	}
	primary := strings.TrimSpace(out)
	if primary == "" {
		return fmt.Errorf("cluster %s reports no current primary", f.Cluster)
	}
	_, err = f.Exec.Run(ctx, "", "kubectl", "delete", "pod", primary, "-n", f.Namespace, "--wait=false")
	return err
}

// Revert implements Fault.
func (CNPGFailover) Revert(context.Context) error { return nil }

// ClockSkew sets FAKETIME on a deployment whose image preloads libfaketime;
// the DB clock stays authoritative, so this exercises pod-clock independence.
type ClockSkew struct {
	Exec       Executor
	Namespace  string
	Deployment string
	Offset     string // libfaketime form, e.g. "-10m"
}

// Name implements Fault.
func (f ClockSkew) Name() string { return "clock-skew:" + f.Deployment }

// Apply implements Fault.
func (f ClockSkew) Apply(ctx context.Context) error {
	_, err := f.Exec.Run(ctx, "", "kubectl", "set", "env", "deployment/"+f.Deployment, "-n", f.Namespace, "FAKETIME="+f.Offset)
	return err
}

// Revert implements Fault.
func (f ClockSkew) Revert(ctx context.Context) error {
	_, err := f.Exec.Run(ctx, "", "kubectl", "set", "env", "deployment/"+f.Deployment, "-n", f.Namespace, "FAKETIME-")
	return err
}

var pidPattern = regexp.MustCompile(`^[0-9]+$`)

// SigStop pauses a process with SIGSTOP, addressed only by PID read from the
// pod's cgroup. There is deliberately no name-based path: killing by name has
// taken down unrelated processes on shared hosts.
type SigStop struct {
	Exec      Executor
	Namespace string
	Pod       string
	Container string
	pid       int
}

// Name implements Fault.
func (f *SigStop) Name() string { return "sigstop:" + f.Pod }

// Apply implements Fault. It stops the lowest PID in the container cgroup (the main process).
func (f *SigStop) Apply(ctx context.Context) error {
	out, err := f.Exec.Run(ctx, "", "kubectl", "exec", f.Pod, "-n", f.Namespace, "-c", f.Container, "--", "cat", "/sys/fs/cgroup/cgroup.procs")
	if err != nil {
		return err
	}
	pid, err := lowestPID(out)
	if err != nil {
		return err
	}
	f.pid = pid
	return f.signal(ctx, "STOP")
}

// Revert implements Fault.
func (f *SigStop) Revert(ctx context.Context) error {
	if f.pid == 0 {
		return nil
	}
	return f.signal(ctx, "CONT")
}

func (f *SigStop) signal(ctx context.Context, sig string) error {
	_, err := f.Exec.Run(ctx, "", "kubectl", "exec", f.Pod, "-n", f.Namespace, "-c", f.Container, "--", "kill", "-"+sig, strconv.Itoa(f.pid))
	return err
}

func lowestPID(procs string) (int, error) {
	lowest := 0
	for _, line := range strings.Fields(procs) {
		if !pidPattern.MatchString(line) {
			return 0, fmt.Errorf("cgroup.procs entry %q is not a PID", line)
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return 0, err
		}
		if pid > 0 && (lowest == 0 || pid < lowest) {
			lowest = pid
		}
	}
	if lowest == 0 {
		return 0, fmt.Errorf("no PID in cgroup.procs")
	}
	return lowest, nil
}

// WithFault applies f, runs fn, and always reverts, joining any revert error.
func WithFault(ctx context.Context, f Fault, fn func(context.Context) error) (err error) {
	if err := f.Apply(ctx); err != nil {
		return fmt.Errorf("apply %s: %w", f.Name(), err)
	}
	defer func() {
		// Revert on a fresh context: ctx may be the reason fn returned.
		if rerr := f.Revert(context.WithoutCancel(ctx)); rerr != nil && err == nil {
			err = fmt.Errorf("revert %s: %w", f.Name(), rerr)
		}
	}()
	return fn(ctx)
}
