package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

const securityAlertsResultFile = "security-alerts.json"

const securityAlertsQueryHelp = "Usage: goobers security-alerts-query --source code-scanning|dependabot " +
	"[--state <state>] [--severity <list>] [--tool <name>] [--ref <ref>] [--ecosystem <list>] " +
	"[--scope runtime|development] [--max-results <n>] [path]\n\n" +
	"Read one of the repository's native security alert feeds and emit a bounded,\n" +
	"schema-validated artifact for work nomination (a connector stage). It is\n" +
	"strictly read-only: it never files, comments on, or labels an issue.\n\n" +
	"Each source is backed by its own least-privilege capability, so a stage sees\n" +
	"one feed and nothing else:\n" +
	"  code-scanning  github:code-scanning:read      (PAT: Code scanning alerts: Read-only)\n" +
	"  dependabot     github:dependabot-alerts:read  (PAT: Dependabot alerts: Read-only)\n\n" +
	"Every alert-derived string — rule text, analysis messages, advisory summaries,\n" +
	"paths, package names — is repository or third-party content. The artifact\n" +
	"records integrity `unapproved` for the whole set: it is evidence for a\n" +
	"nominator to reason about, never instructions to follow.\n\n" +
	"Each alert carries a stable `dedupeKey` so repeated scheduled runs correlate\n" +
	"with an existing nomination instead of filing one issue per scan. For code\n" +
	"scanning the key is tool:rule:path, so one rule firing at many data-flow\n" +
	"locations is one defect; for Dependabot it is advisory:ecosystem:package:manifest.\n\n" +
	"--ref narrows code-scanning alerts to one git ref. Default-branch alerts are\n" +
	"what this intake nominates; a CodeQL failure on an already-open implementation\n" +
	"PR is ordinary CI and is handled by the ci-poll/repass path, not here.\n\n" +
	"Exit codes: 0 = artifact written, 1 = config/credential/provider error,\n" +
	"2 = usage error.\n"

