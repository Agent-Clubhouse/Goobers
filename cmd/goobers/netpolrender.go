package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/netpolrender"
)

const netpolRenderHelp = "Usage: goobers netpol-render [--out <dir>] [--check] [--baseline <path>]\n" +
	"                             [--write-baseline] [--timeout <duration>]\n" +
	"                             [--print-blob-endpoint] [instance-root]\n\n" +
	"Render the per-runner-class NetworkPolicy reference manifests from the\n" +
	"runners: inventory — the decision-016 single source of the network reference\n" +
	"manifests (issue #3568, docs/design/goobernetes-restrictions.md §6/§7).\n\n" +
	"One manifest set is emitted per DISTINCT restriction set (runner class). Every\n" +
	"policy selects on goobers.dev/runner-class derived by the SAME shared function\n" +
	"the dispatcher stamps stage pods with, so selector and stamp agree by\n" +
	"construction. Each policy also carries the goobers.dev/runner-class-restrictions\n" +
	"ANNOTATION — the human-readable restriction set behind the (possibly opaque)\n" +
	"class value, so `kubectl get netpol -o yaml` answers \"which class is this\".\n\n" +
	"Per class: a network:none class gets only DNS and the blob-endpoint data path;\n" +
	"every other class additionally gets the instance-configured egress.allowlist\n" +
	"CIDR groups (instance.yaml egress: — operator-supplied; the render REFUSES\n" +
	"CHANGE-ME documentation placeholders rather than emitting a stub). Every class,\n" +
	"restricted included, carries the blob-endpoint egress row: it is the class's\n" +
	"own artifact data path, and each cross-namespace grant is composed as\n" +
	"namespaceSelector AND podSelector in a single peer element.\n\n" +
	"--out writes one file per class plus a kustomization.yaml; without it the\n" +
	"manifests stream to stdout.\n\n" +
	"--check validates instead of writing:\n" +
	"  - provenance drift: every egress.allowlist group with a source URL is\n" +
	"    re-fetched and EVERY sourceSHA256 marker compared (a stale CIDR set\n" +
	"    otherwise fails mid-run as a connect timeout indistinguishable from a\n" +
	"    correct denial);\n" +
	"  - coverage ratchet: per-class model-endpoint coverage, measured in\n" +
	"    ADDRESSES (never CIDR-block counts), compared against the committed\n" +
	"    baseline — failing on a rise or an unfrozen class;\n" +
	"  - output freshness: with --out, the on-disk manifests must match a fresh\n" +
	"    render.\n\n" +
	"--write-baseline freezes the current per-class coverage into the baseline\n" +
	"file (--baseline; defaults to <out>/coverage-baseline.json).\n\n" +
	"--print-blob-endpoint prints the blob endpoint (namespace, pod labels,\n" +
	"container port) as JSON and exits, touching no cluster and needing no instance\n" +
	"root. The goobers-system ingress half of the blob grant (#3585) lives in a\n" +
	"separate repo and renders its ingress peer FROM this value rather than\n" +
	"restating it, so the endpoint is derived once and cannot drift.\n\n" +
	"The rendered manifests are REFERENCE manifests the cluster operator applies;\n" +
	"this command never touches a cluster, and rendering says nothing about\n" +
	"enforcement — `goobers doctor --k8s` owns enforcement honesty (D12).\n\n" +
	"An instance with no pod-hosted runners (no runners: block, or self-only)\n" +
	"renders nothing and exits 0.\n\n" +
	"Exit codes: 0 = OK, 1 = refusal or check failure, 2 = usage/IO error.\n"

// netpolRenderFetch fetches one provenance source document — a seam so tests
// substitute the transport.
var netpolRenderFetch = func(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, 8<<20))
}

