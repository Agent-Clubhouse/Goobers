package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// githubCodeScanningAlert is the subset of GitHub's code-scanning alert
// payload this intake reads. Fields the nomination path does not use are
// deliberately absent rather than carried as opaque JSON: an alert record is
// untrusted content, and the narrower the projection the less of it reaches a
// prompt.
type githubCodeScanningAlert struct {
	Number          int        `json:"number"`
	HTMLURL         string     `json:"html_url"`
	State           string     `json:"state"`
	DismissedReason string     `json:"dismissed_reason"`
	CreatedAt       *time.Time `json:"created_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
	DismissedAt     *time.Time `json:"dismissed_at"`
	FixedAt         *time.Time `json:"fixed_at"`
	Rule            struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Severity         string `json:"severity"`
		SecuritySeverity string `json:"security_severity_level"`
		Description      string `json:"description"`
	} `json:"rule"`
	Tool struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"tool"`
	MostRecentInstance *struct {
		Ref       string `json:"ref"`
		CommitSHA string `json:"commit_sha"`
		Message   struct {
			Text string `json:"text"`
		} `json:"message"`
		Location struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		} `json:"location"`
	} `json:"most_recent_instance"`
	InstancesURL string `json:"instances_url"`
}

// githubDependabotAlert is the subset of GitHub's Dependabot alert payload
// this intake reads.
type githubDependabotAlert struct {
	Number          int        `json:"number"`
	HTMLURL         string     `json:"html_url"`
	State           string     `json:"state"`
	DismissedReason string     `json:"dismissed_reason"`
	CreatedAt       *time.Time `json:"created_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
	DismissedAt     *time.Time `json:"dismissed_at"`
	FixedAt         *time.Time `json:"fixed_at"`
	Dependency      struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		ManifestPath string `json:"manifest_path"`
		Scope        string `json:"scope"`
		Relationship string `json:"relationship"`
	} `json:"dependency"`
	SecurityAdvisory struct {
		GHSAID      string `json:"ghsa_id"`
		CVEID       string `json:"cve_id"`
		Summary     string `json:"summary"`
		Severity    string `json:"severity"`
		Identifiers []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"identifiers"`
	} `json:"security_advisory"`
	SecurityVulnerability struct {
		Severity               string `json:"severity"`
		VulnerableVersionRange string `json:"vulnerable_version_range"`
		FirstPatchedVersion    *struct {
			Identifier string `json:"identifier"`
		} `json:"first_patched_version"`
	} `json:"security_vulnerability"`
}

// ListSecurityAlerts reads one of GitHub's two native security alert feeds
// (#2984, #2987).
//
// It is a strictly read-only surface, and each feed is backed by its own
// fine-grained permission — "Code scanning alerts: Read-only" and "Dependabot
// alerts: Read-only" — so a scheduled nomination workflow can hold exactly the
// grant it needs and nothing that can write an issue, a pull request, or repo
// contents.
//
// The walk is bounded twice over: per_page is GitHub's own ceiling, and the
// walk stops as soon as req.Limit records are collected rather than draining
// every page. Truncation is visible to the caller as len(alerts) == Limit; a
// caller that must distinguish "exactly Limit" from "at least Limit" raises
// the limit.
func (p *GitHubProvider) ListSecurityAlerts(ctx context.Context, req ListSecurityAlertsRequest) ([]SecurityAlert, error) {
	if err := ValidateListSecurityAlertsRequest(req); err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = DefaultSecurityAlertLimit
	}
	endpoint, err := githubSecurityAlertEndpoint(p.BaseURL, req)
	if err != nil {
		return nil, err
	}

	alerts := make([]SecurityAlert, 0, min(limit, maxPerPage))
	pageErr := p.getAllPages(ctx, endpoint, func(page []byte) error {
		decoded, err := decodeGitHubSecurityAlertPage(req.Source, page)
		if err != nil {
			return err
		}
		for _, alert := range decoded {
			alerts = append(alerts, alert)
			if len(alerts) >= limit {
				return errStopPaging
			}
		}
		return nil
	})
	if pageErr != nil {
		return nil, fmt.Errorf("list %s alerts for %s/%s: %w",
			req.Source, req.Repository.Owner, req.Repository.Name, pageErr)
	}
	SortSecurityAlerts(alerts)
	return alerts, nil
}

