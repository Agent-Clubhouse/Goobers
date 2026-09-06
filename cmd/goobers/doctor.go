package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/k8spreflight"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

const doctorHelp = "Usage: goobers doctor --k8s [--kubeconfig <path>] [--context <name>] [--report text|json]\n" +
	"                          [--oidc-issuer <url>] [--registry <host>] [--egress <host:port,...>]\n" +
	"                          [--temporal-hostport <host:port>] [--temporal-namespace <name>]\n" +
	"                          [--timeout <duration>]\n" +
	"       goobers doctor --repo [--report text|json] [instance-root]\n" +
	"       goobers doctor --av-exclusions [--report text|json] [--work-root <dir>] [instance-root]\n\n" +
	"--k8s preflights a target Kubernetes cluster against the documented\n" +
	"infrastructure shape (docs/design/k8s-infra-shape.md) before installing\n" +
	"Goobers on it — the install-time enforcement of that document (#668).\n\n" +
	"The --k8s check set, each row citing the shape-doc section it enforces:\n\n" +
	"  cluster-version    required  §1     cluster reachable, supported version\n" +
	"  networkpolicy-api  required  §5     NetworkPolicy API served (warn: enforcement unverified)\n" +
	"  rbac-install       required  §1/§3  permissions to install goobers-system\n" +
	"  rbac-gaggle        required  §3/§5  permissions to stamp per-gaggle namespaces\n" +
	"  storage-rwx        required  §4     ReadWriteMany-capable StorageClass exists\n" +
	"  mixed-os-placement required  §7     Linux workloads cannot land on Windows nodes\n" +
	"  oidc-issuer        required* §1/§3  issuer discovery document reachable\n" +
	"  egress             required* §1/§5  outbound targets reachable from this host\n" +
	"  temporal-namespace required* §2/§4  configured Temporal namespace is registered\n" +
	"  registry           optional  §1     registry reachable (host-side sanity)\n\n" +
	"Checks marked required* apply when their probe target is configured; left\n" +
	"unconfigured they report a skipped warn. Every check is read-only: nothing is\n" +
	"created on the cluster, and a check that cannot run reports fail with the\n" +
	"reason — never a silent pass. Reference manifests expressing the same\n" +
	"requirements live under deploy/reference/ (#663).\n\n" +
	"networkpolicy-api warns even when the API is served: a served API is only a\n" +
	"correlate of enforcement — a CNI can serve it and still ignore policies\n" +
	"silently. This check is API-discovery only; enforcement can only be proven\n" +
	"by a denied attempt from an in-cluster negative control, never by doctor\n" +
	"--k8s alone.\n\n" +
	"--repo diffs each configured repo's declared forge-policy manifest\n" +
	"(<instance-root>/instance.yaml repos[].policy: required merge method,\n" +
	"merge-queue requirement, required status checks — issue #916, Tier 4 of\n" +
	"#903) against its live GitHub state. Repos with no policy declared are\n" +
	"skipped. Token-scope introspection is reported as unavailable when GitHub\n" +
	"does not expose it (fine-grained PAT / GitHub App tokens) — never inferred\n" +
	"from a failed call. instance-root defaults to \".\".\n\n" +
	"--av-exclusions lists every directory Goobers writes and immediately reads\n" +
	"back — the set real-time antivirus scanning on Windows must exclude, or a\n" +
	"scan holding a handle on a just-written file surfaces minutes later as an\n" +
	"unrelated git \"Permission denied\" (#3480, #3161–#3164). The list is\n" +
	"derived from the same path code the daemon (instance root, run journals,\n" +
	"scheduler ledger, blob store, workcopies, TEMP), the worker (--work-root,\n" +
	"which applies to this mode only, and its workcopies/scratch subtrees) and\n" +
	"a Windows stage pod (C:\\workspace, the tmp:ephemeral TEMP, the container\n" +
	"user's profile) actually use, so it cannot drift from what the binary\n" +
	"writes. Each gaggle's own spec.workcopies.root is enumerated separately,\n" +
	"since it beats the instance-wide one and may name any drive; when config/\n" +
	"cannot be read, that is reported rather than passed off as no gaggles.\n" +
	"On a Windows host it also reads\n" +
	"Microsoft Defender's exclusion list (Get-MpPreference, read-only) and\n" +
	"reports each directory as excluded, not-excluded, or unknown; elsewhere\n" +
	"it lists the set and reports unknown. ADVISORY: exit 0 whatever the\n" +
	"coverage — an organisation-wide AV policy is the operator's to set, and\n" +
	"nothing here changes it. Declare the answer on each windows runner as\n" +
	"provides.windows.avExclusionsVerified (validate warns RNR006 without it).\n\n" +
	"--report json emits the stable machine-readable report; text (default)\n" +
	"prints a human-readable table (--k8s), per-repo findings (--repo), or the\n" +
	"per-directory coverage list (--av-exclusions).\n\n" +
	"Exit codes: 0 = conformant (warns allowed for --k8s; always for\n" +
	"--av-exclusions), 1 = a required check failed or drift was found,\n" +
	"2 = usage/IO error.\n"

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

