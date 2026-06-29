package tutor

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

const defaultBranchPrefix = "tutor"

// Planner converts a finding into a concrete config-repo proposal.
type Planner struct {
	BranchPrefix string
	ConfigRoot   string
}

// Propose generates a PR-ready config-as-code change for a finding.
func (p Planner) Propose(f Finding) Proposal {
	prefix := p.BranchPrefix
	if prefix == "" {
		prefix = defaultBranchPrefix
	}
	slug := findingSlug(f)
	title := proposalTitle(f)
	body := proposalBody(f)
	file := proposalFile(f, p.ConfigRoot)
	return Proposal{
		Finding:    f,
		BranchName: strings.Trim(prefix, "/") + "/" + slug,
		Title:      title,
		Body:       body,
		Files:      []providers.CommitFile{file},
	}
}

func proposalTitle(f Finding) string {
	switch f.Type {
	case FindingGateRejection:
		return fmt.Sprintf("Tutor: reduce %s gate rejections", f.GateID)
	case FindingTaskFailure:
		return fmt.Sprintf("Tutor: improve %s task reliability", f.TaskID)
	case FindingSlowTask:
		return fmt.Sprintf("Tutor: reduce %s task latency", f.TaskID)
	case FindingRetries:
		return fmt.Sprintf("Tutor: reduce %s task retries", f.TaskID)
	default:
		return "Tutor: improve Goobers config"
	}
}

func proposalFile(f Finding, configRoot string) providers.CommitFile {
	root := strings.Trim(configRoot, "/")
	if root != "" {
		root += "/"
	}
	if f.GooberID != "" {
		return providers.CommitFile{
			Path:       fmt.Sprintf("%sgaggles/%s/goobers/%s/instructions.md", root, sanitizePathPart(f.Gaggle), sanitizePathPart(f.GooberID)),
			Content:    guidanceContent(f),
			ChangeType: string(providers.CommitChangeEdit),
		}
	}
	return providers.CommitFile{
		Path:       fmt.Sprintf("%sgaggles/%s/workflows/%s.yaml", root, sanitizePathPart(f.Gaggle), sanitizePathPart(f.WorkflowID)),
		Content:    guidanceContent(f),
		ChangeType: string(providers.CommitChangeEdit),
	}
}

func proposalBody(f Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", f.Rationale)
	fmt.Fprintf(&b, "**Recommendation:** %s\n\n", f.Recommendation)
	fmt.Fprintf(&b, "**Target:** `%s`\n", targetLabel(f))
	fmt.Fprintf(&b, "**Severity:** %s\n", f.Severity)
	fmt.Fprintf(&b, "**Evidence:** %d problematic signal(s) out of %d observed.\n", f.ProblemCount, f.Observed)
	if len(f.Evidence) > 0 {
		b.WriteString("\nSample evidence:\n")
		for _, ev := range f.Evidence {
			fmt.Fprintf(&b, "- `%s`", ev.Signal)
			if ev.RunID != "" {
				fmt.Fprintf(&b, " run `%s`", ev.RunID)
			}
			if ev.Decision != "" {
				fmt.Fprintf(&b, " decision `%s`", ev.Decision)
			}
			if ev.Status != "" {
				fmt.Fprintf(&b, " status `%s`", ev.Status)
			}
			if ev.Duration > 0 {
				fmt.Fprintf(&b, " duration `%s`", ev.Duration)
			}
			if ev.RetryCount > 0 {
				fmt.Fprintf(&b, " retries `%d`", ev.RetryCount)
			}
			if ev.URL != "" {
				fmt.Fprintf(&b, " evidence %s", ev.URL)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func guidanceContent(f Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n<!-- goober-tutor:%s -->\n", findingSlug(f))
	fmt.Fprintf(&b, "## Tutor-added guidance: %s\n\n", targetLabel(f))
	fmt.Fprintf(&b, "%s\n\n", f.Recommendation)
	fmt.Fprintf(&b, "## Why this guidance was added\n\n%s\n\n", f.Rationale)
	appendEvidence(&b, f)
	b.WriteString("<!-- /goober-tutor -->\n")
	return b.String()
}

func appendEvidence(b *strings.Builder, f Finding) {
	if len(f.Evidence) > 0 {
		b.WriteString("## Telemetry evidence\n\n")
		for _, ev := range f.Evidence {
			fmt.Fprintf(b, "- %s", ev.Signal)
			if ev.RunID != "" {
				fmt.Fprintf(b, " in run `%s`", ev.RunID)
			}
			if ev.Decision != "" {
				fmt.Fprintf(b, " returned `%s`", ev.Decision)
			}
			if ev.Status != "" {
				fmt.Fprintf(b, " status `%s`", ev.Status)
			}
			if ev.Duration > 0 {
				fmt.Fprintf(b, " after `%s`", ev.Duration)
			}
			if ev.RetryCount > 0 {
				fmt.Fprintf(b, " with `%d` retries", ev.RetryCount)
			}
			if ev.URL != "" {
				fmt.Fprintf(b, " (%s)", ev.URL)
			}
			b.WriteByte('\n')
		}
	}
}

func targetLabel(f Finding) string {
	switch {
	case f.GateID != "":
		return fmt.Sprintf("workflow/%s gate/%s", f.WorkflowID, f.GateID)
	case f.TaskID != "":
		if f.GooberID != "" {
			return fmt.Sprintf("workflow/%s task/%s goober/%s", f.WorkflowID, f.TaskID, f.GooberID)
		}
		return fmt.Sprintf("workflow/%s task/%s", f.WorkflowID, f.TaskID)
	default:
		return "workflow/" + f.WorkflowID
	}
}

func findingSlug(f Finding) string {
	parts := []string{string(f.Type), f.WorkflowID}
	if f.TaskID != "" {
		parts = append(parts, f.TaskID)
	}
	if f.GateID != "" {
		parts = append(parts, f.GateID)
	}
	return sanitizePathPart(strings.Join(parts, "-"))
}

func sanitizePathPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