func decodeGitHubSecurityAlertPage(source SecurityAlertSource, page []byte) ([]SecurityAlert, error) {
	switch source {
	case SecurityAlertCodeScanning:
		var raw []githubCodeScanningAlert
		if err := json.Unmarshal(page, &raw); err != nil {
			return nil, fmt.Errorf("decode code-scanning alert page: %w", err)
		}
		out := make([]SecurityAlert, 0, len(raw))
		for _, alert := range raw {
			out = append(out, mapGitHubCodeScanningAlert(alert))
		}
		return out, nil
	case SecurityAlertDependabot:
		var raw []githubDependabotAlert
		if err := json.Unmarshal(page, &raw); err != nil {
			return nil, fmt.Errorf("decode Dependabot alert page: %w", err)
		}
		out := make([]SecurityAlert, 0, len(raw))
		for _, alert := range raw {
			out = append(out, mapGitHubDependabotAlert(alert))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported security alert source %q", source)
	}
}

func githubSecurityAlertEndpoint(baseURL string, req ListSecurityAlertsRequest) (string, error) {
	var path []string
	switch req.Source {
	case SecurityAlertCodeScanning:
		path = []string{"repos", req.Repository.Owner, req.Repository.Name, "code-scanning", "alerts"}
	case SecurityAlertDependabot:
		path = []string{"repos", req.Repository.Owner, req.Repository.Name, "dependabot", "alerts"}
	default:
		return "", fmt.Errorf("unsupported security alert source %q", req.Source)
	}
	endpoint, err := joinURL(baseURL, path...)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	// Default to open alerts: the intake nominates live work, so "everything
	// ever reported" must never be the accidental default.
	state := strings.TrimSpace(req.State)
	if state == "" {
		state = "open"
	}
	if !strings.EqualFold(state, "all") {
		values.Set("state", state)
	}
	if len(req.Severity) > 0 {
		values.Set("severity", strings.Join(req.Severity, ","))
	}
	if req.Source == SecurityAlertCodeScanning {
		if req.Tool != "" {
			values.Set("tool_name", req.Tool)
		}
		if req.Ref != "" {
			values.Set("ref", req.Ref)
		}
	}
	if req.Source == SecurityAlertDependabot {
		if len(req.Ecosystem) > 0 {
			values.Set("ecosystem", strings.Join(req.Ecosystem, ","))
		}
		if req.Scope != "" {
			values.Set("scope", req.Scope)
		}
	}
	if len(values) == 0 {
		return endpoint, nil
	}
	return addQuery(endpoint, values)
}

func mapGitHubCodeScanningAlert(alert githubCodeScanningAlert) SecurityAlert {
	detail := &CodeScanningDetail{
		ToolName:        alert.Tool.Name,
		ToolVersion:     alert.Tool.Version,
		RuleID:          alert.Rule.ID,
		RuleName:        alert.Rule.Name,
		RuleDescription: alert.Rule.Description,
	}
	if instance := alert.MostRecentInstance; instance != nil {
		detail.Message = instance.Message.Text
		detail.Path = instance.Location.Path
		detail.StartLine = instance.Location.StartLine
		detail.EndLine = instance.Location.EndLine
		detail.Ref = instance.Ref
		detail.CommitSHA = instance.CommitSHA
	}
	out := SecurityAlert{
		Source:          SecurityAlertCodeScanning,
		Number:          alert.Number,
		URL:             alert.HTMLURL,
		State:           alert.State,
		DismissedReason: alert.DismissedReason,
		// security_severity_level is the CVSS-derived severity GitHub shows in
		// the UI; rule.severity is the tool's own error/warning/note level.
		// Prefer the former and fall back, so a security ranking is used where
		// one exists without dropping alerts that have none.
		Severity:     firstNonBlank(alert.Rule.SecuritySeverity, alert.Rule.Severity),
		CreatedAt:    alert.CreatedAt,
		UpdatedAt:    alert.UpdatedAt,
		DismissedAt:  alert.DismissedAt,
		FixedAt:      alert.FixedAt,
		CodeScanning: detail,
		Integrity:    apiintegrity.Unapproved,
	}
	out.DedupeKey = SecurityAlertDedupeKey(out)
	return out
}

func mapGitHubDependabotAlert(alert githubDependabotAlert) SecurityAlert {
	detail := &DependabotDetail{
		GHSAID:                 alert.SecurityAdvisory.GHSAID,
		CVEID:                  firstNonBlank(alert.SecurityAdvisory.CVEID, githubAdvisoryCVE(alert)),
		Summary:                alert.SecurityAdvisory.Summary,
		Ecosystem:              alert.Dependency.Package.Ecosystem,
		Package:                alert.Dependency.Package.Name,
		ManifestPath:           alert.Dependency.ManifestPath,
		Scope:                  alert.Dependency.Scope,
		Relationship:           alert.Dependency.Relationship,
		VulnerableVersionRange: alert.SecurityVulnerability.VulnerableVersionRange,
	}
	if patched := alert.SecurityVulnerability.FirstPatchedVersion; patched != nil {
		detail.FirstPatchedVersion = patched.Identifier
	}
	out := SecurityAlert{
		Source:          SecurityAlertDependabot,
		Number:          alert.Number,
		URL:             alert.HTMLURL,
		State:           alert.State,
		DismissedReason: alert.DismissedReason,
		Severity:        firstNonBlank(alert.SecurityVulnerability.Severity, alert.SecurityAdvisory.Severity),
		CreatedAt:       alert.CreatedAt,
		UpdatedAt:       alert.UpdatedAt,
		DismissedAt:     alert.DismissedAt,
		FixedAt:         alert.FixedAt,
		Dependabot:      detail,
		Integrity:       apiintegrity.Unapproved,
	}
	out.DedupeKey = SecurityAlertDedupeKey(out)
	return out
}

// githubAdvisoryCVE recovers a CVE from the advisory's identifier list when the
// top-level cve_id is null, which GitHub does for advisories that carry the CVE
// only as an identifier entry.
func githubAdvisoryCVE(alert githubDependabotAlert) string {
	for _, identifier := range alert.SecurityAdvisory.Identifiers {
		if strings.EqualFold(identifier.Type, "CVE") {
			return identifier.Value
		}
	}
	return ""
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
