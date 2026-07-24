package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/goobers/goobers/internal/k8spreflight"
)

const doctorHelp = "Usage: goobers doctor --k8s [--kubeconfig <path>] [--context <name>] [--report text|json]\n" +
	"                          [--oidc-issuer <url>] [--registry <host>] [--egress <host:port,...>]\n" +
	"                          [--timeout <duration>]\n\n" +
	"Preflight a target Kubernetes cluster against the documented infrastructure\n" +
	"shape (docs/design/k8s-infra-shape.md) before installing Goobers on it — the\n" +
	"install-time enforcement of that document (#668). --k8s is the only doctor\n" +
	"mode today.\n\n" +
	"The check set, each row citing the shape-doc section it enforces:\n\n" +
	"  cluster-version    required  §1     cluster reachable, supported version\n" +
	"  networkpolicy-api  required  §5     NetworkPolicy API served (deny-first enforceable)\n" +
	"  rbac-install       required  §1/§3  permissions to install goobers-system\n" +
	"  rbac-gaggle        required  §3/§5  permissions to stamp per-gaggle namespaces\n" +
	"  storage-rwx        required  §4     ReadWriteMany-capable StorageClass exists\n" +
	"  oidc-issuer        required* §1/§3  issuer discovery document reachable\n" +
	"  egress             required* §1/§5  outbound targets reachable from this host\n" +
	"  registry           optional  §1     registry reachable (host-side sanity)\n\n" +
	"Checks marked required* apply when their probe target is configured; left\n" +
	"unconfigured they report a skipped warn. Every check is read-only: nothing is\n" +
	"created on the cluster, and a check that cannot run reports fail with the\n" +
	"reason — never a silent pass. Reference manifests expressing the same\n" +
	"requirements live under deploy/reference/ (#663).\n\n" +
	"--report json emits the stable machine-readable report; text (default) prints\n" +
	"the conformance table with remediation hints.\n\n" +
	"Exit codes: 0 = cluster conforms (warns allowed), 1 = a required check\n" +
	"failed, 2 = usage/IO error.\n"

// doctorKubeClient builds the typed clientset for the target
// kubeconfig/context, returning the cluster endpoint for the report header.
// A seam so tests can substitute a fake clientset.
var doctorKubeClient = func(kubeconfig, contextName string, timeout time.Duration) (kubernetes.Interface, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	restConfig.Timeout = timeout
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, "", fmt.Errorf("build cluster client: %w", err)
	}
	return client, restConfig.Host, nil
}

// runDoctor backs `goobers doctor`. The --k8s mode runs the cluster preflight
// (k8s-infra-shape.md deliverable K3, #668) via internal/k8spreflight and
// renders the conformance report.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	k8sMode := fs.Bool("k8s", false, "preflight a Kubernetes cluster against docs/design/k8s-infra-shape.md")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: the standard loading rules)")
	kubeContext := fs.String("context", "", "kubeconfig context (default: the current context)")
	reportFormat := fs.String("report", "text", "report format: text or json")
	oidcIssuer := fs.String("oidc-issuer", "", "OIDC issuer URL whose discovery document must be reachable")
	registry := fs.String("registry", "", "container registry host to probe for reachability")
	egress := fs.String("egress", "", "comma-separated host:port outbound targets that must be reachable")
	timeout := fs.Duration("timeout", k8spreflight.DefaultTimeout, "per-probe timeout")
	fs.Usage = helpUsage(stderr, "doctor")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if !*k8sMode {
		pf(stderr, "goobers doctor: --k8s is the only doctor mode today (cluster preflight, #668)\n\n")
		fs.Usage()
		return 2
	}
	if *reportFormat != "text" && *reportFormat != "json" {
		pf(stderr, "goobers doctor: --report must be text or json, got %q\n", *reportFormat)
		return 2
	}

	client, host, err := doctorKubeClient(*kubeconfig, *kubeContext, *timeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	report := k8spreflight.Run(context.Background(), client, k8spreflight.Options{
		OIDCIssuer: *oidcIssuer,
		Registry:   *registry,
		Egress:     splitCommaList(*egress),
		Timeout:    *timeout,
	})
	report.Target = host

	if *reportFormat == "json" {
		if err := k8spreflight.WriteJSON(stdout, report); err != nil {
			pf(stderr, "error: encode report: %v\n", err)
			return 2
		}
	} else {
		k8spreflight.WriteText(stdout, report)
	}
	if !report.Conformant {
		return 1
	}
	return 0
}

func splitCommaList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
