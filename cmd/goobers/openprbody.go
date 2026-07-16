package main

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const maxLocalCIOutputBytes = 12 * 1024

type prBodyReview struct {
	verdict    apiv1.Verdict
	outcome    string
	target     string
	diffDigest string
}

type prBodyCI struct {
	status string
	output string
}

type prBodyChange struct {
	path      string
	additions int
	deletions int
}

type journalArtifact struct {
	name string
	ref  journal.Ref
}

// renderStructuredPRBody projects the implementation run's existing journal
// evidence into a reviewer-facing PR body. Workflows without reviewer or
// local-ci evidence keep open-pr's generic fallback.
func renderStructuredPRBody(root, runID, issueID, issueTitle string) (string, bool, error) {
	runDir := filepath.Join(layoutFor(root).RunsDir(), runID)
	if _, err := os.Stat(runDir); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		return "", false, err
	}
	identity, err := rd.Identity()
	if err != nil {
		return "", false, err
	}
	events, err := rd.Events()
	if err != nil {
		return "", false, err
	}

	var (
		artifacts  []journalArtifact
		issueBody  string
		reviews    []prBodyReview
		latestCI   *journal.Event
		latestDiff []byte
	)
	for i := range events {
		ev := &events[i]
		if ev.Type == journal.EventArtifactRecorded && ev.Ref != nil {
			artifacts = append(artifacts, journalArtifact{name: ev.Name, ref: *ev.Ref})
		}
		if issueBody == "" && ev.Type == journal.EventStageFinished && ev.Outputs != nil {
			if id, ok := ev.Outputs["id"].(string); ok && id == issueID {
				issueBody, _ = ev.Outputs["body"].(string)
			}
		}
		if ev.Type == journal.EventStageFinished && ev.Stage == "local-ci" {
			latestCI = ev
		}
		if ev.Type != journal.EventGateEvaluated || ev.Gate != "review" {
			continue
		}
		review := prBodyReview{outcome: ev.Verdict, target: ev.Target}
		if ev.Runner != nil {
			review.diffDigest, _ = ev.Runner["diffDigest"].(string)
		}
		if ev.Ref != nil {
			data, readErr := rd.ArtifactBytes(*ev.Ref)
			if readErr != nil {
				return "", false, fmt.Errorf("read reviewer verdict %q: %w", ev.Name, readErr)
			}
			if unmarshalErr := json.Unmarshal(data, &review.verdict); unmarshalErr != nil {
				return "", false, fmt.Errorf("unmarshal reviewer verdict %q: %w", ev.Name, unmarshalErr)
			}
		}
		reviews = append(reviews, review)
	}

	if len(reviews) == 0 && latestCI == nil {
		return "", false, nil
	}

	if len(reviews) > 0 {
		digest := reviews[len(reviews)-1].diffDigest
		if digest != "" {
			ref, ok := artifactByDigest(artifacts, digest)
			if !ok {
				return "", false, fmt.Errorf("reviewed diff artifact %s not found", digest)
			}
			latestDiff, err = rd.ArtifactBytes(ref)
			if err != nil {
				return "", false, fmt.Errorf("read reviewed diff artifact: %w", err)
			}
		}
	}

	var ci *prBodyCI
	if latestCI != nil {
		ci = &prBodyCI{status: latestCI.Status}
		if ref, ok := stageArtifactByName(artifacts, latestCI.Artifacts, runID+":local-ci/stdout.log"); ok {
			output, readErr := rd.ArtifactBytes(ref)
			if readErr != nil {
				return "", false, fmt.Errorf("read local-ci stdout artifact: %w", readErr)
			}
			ci.output = localCICaseOutput(output)
		}
	}

	return formatStructuredPRBody(issueID, issueTitle, issueBody, identity.WorkflowDigest, reviews, parseUnifiedDiff(latestDiff), ci), true, nil
}

func artifactByDigest(artifacts []journalArtifact, digest string) (journal.Ref, bool) {
	for i := len(artifacts) - 1; i >= 0; i-- {
		if artifacts[i].ref.Digest == digest {
			return artifacts[i].ref, true
		}
	}
	return journal.Ref{}, false
}

