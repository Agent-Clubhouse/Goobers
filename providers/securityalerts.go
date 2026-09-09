package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// SecurityAlertSource names one provider-native security alert feed. The two
// feeds are separate surfaces with separate permissions on GitHub, so they are
// separate capabilities and separate credentials here too (#2984, #2987).
type SecurityAlertSource string

const (
	// SecurityAlertCodeScanning is the static-analysis alert feed (CodeQL and
	// any other tool uploading SARIF).
	SecurityAlertCodeScanning SecurityAlertSource = "code-scanning"
	// SecurityAlertDependabot is the vulnerable-dependency alert feed.
	SecurityAlertDependabot SecurityAlertSource = "dependabot"
)

// Valid reports whether s names a supported alert feed.
func (s SecurityAlertSource) Valid() bool {
	switch s {
	case SecurityAlertCodeScanning, SecurityAlertDependabot:
		return true
	default:
		return false
	}
}

// SecurityAlertSources returns every supported feed, in declaration order.
func SecurityAlertSources() []SecurityAlertSource {
	return []SecurityAlertSource{SecurityAlertCodeScanning, SecurityAlertDependabot}
}

// ListSecurityAlertsRequest is a bounded read of one alert feed. Every field
// beyond Repository and Source narrows the result; none of them widen it.
type ListSecurityAlertsRequest struct {
	Repository RepositoryRef
	Source     SecurityAlertSource
	// State filters by alert state ("open", "dismissed", "fixed", "resolved",
	// "auto_dismissed", or "all"). Empty means open alerts only: the intake
	// exists to nominate live work, so the default must not be "everything
	// this repository has ever reported".
	State string
	// Severity filters to the named severities. The vocabularies differ per
	// feed (code scanning: critical/high/medium/low/warning/note/error;
	// Dependabot: critical/high/medium/low), and are passed through rather
	// than normalized, so an unsupported value is the provider's error to
	// report and never a silent empty result.
	Severity []string
	// Tool filters code-scanning alerts to one analysis tool ("CodeQL").
	Tool string
	// Ref filters code-scanning alerts to one git ref ("refs/heads/main").
	// This is the field that separates default-branch alerts, which are
	// nomination input, from a PR's own analysis, which is ordinary CI.
	Ref string
	// Ecosystem filters Dependabot alerts to the named package ecosystems.
	Ecosystem []string
	// Scope filters Dependabot alerts to "runtime" or "development"
	// dependencies.
	Scope string
	// Limit caps the number of alerts returned. Zero means
	// DefaultSecurityAlertLimit; the walk stops as soon as the cap is reached
	// rather than draining every page.
	Limit int
}

// Security alert read bounds. A scheduled intake must be bounded by
// construction: an unbounded walk of a large repository's alert history is
// both a rate-limit hazard and an unbounded artifact.
const (
	// DefaultSecurityAlertLimit is the cap applied when a request names none.
	DefaultSecurityAlertLimit = 100
	// MaxSecurityAlertLimit is the largest cap a caller may request.
	MaxSecurityAlertLimit = 1000
)

// CodeScanningDetail carries the code-scanning-specific fields of an alert.
// Every string here is repository- or tool-authored content: see
// SecurityAlert.Integrity.
type CodeScanningDetail struct {
	ToolName        string `json:"toolName,omitempty"`
	ToolVersion     string `json:"toolVersion,omitempty"`
	RuleID          string `json:"ruleId,omitempty"`
	RuleName        string `json:"ruleName,omitempty"`
	RuleDescription string `json:"ruleDescription,omitempty"`
	Message         string `json:"message,omitempty"`
	Path            string `json:"path,omitempty"`
	StartLine       int    `json:"startLine,omitempty"`
	EndLine         int    `json:"endLine,omitempty"`
	Ref             string `json:"ref,omitempty"`
	CommitSHA       string `json:"commitSha,omitempty"`
	// InstanceCount is how many analysis instances (data-flow locations) the
	// provider reports for this alert. It is the number that makes
	// per-location fan-out visible to a nominator without inviting it.
	InstanceCount int `json:"instanceCount,omitempty"`
}

// DependabotDetail carries the Dependabot-specific fields of an alert.
type DependabotDetail struct {
	GHSAID string `json:"ghsaId,omitempty"`
	CVEID  string `json:"cveId,omitempty"`
	// Summary is the advisory's own one-line summary — untrusted advisory text.
	Summary                string `json:"summary,omitempty"`
	Ecosystem              string `json:"ecosystem,omitempty"`
	Package                string `json:"package,omitempty"`
	ManifestPath           string `json:"manifestPath,omitempty"`
	Scope                  string `json:"scope,omitempty"`
	Relationship           string `json:"relationship,omitempty"`
	VulnerableVersionRange string `json:"vulnerableVersionRange,omitempty"`
	FirstPatchedVersion    string `json:"firstPatchedVersion,omitempty"`
}