// runNetpolRender backs `goobers netpol-render`.
func runNetpolRender(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("netpol-render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("out", "", "directory to write the rendered manifest set into (default: stdout)")
	check := fs.Bool("check", false, "validate provenance markers, the coverage ratchet, and (with --out) output freshness instead of writing")
	baselinePath := fs.String("baseline", "", "coverage baseline file (default: <out>/coverage-baseline.json)")
	writeBaseline := fs.Bool("write-baseline", false, "freeze the current per-class model-endpoint coverage into the baseline file")
	timeout := fs.Duration("timeout", 30*time.Second, "per-fetch timeout for provenance --check")
	printBlobEndpoint := fs.Bool("print-blob-endpoint", false, "print the blob endpoint (namespace, pod labels, container port) as JSON and exit; a downstream goobers-system base renders its ingress half (#3585) FROM this value instead of restating it — so a change to the endpoint propagates rather than drifting")
	fs.Usage = helpUsage(stderr, "netpol-render")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *printBlobEndpoint {
		return printBlobEndpointJSON(stdout, stderr)
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	} else if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *check && *writeBaseline {
		pf(stderr, "goobers netpol-render: --check and --write-baseline are exclusive — a check that rewrites its own baseline checks nothing\n")
		return 2
	}
	baseline := *baselinePath
	if baseline == "" && *outDir != "" {
		baseline = filepath.Join(*outDir, "coverage-baseline.json")
	}
	if (*check || *writeBaseline) && baseline == "" {
		pf(stderr, "goobers netpol-render: --check/--write-baseline need a baseline file — pass --baseline or --out\n")
		return 2
	}

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

	input := netpolRenderInput(cfg)
	if len(input.Runners) == 0 {
		pln(stdout, "netpol-render: no pod-hosted runners declared; nothing to render (zero-declaration invariance)")
		return 0
	}

	result, err := netpolrender.Render(input)
	if err != nil {
		pf(stderr, "error: render: %v\n", err)
		return 1
	}
	coverage, err := netpolrender.ModelEndpointCoverage(result.Classes, input.Allowlist)
	if err != nil {
		pf(stderr, "error: coverage: %v\n", err)
		return 1
	}

	if *check {
		return runNetpolCheck(input, result, coverage, baseline, *outDir, *timeout, stdout, stderr)
	}

	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		for _, file := range result.Files {
			if err := os.WriteFile(filepath.Join(*outDir, file.Name), file.Content, 0o644); err != nil {
				pf(stderr, "error: %v\n", err)
				return 2
			}
		}
		pf(stdout, "netpol-render: wrote %d file(s) for %d runner class(es) to %s\n", len(result.Files), len(result.Classes), *outDir)
	} else {
		for i, file := range result.Files {
			if file.Name == "kustomization.yaml" {
				continue
			}
			if i > 0 {
				pln(stdout, "---")
			}
			_, _ = stdout.Write(file.Content)
		}
	}

	if *writeBaseline {
		raw, err := netpolrender.MarshalBaseline(netpolrender.NewBaseline(result.Classes, coverage))
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		if err := os.WriteFile(baseline, raw, 0o644); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		pf(stdout, "netpol-render: froze per-class model-endpoint coverage (in addresses) into %s\n", baseline)
	}
	for _, class := range result.Classes {
		pf(stdout, "  class %s [%s] model-endpoint coverage: %s addresses\n", class.Value, class.Preimage, coverage[class.Value])
	}
	return 0
}

