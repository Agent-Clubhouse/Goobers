// Package k8spreflight validates a target Kubernetes cluster against the
// documented infrastructure shape (docs/design/k8s-infra-shape.md) before a
// Goobers install — deliverable K3 of that doc, `goobers doctor --k8s`
// (#668). Each check cites the shape-doc section it enforces, carries a
// required/optional severity that traces to the doc rather than ad-hoc
// judgment, and fails closed: a check that cannot run reports fail (or a
// skipped-probe warn) with the reason, never a silent pass.
package k8spreflight

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Status is a check outcome.
type Status string

// Check outcomes: warn covers optional-check misses and skipped probes; fail
// on a required check makes the report non-conformant.
const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Severity classifies a check against the shape doc: a failing required check
// means the cluster does not conform (nonzero exit); an optional check — a
// probe the operator did not configure, or a host-side approximation — never
// blocks conformance on its own.
type Severity string

// Check severities, traced to k8s-infra-shape.md rather than ad-hoc judgment.
const (
	SeverityRequired Severity = "required"
	SeverityOptional Severity = "optional"
)

// Result is one row of the conformance report.
type Result struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Citation string   `json:"citation"` // k8s-infra-shape.md section, e.g. "§4"
	Severity Severity `json:"severity"`
	Status   Status   `json:"status"`
	Detail   string   `json:"detail"`
	Hint     string   `json:"hint,omitempty"` // remediation, shown for non-pass rows
}

// Report is the full conformance report, stable for --report json consumers.
type Report struct {
	// SelectedChecks records restricted coverage. Empty means the full check set.
	SelectedChecks []string `json:"selectedChecks,omitempty"`
	// Target is the cluster endpoint the report was produced against (set by
	// the CLI; empty when unknown).
	Target string `json:"target,omitempty"`
	// Conformant is false iff any required check failed — the CLI exit
	// contract (#668: exit code nonzero iff any required check fails).
	Conformant bool     `json:"conformant"`
	Results    []Result `json:"results"`
}

// DefaultTimeout bounds each network probe when Options.Timeout is zero.
const DefaultTimeout = 10 * time.Second

// Options carries the operator-supplied probe targets. The zero value runs
// the cluster-only checks and reports the network probes as skipped warns.
type Options struct {
	// Checks limits execution to these check IDs. Empty runs the full preflight.
	Checks []string
	// OverlayDir is the consumer kustomization directory. Empty means the
	// overlay-side checks are explicitly unchecked, never a silent pass.
	OverlayDir string
	// ImageRuntime is docker or podman (default docker).
	ImageRuntime string
	// ImagePullPolicy defaults to always; never explicitly inspects cached images.
	ImagePullPolicy string
	// ImageTools names additional PATH tools required of the rendered images.
	ImageTools []string
	// ImageCAFile is the internal root CA whose image trust anchor is checked.
	// Empty leaves that part explicitly unchecked.
	ImageCAFile       string
	runOverlayCommand overlayCommandRunner
	// LookupAPIServerIPs substitutes endpoint DNS resolution in tests. Nil uses
	// the default resolver. The check always supplies its bounded probe context.
	LookupAPIServerIPs func(context.Context, string) ([]net.IP, error)
	// APIServerEndpoint is the endpoint compared with labeled egress policies.
	// The CLI defaults it to kubeconfig, with an override for in-cluster DNAT.
	APIServerEndpoint string
	// OIDCIssuer is the customer OIDC issuer for portal/API auth (§1/§3);
	// its discovery document must be reachable from the doctor host.
	OIDCIssuer string
	// Registry is the container registry the cluster pulls Goobers images
	// from (§1), as host[:port] or a full URL.
	Registry string
	// Egress lists required outbound host:port targets — git/backlog
	// provider, model endpoint, sandbox targets (§1/§5).
	Egress []string
	// OTLPEndpoint is the OTLP collector endpoint the cluster exports to. When
	// empty the signal-set check is a skipped warn.
	OTLPEndpoint string
	// TemporalHostPort is the Temporal frontend's host:port (§2/§4), e.g.
	// "temporal-frontend.goobers-temporal:7233". When empty the namespace
	// check is a skipped warn.
	TemporalHostPort string
	// TemporalNamespace is the namespace the worker/engine connect to
	// (internal/instance/config.go's DefaultTemporalNamespace, "default",
	// unless overridden). #4287: the OSS Temporal chart stands up a cluster
	// with no namespaces registered, so this must exist before first use —
	// see deploy/reference/temporal/namespace-job.yaml.
	TemporalNamespace string
	// DialTemporal dials the Temporal frontend; nil uses client.Dial. Tests
	// substitute a fake to avoid a live Temporal server.
	DialTemporal func(ctx context.Context, hostPort string) (temporalNamespaceDescriber, error)
	// HTTPClient serves the issuer/registry probes; nil builds one bounded
	// by Timeout.
	HTTPClient *http.Client
	// DialContext serves the egress probes; nil uses a net.Dialer bounded by
	// Timeout.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// Timeout bounds each individual probe; zero means DefaultTimeout.
	Timeout time.Duration
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return o.Timeout
}