// securityAlertsFilters echoes the bounds actually applied, so an artifact
// explains its own scope rather than leaving a reader to infer it from what is
// absent.
type securityAlertsFilters struct {
	State      string   `json:"state,omitempty"`
	Severity   []string `json:"severity,omitempty"`
	Tool       string   `json:"tool,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Ecosystem  []string `json:"ecosystem,omitempty"`
	Scope      string   `json:"scope,omitempty"`
	MaxResults int      `json:"maxResults,omitempty"`
}

type securityAlertsArtifact struct {
	Schema     string                    `json:"schema"`
	Source     string                    `json:"source"`
	Repository string                    `json:"repository"`
	QueriedAt  string                    `json:"queriedAt"`
	Filters    *securityAlertsFilters    `json:"filters,omitempty"`
	Truncated  bool                      `json:"truncated"`
	Integrity  apiintegrity.Grade        `json:"integrity"`
	NoWork     bool                      `json:"noWork"`
	Alerts     []providers.SecurityAlert `json:"alerts"`
}

const securityAlertsSchemaID = "goobers.dev/security-alerts/v1"

// securityAlertCapabilities maps each feed to the least-privilege capability
// that reads it. They are separate because GitHub's permissions are separate:
// a workflow that nominates dependency work must not thereby gain the ability
// to read static-analysis findings, and neither grant authorizes any write.
var securityAlertCapabilities = map[providers.SecurityAlertSource]capability.Capability{
	providers.SecurityAlertCodeScanning: capability.GitHubCodeScanningRead,
	providers.SecurityAlertDependabot:   capability.GitHubDependabotAlertsRead,
}

func runSecurityAlertsQuery(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("security-alerts-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "security-alerts-query")
	var (
		source     = fs.String("source", providerInput("source", ""), "alert feed to read (code-scanning or dependabot)")
		state      = fs.String("state", providerInput("state", ""), "alert state filter (default open)")
		severity   = fs.String("severity", providerInput("severity", ""), "comma-separated severity filter")
		tool       = fs.String("tool", providerInput("tool", ""), "code-scanning analysis tool name")
		ref        = fs.String("ref", providerInput("ref", ""), "code-scanning git ref")
		ecosystem  = fs.String("ecosystem", providerInput("ecosystem", ""), "comma-separated Dependabot package ecosystems")
		scope      = fs.String("scope", providerInput("scope", ""), "Dependabot dependency scope (runtime or development)")
		maxResults = fs.String("max-results", providerInput("maxResults", ""), "maximum alerts to collect")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	req, err := securityAlertsRequest(*source, *state, *severity, *tool, *ref, *ecosystem, *scope, *maxResults)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	repo, err := providerRepo(root)
	if err != nil {
		// Routed through failProviderStage, not a bare print: an early config
		// failure must overwrite a stale result file with a typed error, or
		// the runner reads the previous tick's success as this tick's.
		return failProviderStage(stderr, "resolve target repository", err, securityAlertsResultFile)
	}
	req.Repository = backlogRepoRefForStage(root, repo)
	// Validate the whole request against the shared contract before a
	// credential is resolved, so a mistyped filter is a usage error rather
	// than a provider round-trip that fails halfway.
	if err := providers.ValidateListSecurityAlertsRequest(req); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	lister, err := securityAlertProvider(root, repo, req.Source)
	if err != nil {
		return failProviderStage(stderr,
			fmt.Sprintf("resolve %s alert credential", req.Source), err, securityAlertsResultFile)
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	alerts, err := lister.ListSecurityAlerts(ctx, req)
	if err != nil {
		return failProviderStage(stderr,
			fmt.Sprintf("list %s alerts", req.Source), err, securityAlertsResultFile)
	}
	if alerts == nil {
		alerts = []providers.SecurityAlert{}
	}

	artifact := securityAlertsArtifact{
		Schema:     securityAlertsSchemaID,
		Source:     string(req.Source),
		Repository: req.Repository.Owner + "/" + req.Repository.Name,
		QueriedAt:  time.Now().UTC().Format(time.RFC3339),
		Filters:    securityAlertsFiltersFor(req),
		// The feed returned exactly as many alerts as the cap allows, so the
		// artifact may not be the complete set. Reported rather than inferred:
		// a nominator that silently treats a truncated read as complete would
		// conclude a fixed alert had disappeared.
		Truncated: len(alerts) >= effectiveSecurityAlertLimit(req),
		Integrity: apiintegrity.Unapproved,
		NoWork:    len(alerts) == 0,
		Alerts:    alerts,
	}
	if code := writeJSONArtifactWithSchema(artifact, schemas.SecurityAlerts, stdout, stderr); code != 0 {
		return code
	}
	pf(stderr, "read %d open %s alert(s) for %s\n", len(alerts), req.Source, artifact.Repository)
	return 0
}

func effectiveSecurityAlertLimit(req providers.ListSecurityAlertsRequest) int {
	if req.Limit > 0 {
		return req.Limit
	}
	return providers.DefaultSecurityAlertLimit
}

func securityAlertsFiltersFor(req providers.ListSecurityAlertsRequest) *securityAlertsFilters {
	state := req.State
	if state == "" {
		state = "open"
	}
	return &securityAlertsFilters{
		State:      state,
		Severity:   req.Severity,
		Tool:       req.Tool,
		Ref:        req.Ref,
		Ecosystem:  req.Ecosystem,
		Scope:      req.Scope,
		MaxResults: effectiveSecurityAlertLimit(req),
	}
}

func securityAlertsRequest(source, state, severity, tool, ref, ecosystem, scope, maxResults string) (providers.ListSecurityAlertsRequest, error) {
	req := providers.ListSecurityAlertsRequest{
		Source:    providers.SecurityAlertSource(strings.TrimSpace(source)),
		State:     strings.TrimSpace(state),
		Severity:  splitCommaList(severity),
		Tool:      strings.TrimSpace(tool),
		Ref:       strings.TrimSpace(ref),
		Ecosystem: splitCommaList(ecosystem),
		Scope:     strings.TrimSpace(scope),
	}
	if req.Source == "" {
		return req, fmt.Errorf("--source is required (code-scanning or dependabot)")
	}
	if raw := strings.TrimSpace(maxResults); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > providers.MaxSecurityAlertLimit {
			return req, fmt.Errorf("invalid max-results %q (want an integer between 1 and %d)",
				raw, providers.MaxSecurityAlertLimit)
		}
		req.Limit = n
	}
	return req, nil
}

// securityAlertSurfaces maps each feed to the PROVIDER capability that backs
// it, so a backend is refused on its own declaration rather than on a Go type
// assertion after a credential has already been resolved.
var securityAlertSurfaces = map[providers.SecurityAlertSource]providers.Capability{
	providers.SecurityAlertCodeScanning: providers.CapSecurityAlertsCodeScanning,
	providers.SecurityAlertDependabot:   providers.CapSecurityAlertsDependabot,
}

// securityAlertProvider resolves the feed-specific capability-scoped
// credential.
//
// It refuses a backend that does not DECLARE the surface before it builds
// anything, so the diagnosis names the provider and the feed instead of
// whatever the unrelated construction happens to fail on first. Returning an
// empty alert list instead would be worse than either: a nominator cannot tell
// it apart from "this repository has no alerts", which is exactly the
// fail-open answer the capability model exists to prevent.
func securityAlertProvider(root string, repo providers.RepositoryRef, source providers.SecurityAlertSource) (providers.SecurityAlertLister, error) {
	cap, ok := securityAlertCapabilities[source]
	if !ok {
		return nil, fmt.Errorf("unsupported security alert source %q", source)
	}
	declared, known := providers.CapabilitiesFor(repo.Provider)
	if !known || !declared.Has(securityAlertSurfaces[source]) {
		return nil, fmt.Errorf(
			"repository provider %q does not support %s alert intake (capability %s is undeclared); "+
				"route this workflow at a GitHub repository",
			repo.Provider, source, securityAlertSurfaces[source])
	}
	provider, err := newProviderForStage(root, repo, true,
		withStageProviderCapability(cap), withStageProviderCache())
	if err != nil {
		return nil, err
	}
	lister, ok := provider.(providers.SecurityAlertLister)
	if !ok {
		// A provider that declares the capability but does not implement the
		// surface is an internal inconsistency, not an operator error.
		return nil, fmt.Errorf(
			"repository provider %q declares %s but does not implement %s alert intake",
			repo.Provider, securityAlertSurfaces[source], source)
	}
	return lister, nil
}
