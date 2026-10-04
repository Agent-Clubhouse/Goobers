package k8spreflight

import (
	"context"
	"fmt"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"k8s.io/client-go/kubernetes"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporalcodec"
	"github.com/goobers/goobers/internal/temporaldial"
)

// ResolveTemporalOptions applies instance transport and payload settings to the
// preflight. Explicit targets take precedence; without an instance, empty targets
// remain empty so the Temporal check stays optional. Construction performs no key
// operation or Temporal dial.
func ResolveTemporalOptions(root string, opts Options) (Options, error) {
	cfg := &instance.Config{}
	if root != "" {
		var err error
		cfg, err = instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			return Options{}, err
		}
	}
	dc, err := temporalcodec.DataConverter(cfg)
	if err != nil {
		return Options{}, fmt.Errorf("temporal payload codec: %w", err)
	}
	engineConfig := cfg.EffectiveEngineConfig()
	opts.TemporalTLS = engineConfig.TLS
	opts.TemporalDataConverter = dc
	if root != "" {
		if opts.TemporalHostPort == "" {
			opts.TemporalHostPort = engineConfig.HostPort
		}
		if opts.TemporalNamespace == "" {
			opts.TemporalNamespace = engineConfig.Namespace
		}
	}
	return opts, nil
}

// temporalNamespaceDescriber is the narrow slice of client.Client this
// package needs — just enough to check namespace existence, so a test can
// substitute a fake without dialing a live Temporal server.
type temporalNamespaceDescriber interface {
	DescribeNamespace(ctx context.Context, in *workflowservice.DescribeNamespaceRequest) (*workflowservice.DescribeNamespaceResponse, error)
	Close()
}

// dialedTemporalClient adapts client.Client (whose WorkflowService() returns
// the raw gRPC service, not a namespace-scoped describe method) to
// temporalNamespaceDescriber.
type dialedTemporalClient struct {
	c client.Client
}

func (d dialedTemporalClient) DescribeNamespace(ctx context.Context, in *workflowservice.DescribeNamespaceRequest) (*workflowservice.DescribeNamespaceResponse, error) {
	return d.c.WorkflowService().DescribeNamespace(ctx, in)
}

func (d dialedTemporalClient) Close() { d.c.Close() }

// defaultDialTemporal dials through the shared temporaldial constructor
// (#5289) with the check's TLS options; nil tls is the plaintext dial. The
// namespace is left empty, as before: DescribeNamespace names it explicitly.
func defaultDialTemporal(tls *temporaldial.TLS, dc ...converter.DataConverter) func(context.Context, string) (temporalNamespaceDescriber, error) {
	return func(ctx context.Context, hostPort string) (temporalNamespaceDescriber, error) {
		c, err := temporaldial.Dial(ctx, hostPort, "", tls, dc...)
		if err != nil {
			return nil, err
		}
		return dialedTemporalClient{c: c}, nil
	}
}

// checkTemporalNamespace verifies the Temporal namespace the worker/engine
// connect to actually exists (#4287). The OSS Temporal Helm chart stands up
// a cluster with no namespaces registered at all: every other health signal
// (pods Running, frontend reachable) passes while the worker/engine dies
// with "Namespace <ns> is not found" — deploy/reference/temporal/
// namespace-job.yaml is the fix; this check is what would have caught its
// absence before a run ever tried to use it.
func checkTemporalNamespace(ctx context.Context, _ kubernetes.Interface, opts Options) Result {
	result := Result{
		ID:       "temporal-namespace",
		Title:    "configured Temporal namespace is registered",
		Citation: "§2/§4",
	}
	if opts.TemporalHostPort == "" {
		result.Severity = SeverityOptional
		result.Status = StatusWarn
		result.Detail = "skipped — no Temporal frontend configured"
		result.Hint = "rerun with --temporal-hostport <host:port> (and --temporal-namespace, default \"default\")"
		return result
	}
	result.Severity = SeverityRequired
	namespace := opts.TemporalNamespace
	if namespace == "" {
		namespace = "default"
	}
	dial := opts.DialTemporal
	if dial == nil {
		dial = defaultDialTemporal(opts.TemporalTLS, opts.TemporalDataConverter)
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	c, err := dial(ctx, opts.TemporalHostPort)
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("could not reach Temporal frontend %s over %s: %v", opts.TemporalHostPort, opts.TemporalTLS.Transport(), err)
		return result
	}
	defer c.Close()
	if _, err := c.DescribeNamespace(ctx, &workflowservice.DescribeNamespaceRequest{Namespace: namespace}); err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("namespace %q is not registered: %v", namespace, err)
		result.Hint = "apply deploy/reference/temporal/namespace-job.yaml (must run after the chart, before the worker/engine)"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("namespace %q is registered (transport: %s)", namespace, opts.TemporalTLS.Transport())
	return result
}