// runNetpolCheck is the --check mode: provenance drift (every marker),
// coverage ratchet (addresses, fail on rise), and output freshness.
func runNetpolCheck(input netpolrender.Input, result *netpolrender.Result, coverage map[string]*big.Int, baselinePath, outDir string, timeout time.Duration, stdout, stderr io.Writer) int {
	failed := false

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	mismatches, unverifiable := netpolrender.CheckProvenance(ctx, netpolrender.Fetcher(netpolRenderFetch), input.Allowlist)
	for _, name := range unverifiable {
		pf(stdout, "note: egress.allowlist group %q declares no source — provenance unverifiable, not checked\n", name)
	}
	if len(mismatches) > 0 {
		failed = true
		pf(stderr, "provenance drift (%d marker(s) — every marker is validated, not just the first):\n", len(mismatches))
		for _, m := range mismatches {
			pf(stderr, "  %s\n", m.String())
		}
	} else {
		pf(stdout, "provenance: %d verifiable marker(s) current\n", len(input.Allowlist)-len(unverifiable))
	}

	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		failed = true
		pf(stderr, "coverage ratchet: cannot read baseline %s: %v — a missing baseline never passes silently; "+
			"freeze one deliberately with --write-baseline\n", baselinePath, err)
	} else if parsed, err := netpolrender.ParseBaseline(raw); err != nil {
		failed = true
		pf(stderr, "coverage ratchet: %v\n", err)
	} else {
		notes, err := netpolrender.CheckBaseline(parsed, result.Classes, coverage)
		for _, note := range notes {
			pf(stdout, "note: %s\n", note)
		}
		if err != nil {
			failed = true
			pf(stderr, "%v\n", err)
		} else {
			pf(stdout, "coverage ratchet: %d class(es) at or under baseline (unit: %s)\n", len(result.Classes), netpolrender.BaselineUnit)
		}
	}

	if outDir != "" {
		for _, file := range result.Files {
			onDisk, err := os.ReadFile(filepath.Join(outDir, file.Name))
			if err != nil || string(onDisk) != string(file.Content) {
				failed = true
				pf(stderr, "stale output: %s does not match a fresh render — re-run `goobers netpol-render --out %s`\n",
					filepath.Join(outDir, file.Name), outDir)
			}
		}
	}

	if failed {
		return 1
	}
	pln(stdout, "netpol-render --check: OK")
	return 0
}

// netpolRenderInput distills the loaded config into the renderer's input:
// pod-hosted runners only (a host:"self" entry never gets a pod, so it never
// gets a class policy — and an instance with no runners: block resolves to
// the implicit self entry, rendering nothing), plus the operator-supplied
// egress allowlist.
// printBlobEndpointJSON writes the blob endpoint (decision 010/012) as
// machine-readable JSON and exits — no cluster touch, no instance root needed.
// The goobers-system ingress half of the blob grant (#3585) lives in a
// separate repo that cannot import netpolrender.DefaultBlobEndpoint; it renders
// its ingress peer FROM this output, so the endpoint is derived once and
// propagates rather than being restated (and drifting — decision 015/016).
func printBlobEndpointJSON(stdout, stderr io.Writer) int {
	ep := netpolrender.DefaultBlobEndpoint()
	out := struct {
		Namespace string            `json:"namespace"`
		PodLabels map[string]string `json:"podLabels"`
		Port      int               `json:"port"`
	}{Namespace: ep.Namespace, PodLabels: ep.PodLabels, Port: ep.Port}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		pf(stderr, "goobers netpol-render: marshal blob endpoint: %v\n", err)
		return 1
	}
	pln(stdout, string(data))
	return 0
}

func netpolRenderInput(cfg *instance.Config) netpolrender.Input {
	var input netpolrender.Input
	for _, entry := range cfg.ResolvedRunners() {
		kind, err := instance.ClassifyRunnerHost(entry.Host)
		if err != nil || kind == instance.RunnerHostSelf {
			continue
		}
		restrictions := make([]string, len(entry.Restrictions))
		for i, r := range entry.Restrictions {
			restrictions[i] = string(r)
		}
		input.Runners = append(input.Runners, netpolrender.Runner{Name: entry.Name, Restrictions: restrictions})
	}
	if cfg.Egress != nil {
		for _, group := range cfg.Egress.Allowlist {
			input.Allowlist = append(input.Allowlist, netpolrender.AllowlistGroup{
				Name:         group.Name,
				Kind:         group.Kind,
				Source:       group.Source,
				SourceSHA256: group.SourceSHA256,
				CIDRs:        group.CIDRs,
				Ports:        group.Ports,
			})
		}
	}
	return input
}
