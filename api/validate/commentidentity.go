package validate

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// WarningCommentIdentityDisagreement (WF029) identifies a gaggle whose stages
// that classify pull-request comments as human or Goobers' own — pr-comment-
// watch, rebase-pr, update-behind-pr and merge-pr — resolve identityMode or
// excludeAuthors to different values (#6494). The stages share one classifier
// but read these settings per stage, so a disagreement brings the original bug
// back by configuration: pr-comment-watch routes a person's comment to
// remediation and a healer then clears the label over it as Goobers' own.
// Advisory (strict-neutral): the config still loads and runs.
const WarningCommentIdentityDisagreement WarningCode = "WF029"

// commentIdentityStages are the stage commands that classify comments through
// the shared classifier and read identityMode and excludeAuthors.
var commentIdentityStages = map[string]bool{
	"pr-comment-watch": true,
	"rebase-pr":        true,
	"update-behind-pr": true,
	"merge-pr":         true,
}

// commentIdentityUse is one stage's resolved classifier settings.
type commentIdentityUse struct {
	where   string
	file    string
	name    string
	mode    string
	exclude string
}

func (u commentIdentityUse) setting() string {
	return "identityMode=" + u.mode + " excludeAuthors=[" + u.exclude + "]"
}

// checkCommentIdentityAgreement warns once per gaggle whose comment-
// classifying stages resolve identityMode (unset: shared on an Azure DevOps
// project, dedicated elsewhere) or excludeAuthors differently. A value bound
// at run time (inputsFrom) cannot be compared and is skipped, as is
// update-behind-pr on Azure DevOps, where it reads no comments.
func (ix *index) checkCommentIdentityAgreement(r *Report) {
	byGaggle := map[string][]commentIdentityUse{}
	keys := make([]workflowIdentity, 0, len(ix.workflows))
	for key := range ix.workflows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].gaggle != keys[j].gaggle {
			return keys[i].gaggle < keys[j].gaggle
		}
		return keys[i].name < keys[j].name
	})
	for _, key := range keys {
		indexed := ix.workflows[key]
		w := indexed.definition
		ado := ix.gaggles[w.Spec.Gaggle].Spec.Project.Provider == apiv1.ProviderADO
		for _, task := range w.Spec.Tasks {
			if use, ok := commentIdentityTaskUse(task, ado); ok {
				use.where = fmt.Sprintf("%s/%s", w.Name, task.Name)
				use.file, use.name = indexed.file, w.Name
				byGaggle[w.Spec.Gaggle] = append(byGaggle[w.Spec.Gaggle], use)
			}
		}
	}
	for _, gaggle := range sortedKeys(byGaggle) {
		uses := byGaggle[gaggle]
		if commentIdentityUsesAgree(uses) {
			continue
		}
		parts := make([]string, 0, len(uses))
		for _, use := range uses {
			parts = append(parts, use.where+" ("+use.setting()+")")
		}
		r.addWarning(WarningCommentIdentityDisagreement, uses[0].file, gaggle, "Workflow", uses[0].name,
			"gaggle %q classifies pull-request comments with different settings: %s; set identityMode and excludeAuthors "+
				"to the same values on every pr-comment-watch, rebase-pr, update-behind-pr and merge-pr stage, or a comment one stage "+
				"routes to remediation as a person's can be cleared by another as Goobers' own (#6494)",
			gaggle, strings.Join(parts, ", "))
	}
}

func commentIdentityTaskUse(task apiv1.Task, ado bool) (commentIdentityUse, bool) {
	if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" || !commentIdentityStages[task.Run.Command[1]] {
		return commentIdentityUse{}, false
	}
	if ado && task.Run.Command[1] == "update-behind-pr" {
		return commentIdentityUse{}, false
	}
	for _, input := range []string{"identityMode", "excludeAuthors"} {
		if _, dynamic := task.InputsFrom[input]; dynamic {
			return commentIdentityUse{}, false
		}
	}
	mode := strings.ToLower(strings.TrimSpace(task.Inputs["identityMode"]))
	if mode == "" {
		mode = "dedicated"
		if ado {
			mode = "shared"
		}
	}
	var authors []string
	for _, author := range strings.Split(task.Inputs["excludeAuthors"], ",") {
		if author = strings.ToLower(strings.TrimSpace(author)); author != "" {
			authors = append(authors, author)
		}
	}
	sort.Strings(authors)
	return commentIdentityUse{mode: mode, exclude: strings.Join(authors, ",")}, true
}

func commentIdentityUsesAgree(uses []commentIdentityUse) bool {
	for _, use := range uses[1:] {
		if use.setting() != uses[0].setting() {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
