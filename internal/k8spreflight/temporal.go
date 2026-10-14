package k8spreflight

import (
	"context"
	"fmt"
	"strings"
	"time"

	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"k8s.io/client-go/kubernetes"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporalcodec"
	"github.com/goobers/goobers/internal/temporaldial"
	"github.com/goobers/goobers/internal/workerrouting"
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
		if opts.TemporalTaskQueue == "" {
			opts.TemporalTaskQueue = engineConfig.TaskQueue
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

// DescribeRouting and WorkflowPollers make the dialed client a
// workerrouting.Client for the temporal-worker-routing check (#6020).
func (d dialedTemporalClient) DescribeRouting(ctx context.Context) (client.WorkerDeploymentRoutingConfig, error) {
	return workerrouting.NewClient(d.c).DescribeRouting(ctx)
}

func (d dialedTemporalClient) WorkflowPollers(ctx context.Context, taskQueue string) ([]*taskqueuepb.PollerInfo, error) {
	return workerrouting.NewClient(d.c).WorkflowPollers(ctx, taskQueue)
}

// defaultDialTemporal dials through the shared temporaldial constructor
// (#5289) with the check's TLS options; nil tls is the plaintext dial.
// DescribeNamespace names its namespace explicitly; the worker-routing reads
// are scoped by the dialed one.
func defaultDialTemporal(tls *temporaldial.TLS, namespace string, dc ...converter.DataConverter) func(context.Context, string) (temporalNamespaceDescriber, error) {
	return func(ctx context.Context, hostPort string) (temporalNamespaceDescriber, error) {
		c, err := temporaldial.Dial(ctx, hostPort, namespace, tls, dc...)
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
	namespace := opts.temporalNamespace()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	c, err := opts.dialTemporal()(ctx, opts.TemporalHostPort)
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

func (o Options) temporalNamespace() string {
	if o.TemporalNamespace == "" {
		return "default"
	}
	return o.TemporalNamespace
}

func (o Options) dialTemporal() func(context.Context, string) (temporalNamespaceDescriber, error) {
	if o.DialTemporal != nil {
		return o.DialTemporal
	}
	return defaultDialTemporal(o.TemporalTLS, o.temporalNamespace(), o.TemporalDataConverter)
}

// checkTemporalWorkerRouting reports whether the "goobers" worker deployment
// routes workflow tasks to the workers polling the engine task queue (#6020).
// A versioned worker whose build is not current polls cleanly and receives
// nothing, so a stalled engine passes every other check: Ready worker pods, a
// reachable frontend, a registered namespace. The verdict is the one the
// worker logs (internal/workerrouting). It never changes the routing (#4084):
// a mismatch carries the set-current-version command as its hint.
//
// No live worker routed fails; a rollout whose new build is unrouted while
// the old build still serves warns; an unreadable routing or task queue warns
// as unknown, never passes.
func checkTemporalWorkerRouting(ctx context.Context, _ kubernetes.Interface, opts Options) Result {
	result := Result{
		ID:       "temporal-worker-routing",
		Title:    "Temporal worker deployment routes tasks to the live workers",
		Citation: "§2/§4",
	}
	if opts.TemporalHostPort == "" {
		result.Severity = SeverityOptional
		result.Status = StatusWarn
		result.Detail = "skipped — no Temporal frontend configured"
		result.Hint = "rerun with --instance <root> (or --temporal-hostport <host:port>)"
		return result
	}
	result.Severity = SeverityRequired
	taskQueue := opts.TemporalTaskQueue
	if taskQueue == "" {
		taskQueue = instance.DefaultEngineTaskQueue
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	assessment := assessWorkerRouting(ctx, opts, taskQueue)
	result.Detail = string(assessment.State) + ": " + assessment.Detail
	switch assessment.State {
	case workerrouting.StateHealthy:
		result.Status = StatusPass
	case workerrouting.StateStalled:
		result.Status = StatusFail
	default:
		result.Status = StatusWarn
	}
	switch {
	case len(assessment.Remediation) > 0:
		result.Hint = "run: " + strings.Join(assessment.Remediation, " (or) ")
	case assessment.State == workerrouting.StateUnknown && opts.TemporalTaskQueue == "":
		result.Hint = "rerun with --instance <root> so the check reads the instance's engine task queue"
	}
	return result
}

func assessWorkerRouting(ctx context.Context, opts Options, taskQueue string) workerrouting.Assessment {
	c, err := opts.dialTemporal()(ctx, opts.TemporalHostPort)
	if err != nil {
		return workerrouting.Unknown(taskQueue, fmt.Sprintf("could not reach Temporal frontend %s over %s: %v",
			opts.TemporalHostPort, opts.TemporalTLS.Transport(), err), time.Now())
	}
	defer c.Close()
	routing, ok := c.(workerrouting.Client)
	if !ok {
		return workerrouting.Unknown(taskQueue, "this Temporal client cannot read worker deployment routing", time.Now())
	}
	return workerrouting.Assess(ctx, routing, taskQueue, time.Now())
}