func stageArtifactByName(artifacts []journalArtifact, stageRefs []journal.Ref, name string) (journal.Ref, bool) {
	for _, stageRef := range stageRefs {
		for i := len(artifacts) - 1; i >= 0; i-- {
			if artifacts[i].name == name && artifacts[i].ref.Digest == stageRef.Digest {
				return stageRef, true
			}
		}
	}
	return journal.Ref{}, false
}

func formatStructuredPRBody(issueID, issueTitle, issueBody, workflowDigest string, reviews []prBodyReview, changes []prBodyChange, ci *prBodyCI) string {
	var b strings.Builder
	latest := prBodyReview{}
	if len(reviews) > 0 {
		latest = reviews[len(reviews)-1]
	}

	b.WriteString("## Summary\n\n")
	if issueID != "" {
		fmt.Fprintf(&b, "Implements #%s: **%s**.\n", html.EscapeString(issueID), html.EscapeString(issueTitle))
	}
	if summary := strings.TrimSpace(latest.verdict.Summary); summary != "" {
		if issueID != "" {
			b.WriteString("\n")
		}
		b.WriteString(summary)
		b.WriteString("\n")
	}
	if criteria := markdownSection(issueBody, "acceptance criteria"); criteria != "" {
		b.WriteString("\n<details>\n<summary>Acceptance criteria</summary>\n\n")
		b.WriteString(criteria)
		b.WriteString("\n\n</details>\n")
	}

	b.WriteString("\n## Changes\n\n")
	if len(changes) == 0 {
		b.WriteString("No file-level diff was recorded.\n")
	} else {
		totalAdditions, totalDeletions := 0, 0
		for _, change := range changes {
			totalAdditions += change.additions
			totalDeletions += change.deletions
			fmt.Fprintf(&b, "- <code>%s</code> (+%d / -%d)\n", html.EscapeString(change.path), change.additions, change.deletions)
		}
		fmt.Fprintf(&b, "\n**Total:** +%d / -%d across %d file(s).\n", totalAdditions, totalDeletions, len(changes))
	}

	b.WriteString("\n## Testing\n\n")
	if ci == nil {
		b.WriteString("No local-ci result was recorded.\n")
	} else {
		fmt.Fprintf(&b, "**local-ci:** `%s`\n", html.EscapeString(ci.status))
		if ci.output != "" {
			b.WriteString("\n<details>\n<summary>local-ci cases</summary>\n\n<pre>")
			b.WriteString(html.EscapeString(ci.output))
			b.WriteString("</pre>\n\n</details>\n")
		}
	}

	b.WriteString("\n## Reviewer verdict\n\n")
	if len(reviews) == 0 {
		b.WriteString("No reviewer verdict was recorded.\n")
	} else {
		decision := string(latest.verdict.Decision)
		if decision == "" {
			decision = latest.outcome
		}
		fmt.Fprintf(&b, "**Decision:** `%s`\n", html.EscapeString(decision))
		if rationale := strings.TrimSpace(latest.verdict.Rationale); rationale != "" {
			b.WriteString("\n")
			b.WriteString(rationale)
			b.WriteString("\n")
		}
		for _, finding := range latest.verdict.Findings {
			fmt.Fprintf(&b, "\n- **%s:** %s", html.EscapeString(string(finding.Severity)), finding.Message)
			if finding.Location != "" {
				fmt.Fprintf(&b, " (%s)", html.EscapeString(finding.Location))
			}
		}
		if len(latest.verdict.Findings) > 0 {
			b.WriteString("\n")
		}
		if len(reviews) > 1 {
			fmt.Fprintf(&b, "\n<details>\n<summary>Review -&gt; repass history (%d attempts)</summary>\n\n", len(reviews))
			for i, review := range reviews {
				decision := string(review.verdict.Decision)
				if decision == "" {
					decision = review.outcome
				}
				fmt.Fprintf(&b, "%d. `%s`", i+1, html.EscapeString(decision))
				if review.target != "" {
					fmt.Fprintf(&b, " -> `%s`", html.EscapeString(review.target))
				}
				if summary := compactProse(review.verdict.Summary); summary != "" {
					fmt.Fprintf(&b, " - %s", summary)
				}
				if rationale := compactProse(review.verdict.Rationale); rationale != "" {
					fmt.Fprintf(&b, " Rationale: %s", rationale)
				}
				b.WriteString("\n")
			}
			b.WriteString("\n</details>\n")
		}
	}

	b.WriteString("\n---\n")
	if issueID != "" {
		fmt.Fprintf(&b, "Fixes #%s", html.EscapeString(issueID))
	}
	digest, label := latest.diffDigest, "Reviewed diff"
	if digest == "" {
		digest, label = workflowDigest, "Workflow digest"
	}
	if digest != "" {
		if issueID != "" {
			b.WriteString("  \n")
		}
		fmt.Fprintf(&b, "%s: `%s`", label, html.EscapeString(digest))
	}

	return strings.TrimSpace(b.String())
}