// runDoctor backs `goobers doctor`. --k8s runs the cluster preflight
// (k8s-infra-shape.md deliverable K3, #668) via internal/k8spreflight and
// renders the conformance report. --repo diffs each configured repo's
// declared forge-policy manifest against its live GitHub state (#916, Tier 4
// of #903). --av-exclusions enumerates the directories Goobers writes then
// reads and, on Windows, verifies them against Defender's exclusion list
// (#3480, advisory). Exactly one mode is required per invocation.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	k8sMode := fs.Bool("k8s", false, "preflight a Kubernetes cluster against docs/design/k8s-infra-shape.md")
	repoMode := fs.Bool("repo", false, "diff declared repo forge-policy manifests against live GitHub state")
	avMode := fs.Bool("av-exclusions", false, "list the directories Goobers writes then reads and verify antivirus exclusions (advisory)")
	workRoot := fs.String("work-root", "", "worker work root to enumerate with --av-exclusions (default: the worker's own default)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: the standard loading rules)")
	kubeContext := fs.String("context", "", "kubeconfig context (default: the current context)")
	reportFormat := fs.String("report", "text", "report format: text or json")
	oidcIssuer := fs.String("oidc-issuer", "", "OIDC issuer URL whose discovery document must be reachable")
	registry := fs.String("registry", "", "container registry host to probe for reachability")
	egress := fs.String("egress", "", "comma-separated host:port outbound targets that must be reachable")
	temporalHostPort := fs.String("temporal-hostport", "", "Temporal frontend host:port whose configured namespace must be registered")
	temporalNamespace := fs.String("temporal-namespace", "", "Temporal namespace to check for (default \"default\")")
	timeout := fs.Duration("timeout", k8spreflight.DefaultTimeout, "per-probe timeout")
	fs.Usage = helpUsage(stderr, "doctor")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *reportFormat != "text" && *reportFormat != "json" {
		pf(stderr, "goobers doctor: --report must be text or json, got %q\n", *reportFormat)
		return 2
	}
	modes := 0
	for _, on := range []bool{*k8sMode, *repoMode, *avMode} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		pf(stderr, "goobers doctor: exactly one of --k8s, --repo or --av-exclusions is required\n\n")
		fs.Usage()
		return 2
	}
	// --work-root belongs to --av-exclusions alone. Parsing it and quietly
	// ignoring it in the other two modes would let an operator believe they
	// had scoped a check they had not, which is the same class of silent
	// mis-report the mode exclusivity above exists to prevent.
	workRootSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "work-root" {
			workRootSet = true
		}
	})
	if workRootSet && !*avMode {
		pf(stderr, "goobers doctor: --work-root applies to --av-exclusions only\n\n")
		fs.Usage()
		return 2
	}

	if *repoMode || *avMode {
		root := "."
		if fs.NArg() == 1 {
			root = fs.Arg(0)
		} else if fs.NArg() > 1 {
			fs.Usage()
			return 2
		}
		if *avMode {
			return runDoctorAVExclusions(root, *workRoot, *reportFormat, stdout, stderr, realAVExclusionDeps())
		}
		return runDoctorRepo(root, *reportFormat, stdout, stderr)
	}

	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	client, host, err := doctorKubeClient(*kubeconfig, *kubeContext, *timeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	report := k8spreflight.Run(context.Background(), client, k8spreflight.Options{
		APIServerEndpoint: host,
		OIDCIssuer:        *oidcIssuer,
		Registry:          *registry,
		Egress:            splitCommaList(*egress),
		TemporalHostPort:  *temporalHostPort,
		TemporalNamespace: *temporalNamespace,
		Timeout:           *timeout,
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

// newDoctorGitHubProvider is a seam so tests can substitute a fake provider.
var newDoctorGitHubProvider = func(token string) providers.PolicyProvider {
	return providers.NewGitHubProvider(token)
}

// doctorRepoReport is one repo's `goobers doctor --repo` result — the stable
// --report json shape.
type doctorRepoReport struct {
	Repo        string                     `json:"repo"`
	Branch      string                     `json:"branch"`
	TokenScope  providers.TokenScopeStatus `json:"tokenScope"`
	TokenScopes []string                   `json:"tokenScopes,omitempty"`
	Findings    []doctorRepoFinding        `json:"findings,omitempty"`
}

// doctorRepoFinding names one declared-vs-live mismatch.
type doctorRepoFinding struct {
	Field    string `json:"field"`
	Declared string `json:"declared"`
	Live     string `json:"live"`
}

// runDoctorRepo diffs every configured repo's declared policy manifest
// (instance.yaml repos[].policy) against its live GitHub state (#916, Tier 4
// of #903). Repos with no policy declared are skipped entirely — this is
// opt-in per repo, not a default expectation.
func runDoctorRepo(root, reportFormat string, stdout, stderr io.Writer) int {
	layout := instance.NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root)\n", layout.ConfigFile())
		return 2
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: load config: %v\n", err)
		return 1
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		pf(stderr, "error: secretStores: %v\n", err)
		return 1
	}

	var reports []doctorRepoReport
	drift := false
	for i, repo := range cfg.Repos {
		if repo.Policy == nil {
			continue
		}
		label := fmt.Sprintf("%s/%s", repo.Owner, repo.Name)
		branch := repo.Policy.Branch
		if branch == "" {
			branch = "main"
		}

		refName := fmt.Sprintf("doctor-repo-%d", i)
		token, err := resolveRepoToken(repo, refName, stores)
		if err != nil {
			pf(stderr, "error: repos[%d] (%s): resolve token: %v\n", i, label, err)
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		live, err := newDoctorGitHubProvider(token).GetRepoPolicy(ctx, providers.RepoPolicyRequest{
			Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name},
			Branch:     branch,
		})
		cancel()
		if err != nil {
			pf(stderr, "error: repos[%d] (%s): fetch live policy: %v\n", i, label, err)
			return 2
		}

		findings := diffRepoPolicy(*repo.Policy, live)
		if len(findings) > 0 {
			drift = true
		}
		reports = append(reports, doctorRepoReport{
			Repo:        label,
			Branch:      branch,
			TokenScope:  live.TokenScope,
			TokenScopes: live.TokenScopes,
			Findings:    findings,
		})
	}

	if reportFormat == "json" {
		if err := json.NewEncoder(stdout).Encode(reports); err != nil {
			pf(stderr, "error: encode report: %v\n", err)
			return 2
		}
	} else {
		writeDoctorRepoText(stdout, reports)
	}
	if drift {
		return 1
	}
	return 0
}

// diffRepoPolicy names every declared expectation the repo's live state does
// not satisfy. An unset expectation imposes no requirement and is skipped.
func diffRepoPolicy(declared instance.RepoPolicyExpectation, live providers.RepoPolicyResult) []doctorRepoFinding {
	var findings []doctorRepoFinding

	if declared.RequiredMergeMethod != "" {
		if allowed := live.AllowedMergeMethods; len(allowed) != 1 || string(allowed[0]) != declared.RequiredMergeMethod {
			findings = append(findings, doctorRepoFinding{
				Field:    "requiredMergeMethod",
				Declared: declared.RequiredMergeMethod,
				Live:     formatMergeMethods(allowed),
			})
		}
	}

	if declared.MergeQueueRequired && live.MergeQueuePolicy != providers.MergePolicyMergeQueue {
		findings = append(findings, doctorRepoFinding{
			Field:    "mergeQueueRequired",
			Declared: "true",
			Live:     string(live.MergeQueuePolicy),
		})
	}

	if missing := missingStatusChecks(declared.RequiredStatusChecks, live.RequiredStatusChecks); len(missing) > 0 {
		findings = append(findings, doctorRepoFinding{
			Field:    "requiredStatusChecks",
			Declared: strings.Join(declared.RequiredStatusChecks, ", "),
			Live:     fmt.Sprintf("missing %s (live: %s)", strings.Join(missing, ", "), strings.Join(live.RequiredStatusChecks, ", ")),
		})
	}

	return findings
}

// missingStatusChecks returns the declared contexts absent from live, sorted.
func missingStatusChecks(declared, live []string) []string {
	liveSet := make(map[string]bool, len(live))
	for _, check := range live {
		liveSet[check] = true
	}
	var missing []string
	for _, check := range declared {
		if !liveSet[check] {
			missing = append(missing, check)
		}
	}
	sort.Strings(missing)
	return missing
}

func formatMergeMethods(methods []providers.MergeMethod) string {
	if len(methods) == 0 {
		return "none allowed"
	}
	strs := make([]string, len(methods))
	for i, method := range methods {
		strs[i] = string(method)
	}
	return strings.Join(strs, ", ")
}

func writeDoctorRepoText(stdout io.Writer, reports []doctorRepoReport) {
	if len(reports) == 0 {
		pln(stdout, "REPO POLICY: no repo declares a policy manifest; nothing to check")
		return
	}
	for _, report := range reports {
		pf(stdout, "REPO %s (branch %s): token-scope=%s", report.Repo, report.Branch, report.TokenScope)
		if len(report.TokenScopes) > 0 {
			pf(stdout, " scopes=%s", strings.Join(report.TokenScopes, ","))
		}
		pln(stdout, "")
		if len(report.Findings) == 0 {
			pln(stdout, "  OK: matches declared policy")
			continue
		}
		for _, finding := range report.Findings {
			pf(stdout, "  DRIFT field=%q declared=%q live=%q\n", finding.Field, finding.Declared, finding.Live)
		}
	}
}