// SecurityAlert is one provider-native security alert, normalized across the
// two feeds.
//
// INTEGRITY. Every alert-derived string — rule descriptions, analysis
// messages, advisory summaries, file paths, package names — is repository or
// third-party content, so Integrity is always apiintegrity.Unapproved and the
// whole record is data, never instructions. A consumer that renders an alert
// into an agent prompt must carry that grade with it.
type SecurityAlert struct {
	Source SecurityAlertSource `json:"source"`
	// Number is the provider's own per-repository alert number.
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
	// DismissedReason is the provider's reason for a dismissed alert, so a
	// nominator can tell "accepted risk" from "false positive" without
	// re-deriving it from prose.
	DismissedReason string     `json:"dismissedReason,omitempty"`
	Severity        string     `json:"severity,omitempty"`
	CreatedAt       *time.Time `json:"createdAt,omitempty"`
	UpdatedAt       *time.Time `json:"updatedAt,omitempty"`
	DismissedAt     *time.Time `json:"dismissedAt,omitempty"`
	FixedAt         *time.Time `json:"fixedAt,omitempty"`

	CodeScanning *CodeScanningDetail `json:"codeScanning,omitempty"`
	Dependabot   *DependabotDetail   `json:"dependabot,omitempty"`

	// DedupeKey is the stable identity a scheduled run correlates against an
	// existing nomination. See SecurityAlertDedupeKey for what it is made of
	// and, importantly, what it is deliberately not made of.
	DedupeKey string `json:"dedupeKey,omitempty"`

	Integrity apiintegrity.Grade `json:"integrity,omitempty"`
}

// SecurityAlertDedupeKey returns the stable identity of an alert.
//
// The key is deliberately NOT the alert number: a repository re-scanned after
// a rule upgrade can renumber alerts, and a per-number key would then file a
// fresh issue for a defect already tracked. It is also deliberately NOT the
// analysis location: one CodeQL rule firing at twelve data-flow sites is one
// defect, and a per-location key is exactly how a scan turns into twelve
// near-duplicate issues (the #4230 shape).
//
// So: for code scanning, the tool, rule and affected path — the smallest tuple
// that names one defect in one place. For Dependabot, the advisory and the
// package identity, so one advisory against one manifest is one nomination
// however many times it is re-reported.
func SecurityAlertDedupeKey(alert SecurityAlert) string {
	switch alert.Source {
	case SecurityAlertCodeScanning:
		detail := alert.CodeScanning
		if detail == nil {
			return ""
		}
		return securityAlertKey("code-scanning",
			strings.ToLower(detail.ToolName), detail.RuleID, detail.Path)
	case SecurityAlertDependabot:
		detail := alert.Dependabot
		if detail == nil {
			return ""
		}
		advisory := detail.GHSAID
		if advisory == "" {
			advisory = detail.CVEID
		}
		return securityAlertKey("dependabot",
			strings.ToUpper(advisory), strings.ToLower(detail.Ecosystem),
			strings.ToLower(detail.Package), detail.ManifestPath)
	default:
		return ""
	}
}

func securityAlertKey(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			part = "-"
		}
		cleaned = append(cleaned, part)
	}
	return strings.Join(cleaned, ":")
}

// SortSecurityAlerts orders alerts deterministically: severity descending,
// then dedupe key, then alert number. A scheduled artifact that reorders
// between runs is a diff for a nominator to explain away, so the order is
// pinned rather than left to the provider's paging.
func SortSecurityAlerts(alerts []SecurityAlert) {
	sort.SliceStable(alerts, func(i, j int) bool {
		li, lj := securitySeverityRank(alerts[i].Severity), securitySeverityRank(alerts[j].Severity)
		if li != lj {
			return li > lj
		}
		if alerts[i].DedupeKey != alerts[j].DedupeKey {
			return alerts[i].DedupeKey < alerts[j].DedupeKey
		}
		return alerts[i].Number < alerts[j].Number
	})
}

// securitySeverityRank orders the union of both feeds' severity vocabularies.
// An unrecognized severity ranks lowest but is never dropped: an alert the
// ranking does not understand still has to reach the artifact.
func securitySeverityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return 5
	case "high", "error":
		return 4
	case "medium", "moderate", "warning":
		return 3
	case "low", "note":
		return 2
	default:
		return 1
	}
}

// ValidateListSecurityAlertsRequest applies the shared bounds and vocabulary
// checks, so every backend rejects the same request the same way instead of
// each one discovering the problem in its own transport error.
func ValidateListSecurityAlertsRequest(req ListSecurityAlertsRequest) error {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return err
	}
	if !req.Source.Valid() {
		return fmt.Errorf("unsupported security alert source %q (want %s)",
			req.Source, joinSecurityAlertSources())
	}
	if req.Limit < 0 || req.Limit > MaxSecurityAlertLimit {
		return fmt.Errorf("security alert limit %d is out of range (want 1..%d)", req.Limit, MaxSecurityAlertLimit)
	}
	if req.Source == SecurityAlertDependabot && (req.Tool != "" || req.Ref != "") {
		return fmt.Errorf("tool and ref filters apply to code-scanning alerts only")
	}
	if req.Source == SecurityAlertCodeScanning && (len(req.Ecosystem) > 0 || req.Scope != "") {
		return fmt.Errorf("ecosystem and scope filters apply to Dependabot alerts only")
	}
	if req.Scope != "" && req.Scope != "runtime" && req.Scope != "development" {
		return fmt.Errorf("unsupported dependency scope %q (want runtime or development)", req.Scope)
	}
	return nil
}

func joinSecurityAlertSources() string {
	names := make([]string, 0, 2)
	for _, source := range SecurityAlertSources() {
		names = append(names, string(source))
	}
	return strings.Join(names, " or ")
}

// SecurityAlertLister reads a provider's native security alert feeds. It is
// optional (capabilities security.alerts.code-scanning and
// security.alerts.dependabot) and declared per-provider: a forge without the
// feed must fail closed through the Dispatcher rather than return an empty
// list that reads as "no alerts".
type SecurityAlertLister interface {
	ListSecurityAlerts(context.Context, ListSecurityAlertsRequest) ([]SecurityAlert, error)
}
