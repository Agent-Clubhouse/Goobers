package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/tutorclass"
	"github.com/goobers/goobers/providers"
)

// classifyTutorChangeHelp documents `goobers classify-tutor-change`, TUT-A6's
// differentiated-review classifier (#1218, docs/design/tutor-redesign.md §5
// item 5): workflow topology changes, gate removals/loosening, and skill-list
// changes require explicit human sign-off and must never be auto-merged;
// ordinary persona-prompt edits and gate tuning follow the normal review
// path. This stage never fails the run — it only records a classification
// for open-pr to label and merge-pr to enforce.
const classifyTutorChangeHelp = "Usage: goobers classify-tutor-change [path]\n\n" +
	"Classify a tutor run's diff as persona, gate-tune, or structure\n" +
	"(workflow topology change, gate removal/loosening, or skill-list\n" +
	"change) by diffing every changed workflow/goober YAML file against\n" +
	"base. Never fails the run — an unclassifiable file conservatively\n" +
	"escalates to structure rather than silently defaulting to persona.\n" +
	"Exit codes: 0 = classified, 1 = business error, 2 = usage/IO error.\n"

func runClassifyTutorChange(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("classify-tutor-change", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "classify-tutor-change")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// [path] is accepted for CLI-convention consistency with the other
	// provider-chain stage commands but unused: classification reads only
	// this stage's worktree diff (CWD), and open-pr's later journal
	// read-back (tutorChangeClassificationFromJournal) takes root/runID
	// from its own invocation, not from this stage's result file.
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	base := providerInput("base", "main")
	resultFile := providerInput("resultFile", "tutor-change-class.json")

	changed, err := changedFilesVsBase(base)
	if err != nil {
		pf(stderr, "error: classify-tutor-change: compute changed files vs %q: %v\n", base, err)
		return 1
	}

	category := tutorclass.CategoryPersona
	for _, path := range changed {
		switch {
		case isWorkflowYAML(path):
			oldYAML, oldErr := gitShowOrEmpty(base, path)
			if oldErr != nil {
				pf(stderr, "error: classify-tutor-change: read %s at %s: %v\n", path, base, oldErr)
				return 1
			}
			newYAML, readErr := os.ReadFile(filepath.Join(".", path))
			if readErr != nil {
				if os.IsNotExist(readErr) {
					newYAML = nil
				} else {
					pf(stderr, "error: classify-tutor-change: read %s: %v\n", path, readErr)
					return 1
				}
			}
			topologyChanged, classifyErr := tutorclass.WorkflowTopologyChanged(oldYAML, newYAML)
			if classifyErr != nil {
				pf(stderr, "error: classify-tutor-change: classify %s: %v\n", path, classifyErr)
				return 1
			}
			if topologyChanged {
				category = tutorclass.Escalate(category, tutorclass.CategoryStructure)
				continue
			}
			gateFieldsChanged, classifyErr := tutorclass.GateFieldsChanged(oldYAML, newYAML)
			if classifyErr != nil {
				pf(stderr, "error: classify-tutor-change: classify %s: %v\n", path, classifyErr)
				return 1
			}
			if gateFieldsChanged {
				category = tutorclass.Escalate(category, tutorclass.CategoryGateTune)
			}
		case isGooberYAML(path):
			oldYAML, oldErr := gitShowOrEmpty(base, path)
			if oldErr != nil {
				pf(stderr, "error: classify-tutor-change: read %s at %s: %v\n", path, base, oldErr)
				return 1
			}
			newYAML, readErr := os.ReadFile(filepath.Join(".", path))
			if readErr != nil {
				if os.IsNotExist(readErr) {
					newYAML = nil
				} else {
					pf(stderr, "error: classify-tutor-change: read %s: %v\n", path, readErr)
					return 1
				}
			}
			skillsChanged, classifyErr := tutorclass.GooberSkillsChanged(oldYAML, newYAML)
			if classifyErr != nil {
				pf(stderr, "error: classify-tutor-change: classify %s: %v\n", path, classifyErr)
				return 1
			}
			if skillsChanged {
				category = tutorclass.Escalate(category, tutorclass.CategoryStructure)
			}
		case strings.Contains(filepath.ToSlash(path), "/skills/"):
			// A skill body file itself changed — sign-off required
			// regardless of what it says, per design doc item 5.
			category = tutorclass.Escalate(category, tutorclass.CategoryStructure)
		case strings.HasSuffix(path, "instructions.md"):
			// Persona-level prompt text — the lightest-review default.
		default:
			// An unrecognized file kind touched by this run's diff:
			// conservatively escalate rather than silently defaulting to
			// persona-level review for something this classifier cannot
			// characterize.
			category = tutorclass.Escalate(category, tutorclass.CategoryStructure)
		}
	}

	out := map[string]string{
		"category":        string(category),
		"requiresSignoff": boolString(category.RequiresSignoff()),
	}
	data, err := json.Marshal(out)
	if err != nil {
		pf(stderr, "error: marshal classification: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "classify-tutor-change: %s\n", category)
	return 0
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func isWorkflowYAML(path string) bool {
	slash := filepath.ToSlash(path)
	return strings.Contains(slash, "/workflows/") && (strings.HasSuffix(slash, ".yaml") || strings.HasSuffix(slash, ".yml"))
}

func isGooberYAML(path string) bool {
	slash := filepath.ToSlash(path)
	return strings.Contains(slash, "/goobers/") && strings.HasSuffix(slash, "goober.yaml")
}

// tutorChangeClassificationFromJournal recovers the classify-tutor-change
// stage's classification from the journal — the same scalar-outputs pattern
// gateEditClassificationFromJournal uses, letting open-pr (a later stage)
// reach back to this classification without a fragile InputsFrom chain.
// Returns ("", "") when no such stage ran or its outputs are unreadable.
func tutorChangeClassificationFromJournal(root, runID string) (category, requiresSignoff string) {
	dir, err := runDirFor(layoutFor(root), runID)
	if err != nil {
		return "", ""
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return "", ""
	}
	events, err := rd.Events()
	if err != nil {
		return "", ""
	}
	for _, ev := range events {
		if ev.Type != journal.EventStageFinished || ev.Stage != "classify-tutor-change" || ev.Outputs == nil {
			continue
		}
		gotCategory, ok := ev.Outputs["category"].(string)
		if !ok {
			continue
		}
		gotSignoff, _ := ev.Outputs["requiresSignoff"].(string)
		return gotCategory, gotSignoff
	}
	return "", ""
}

// tutorSignoffRequiredLabel is the label merge-pr checks for (TUT-A6, #1218):
// its presence structurally refuses auto-merge, independent of whether any
// pipeline actually feeds a tutor PR into merge-pr today. tutorPersonaLabel/
// tutorGateTuneLabel are informational-only companions applied to the
// lighter-review categories.
const (
	tutorSignoffRequiredLabel = "tutor:needs-signoff"
	tutorPersonaLabel         = "tutor:persona"
	tutorGateTuneLabel        = "tutor:gate-tune-category"
)

// labelTutorChangeCategory applies the label matching category, clearing the
// other two so a repass that reclassifies never leaves a stale label
// alongside the current one — same swap discipline as labelGateEdit.
func labelTutorChangeCategory(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prNumber int, category string, requiresSignoff bool) error {
	all := []string{tutorSignoffRequiredLabel, tutorPersonaLabel, tutorGateTuneLabel}
	var add, remove []string
	switch {
	case requiresSignoff:
		add = []string{tutorSignoffRequiredLabel}
	case category == string(tutorclass.CategoryGateTune):
		add = []string{tutorGateTuneLabel}
	default:
		add = []string{tutorPersonaLabel}
	}
	for _, label := range all {
		if label != add[0] {
			remove = append(remove, label)
		}
	}
	comment := ""
	if requiresSignoff {
		comment = fmt.Sprintf(
			"🛡️ **Differentiated review routing** (TUT-A6, #1218): this run's diff is classified **%s** — a workflow-topology change, gate removal/loosening, or skill-list change. Per the accepted design (D5), this **requires explicit human sign-off and must never be auto-merged**; `goobers merge-pr` refuses a PR carrying `%s`.",
			category, tutorSignoffRequiredLabel)
	}
	_, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: fmt.Sprintf("%d", prNumber), AddLabels: add, RemoveLabels: remove, Comment: comment,
	})
	return err
}
