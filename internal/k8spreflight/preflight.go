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
	"net"
	"net/http"
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
	// OIDCIssuer is the customer OIDC issuer for portal/API auth (§1/§3);
	// its discovery document must be reachable from the doctor host.
	OIDCIssuer string
	// Registry is the container registry the cluster pulls Goobers images
	// from (§1), as host[:port] or a full URL.
	Registry string
	// Egress lists required outbound host:port targets — git/backlog
	// provider, model endpoint, sandbox targets (§1/§5).
	Egress []string
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

// Run executes the full check set against the target cluster and returns the
// conformance report. It never returns an error: an unrunnable check is a
// failing row, not an aborted report.
func Run(ctx context.Context, client kubernetes.Interface, opts Options) Report {
	checks := []func(context.Context, kubernetes.Interface, Options) Result{
		checkClusterVersion,
		checkNetworkPolicySupport,
		checkInstallRBAC,
		checkGaggleRBAC,
		checkStorage,
		checkOIDCIssuer,
		checkRegistry,
		checkEgress,
	}
	report := Report{Conformant: true}
	for _, check := range checks {
		result := check(ctx, client, opts)
		if result.Status == StatusFail && result.Severity == SeverityRequired {
			report.Conformant = false
		}
		report.Results = append(report.Results, result)
	}
	return report
}