func (o Options) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: o.timeout()}
}

func (o Options) dialContext() func(ctx context.Context, network, address string) (net.Conn, error) {
	if o.DialContext != nil {
		return o.DialContext
	}
	dialer := &net.Dialer{Timeout: o.timeout()}
	return dialer.DialContext
}

type checkDefinition struct {
	id  string
	run func(context.Context, kubernetes.Interface, Options) Result
}

func checkDefinitions() []checkDefinition {
	return []checkDefinition{
		{"cluster-version", checkClusterVersion},
		{"networkpolicy-api", checkNetworkPolicySupport},
		{"apiserver-ipblock-drift", checkAPIServerIPBlockDrift},
		{"rbac-install", checkInstallRBAC},
		{"rbac-gaggle", checkGaggleRBAC},
		{"storage-rwx", checkStorage},
		{"mixed-os-placement", checkMixedOSPlacement},
		{"runner-class-capacity", checkRunnerClassCapacity},
		{"pod-health", checkPodHealth},
		{"otlp-signal-set", checkOTLPSignalSet},
		{"oidc-issuer", checkOIDCIssuer},
		{"registry", checkRegistry},
		{"egress", checkEgress},
		{"temporal-namespace", checkTemporalNamespace},
		{"overlay-pin-agreement", checkOverlayPinAgreement},
		{"overlay-image-contract", checkOverlayImageContract},
	}
}

// ValidateChecks rejects unknown or repeated selections before any probe runs.
func ValidateChecks(ids []string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if !slices.ContainsFunc(checkDefinitions(), func(c checkDefinition) bool { return c.id == id }) {
			return fmt.Errorf("unknown Kubernetes check %q", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate Kubernetes check %q", id)
		}
		seen[id] = true
	}
	return nil
}

// Run executes the selected checks, or the full check set when none were
// selected. An invalid selection fails without making any probe calls.
func Run(ctx context.Context, client kubernetes.Interface, opts Options) Report {
	report := Report{Conformant: true, SelectedChecks: slices.Clone(opts.Checks)}
	if err := ValidateChecks(opts.Checks); err != nil {
		return Report{SelectedChecks: slices.Clone(opts.Checks), Results: []Result{{ID: "check-selection", Severity: SeverityRequired, Status: StatusFail, Detail: err.Error()}}}
	}
	for _, check := range checkDefinitions() {
		if len(opts.Checks) > 0 && !slices.Contains(opts.Checks, check.id) {
			continue
		}
		result := check.run(ctx, client, opts)
		if result.Status == StatusFail && result.Severity == SeverityRequired {
			report.Conformant = false
		}
		report.Results = append(report.Results, result)
	}
	return report
}