func parseUnifiedDiff(diff []byte) []prBodyChange {
	if len(diff) == 0 {
		return nil
	}
	var (
		changes []prBodyChange
		current *prBodyChange
	)
	flush := func() {
		if current != nil {
			changes = append(changes, *current)
		}
	}
	for _, line := range strings.Split(strings.ToValidUTF8(string(diff), "\uFFFD"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			current = &prBodyChange{path: diffGitPath(line)}
		case current == nil:
			continue
		case strings.HasPrefix(line, "rename to "):
			current.path = strings.TrimSpace(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "--- "):
			if path := diffHeaderPath(strings.TrimPrefix(line, "--- ")); path != "/dev/null" {
				current.path = path
			}
		case strings.HasPrefix(line, "+++ "):
			if path := diffHeaderPath(strings.TrimPrefix(line, "+++ ")); path != "/dev/null" {
				current.path = path
			}
		case strings.HasPrefix(line, "+"):
			current.additions++
		case strings.HasPrefix(line, "-"):
			current.deletions++
		}
	}
	flush()
	return changes
}

func diffGitPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if split := strings.LastIndex(rest, " b/"); split >= 0 {
		return diffHeaderPath(rest[split+1:])
	}
	if split := strings.LastIndex(rest, ` "b/`); split >= 0 {
		return diffHeaderPath(rest[split+1:])
	}
	return rest
}

func diffHeaderPath(path string) string {
	path = strings.TrimSpace(strings.SplitN(path, "\t", 2)[0])
	if strings.HasPrefix(path, `"`) {
		if unquoted, err := strconv.Unquote(path); err == nil {
			path = unquoted
		}
	}
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		path = path[2:]
	}
	return path
}

func localCICaseOutput(output []byte) string {
	text := strings.ToValidUTF8(string(output), "\uFFFD")
	var cases []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "ok "),
			strings.HasPrefix(trimmed, "ok\t"),
			strings.HasPrefix(trimmed, "? "),
			strings.HasPrefix(trimmed, "?\t"),
			strings.HasPrefix(trimmed, "FAIL"),
			strings.HasPrefix(trimmed, "=== RUN"),
			strings.HasPrefix(trimmed, "--- PASS:"),
			strings.HasPrefix(trimmed, "--- FAIL:"):
			cases = append(cases, trimmed)
		}
	}
	if len(cases) > 0 {
		text = strings.Join(cases, "\n")
	} else {
		text = strings.TrimSpace(text)
	}
	if len(text) > maxLocalCIOutputBytes {
		text = strings.ToValidUTF8(text[:maxLocalCIOutputBytes], "\uFFFD") + "\n... local-ci output truncated ..."
	}
	return text
}

func markdownSection(body, name string) string {
	lines := strings.Split(body, "\n")
	start, level := -1, 0
	for i, line := range lines {
		headingLevel, heading := markdownHeading(line)
		if start < 0 {
			if headingLevel > 0 && strings.EqualFold(heading, name) {
				start, level = i+1, headingLevel
			}
			continue
		}
		if headingLevel > 0 && headingLevel <= level {
			return strings.TrimSpace(strings.Join(lines[start:i], "\n"))
		}
	}
	if start >= 0 {
		return strings.TrimSpace(strings.Join(lines[start:], "\n"))
	}
	return ""
}

func markdownHeading(line string) (int, string) {
	line = strings.TrimSpace(line)
	level := 0
	for level < len(line) && level < 6 && line[level] == '#' {
		level++
	}
	if level == 0 || level == len(line) || line[level] != ' ' {
		return 0, ""
	}
	return level, strings.TrimSpace(line[level:])
}

func compactProse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
