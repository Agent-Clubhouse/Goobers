package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
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

var (
	containerIDPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	containerNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	skewPattern          = regexp.MustCompile(`^[+-][0-9]+[smhd]$`)
	nodePattern          = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// ClockSkew shifts one pod's clock without restarting it. The pod must run an
// image that preloads libfaketime and sets FAKETIME_TIMESTAMP_FILE (with
// FAKETIME_NO_CACHE=1), so the offset is re-read from a file; Apply writes the
// offset there and reads it back, and fails if libfaketime is not preloaded.
// (Setting FAKETIME through the deployment env would roll the pods, which
// confounds any restart-sensitive scenario.)
type ClockSkew struct {
	Exec      Executor
	Namespace string
	Pod       string
	Container string
	Offset    string // libfaketime relative form, e.g. "-10m"
}

// Name implements Fault.
func (f ClockSkew) Name() string { return "clock-skew:" + f.Pod }

func (f ClockSkew) exec(ctx context.Context, script string) (string, error) {
	return f.Exec.Run(ctx, "", "kubectl", "exec", f.Pod, "-n", f.Namespace, "-c", f.Container, "--", "sh", "-c", script)
}

func (f ClockSkew) write(ctx context.Context, offset string) error {
	if !skewPattern.MatchString(offset) && offset != "+0s" {
		return fmt.Errorf("clock offset %q must look like -10m", offset)
	}
	check := `case "$LD_PRELOAD" in *faketime*) ;; *) echo "libfaketime not preloaded" >&2; exit 3;; esac; ` +
		`[ -n "$FAKETIME_TIMESTAMP_FILE" ] || { echo "FAKETIME_TIMESTAMP_FILE unset" >&2; exit 3; }; `
	out, err := f.exec(ctx, check+`printf '%s' '`+offset+`' > "$FAKETIME_TIMESTAMP_FILE" && cat "$FAKETIME_TIMESTAMP_FILE"`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != offset {
		return fmt.Errorf("clock offset read back %q, want %q", strings.TrimSpace(out), offset)
	}
	return nil
}

// Apply implements Fault.
func (f ClockSkew) Apply(ctx context.Context) error { return f.write(ctx, f.Offset) }

// Revert implements Fault.
func (f ClockSkew) Revert(ctx context.Context) error { return f.write(ctx, "+0s") }

// CgroupFreeze pauses every process of one container by freezing its cgroup
// (cgroup v2 cgroup.freeze) from a privileged helper pod on the container's
// node. Signalling from inside the container cannot work: the kernel drops
// SIGSTOP sent to a PID-namespace init by members of that namespace. The
// target is the container ID read from the pod status, never a process name,
// and Apply fails unless cgroup.events reports "frozen 1" afterwards.
type CgroupFreeze struct {
	Exec      Executor
	Namespace string
	Pod       string
	Container string
	// HelperImage is a pinned image with sh and find; the caller supplies the
	// digest.
	HelperImage string

	// HelperNamespace hosts the privileged helper pod; it is created with the
	// privileged Pod Security level and kept apart from the target namespace,
	// whose restricted policy would reject the helper. Default DefaultChaosNamespace.
	HelperNamespace string

	helper string
	cgroup string
}

// DefaultChaosNamespace is where privileged chaos helper pods run.
const DefaultChaosNamespace = "goobers-chaos"

const (
	helperLabel      = "app.kubernetes.io/managed-by=goobers-chaos"
	cgroupAnnotation = "goobers.dev/chaos-cgroup"
)

func (f *CgroupFreeze) helperNS() string {
	if f.HelperNamespace == "" {
		return DefaultChaosNamespace
	}
	return f.HelperNamespace
}

const chaosNamespaceManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    pod-security.kubernetes.io/enforce: privileged
    app.kubernetes.io/managed-by: goobers-chaos
`

// Name implements Fault.
func (f *CgroupFreeze) Name() string { return "cgroup-freeze:" + f.Pod }

// Apply implements Fault.
func (f *CgroupFreeze) Apply(ctx context.Context) error {
	if f.HelperImage == "" {
		return errors.New("cgroup freeze needs a pinned helper image")
	}
	if !containerNamePattern.MatchString(f.Container) {
		return fmt.Errorf("container name %q is invalid", f.Container)
	}
	idOut, err := f.Exec.Run(ctx, "", "kubectl", "get", "pod", f.Pod, "-n", f.Namespace, "-o",
		fmt.Sprintf(`jsonpath={.status.containerStatuses[?(@.name=="%s")].containerID}`, f.Container))
	if err != nil {
		return err
	}
	id := strings.TrimSpace(idOut)
	if i := strings.Index(id, "://"); i >= 0 {
		id = id[i+3:]
	}
	if !containerIDPattern.MatchString(id) {
		return fmt.Errorf("container ID %q is not a 64-hex ID", id)
	}
	nodeOut, err := f.Exec.Run(ctx, "", "kubectl", "get", "pod", f.Pod, "-n", f.Namespace, "-o", "jsonpath={.spec.nodeName}")
	if err != nil {
		return err
	}
	node := strings.TrimSpace(nodeOut)
	if !nodePattern.MatchString(node) {
		return fmt.Errorf("node name %q is invalid", node)
	}
	f.helper = "chaos-freezer-" + id[:12]
	if _, err := f.Exec.Run(ctx, fmt.Sprintf(chaosNamespaceManifest, f.helperNS()), "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err := f.Exec.Run(ctx, f.helperManifest(node), "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err := f.Exec.Run(ctx, "", "kubectl", "wait", "--for=condition=Ready", "pod/"+f.helper, "-n", f.helperNS(), "--timeout=120s"); err != nil {
		_ = f.deleteHelper(ctx)
		return err
	}
	found, err := f.hostExec(ctx, `find /host/sys/fs/cgroup -type d -name '*`+id+`*' | head -n 1`)
	if err != nil {
		_ = f.deleteHelper(ctx)
		return err
	}
	f.cgroup = strings.TrimSpace(found)
	if f.cgroup == "" || !strings.HasPrefix(f.cgroup, "/host/sys/fs/cgroup/") {
		_ = f.deleteHelper(ctx)
		return fmt.Errorf("no cgroup directory found for container %s", id)
	}
	// Record the cgroup on the helper before freezing so Sweep can thaw it
	// after an aborted run.
	if _, err := f.Exec.Run(ctx, "", "kubectl", "annotate", "pod", f.helper, "-n", f.helperNS(), "--overwrite", cgroupAnnotation+"="+f.cgroup); err != nil {
		_ = f.deleteHelper(ctx)
		return err
	}
	return f.setFrozen(ctx, true)
}

// Revert implements Fault. It thaws the cgroup, verifies, and removes the helper.
func (f *CgroupFreeze) Revert(ctx context.Context) error {
	if f.helper == "" {
		return nil
	}
	var err error
	if f.cgroup != "" {
		err = f.setFrozen(ctx, false)
	}
	if derr := f.deleteHelper(ctx); err == nil {
		err = derr
	}
	return err
}

func (f *CgroupFreeze) setFrozen(ctx context.Context, frozen bool) error {
	want := "0"
	if frozen {
		want = "1"
	}
	out, err := f.hostExec(ctx, fmt.Sprintf(`echo %s > %s/cgroup.freeze && sleep 1 && grep '^frozen ' %s/cgroup.events`, want, f.cgroup, f.cgroup))
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(out); got != "frozen "+want {
		return fmt.Errorf("cgroup %s reports %q after freeze=%s", f.cgroup, got, want)
	}
	return nil
}

func (f *CgroupFreeze) hostExec(ctx context.Context, script string) (string, error) {
	return f.Exec.Run(ctx, "", "kubectl", "exec", f.helper, "-n", f.helperNS(), "--", "sh", "-c", script)
}

func (f *CgroupFreeze) deleteHelper(ctx context.Context) error {
	_, err := f.Exec.Run(ctx, "", "kubectl", "delete", "pod", f.helper, "-n", f.helperNS(), "--ignore-not-found", "--wait=false")
	return err
}

func (f *CgroupFreeze) helperManifest(node string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: goobers-chaos
spec:
  nodeName: %s
  hostPID: true
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  containers:
  - name: helper
    image: %s
    command: ["sleep", "3600"]
    securityContext:
      privileged: true
    volumeMounts:
    - {name: host, mountPath: /host}
  volumes:
  - name: host
    hostPath: {path: /}
`, f.helper, f.helperNS(), node, f.HelperImage)
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

// Sweep thaws and deletes every leftover chaos helper pod in namespace ns, so
// an aborted run never leaves a frozen pod behind. A helper whose cgroup
// cannot be verified thawed is kept and reported: deleting it would strand the
// freeze. A missing namespace or no helpers is a clean sweep.
func Sweep(ctx context.Context, exec Executor, ns string) error {
	out, err := exec.Run(ctx, "", "kubectl", "get", "pods", "-n", ns, "-l", helperLabel, "--ignore-not-found",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{.metadata.annotations.goobers\.dev/chaos-cgroup}{"\n"}{end}`)
	if err != nil {
		return err
	}
	var failed []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, cgroup, _ := strings.Cut(strings.TrimSpace(line), "=")
		if name == "" {
			continue
		}
		if cgroup != "" {
			if !strings.HasPrefix(cgroup, "/host/sys/fs/cgroup/") || strings.Contains(cgroup, "..") || strings.ContainsAny(cgroup, "'\" ;$`&|<>()\n") {
				failed = append(failed, name+": unsafe cgroup annotation")
				continue
			}
			f := &CgroupFreeze{Exec: exec, HelperNamespace: ns, helper: name, cgroup: cgroup}
			if err := f.setFrozen(ctx, false); err != nil {
				failed = append(failed, name+": "+err.Error())
				continue
			}
		}
		if _, err := exec.Run(ctx, "", "kubectl", "delete", "pod", name, "-n", ns, "--ignore-not-found", "--wait=false"); err != nil {
			failed = append(failed, name+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("chaos sweep left helpers: %s", strings.Join(failed, "; "))
	}
	return nil
}
