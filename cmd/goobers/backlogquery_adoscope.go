package main

import (
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// backlogQueryRequireLabels is the stage's requireLabels input plus, on an
// Azure DevOps backlog, the gaggle's spec.backlog.labels (#6098).
//
// Azure Boards are project-wide: every gaggle pointed at one ADO project sees
// every work item in it, so without this scope each gaggle claims every
// approved+ready item in the project, including items meant for a sibling
// repository's gaggle. The labels are appended as required labels, so an item
// must carry ALL of them — the same hard AND-filter spec.backlog.labels
// documents and validate compiles it as (labelpredicate.Compile's required
// set), and the one `goobers connect`/validate's repoSelectorLabels already
// check. Being required labels, they compose (AND) with the stage's own
// labelPredicate and fieldPredicate, and the ADO provider filters them
// server-side (WIQL [System.Tags] CONTAINS, rechecked as whole tags).
//
// GitHub and Gitea issues are repository-scoped, so there the input is
// returned unchanged: this is a no-op for every non-ADO backlog, including a
// GitHub or Gitea backlog for Azure DevOps code (topology (b)).
func backlogQueryRequireLabels(env backlogQueryEnv) []string {
	required := splitLabelList(providerInput("requireLabels", ""))
	if env.issueRepo().Provider != providers.ProviderADO {
		return required
	}
	return appendMissingLabels(required, stageADOBacklogScope(env.root, env.stderr))
}

// stageADOBacklogScope reads the stage's gaggle (GOOBERS_GAGGLE) from the
// instance config and returns its ADO backlog scope labels. An ungaggled
// stage, an unreadable config (a stage pod — warned once, as the other
// stage-side gaggle settings are) or a gaggle without an ADO backlog yields
// nil, today's unscoped behavior.
func stageADOBacklogScope(root string, stderr io.Writer) []string {
	gaggle := os.Getenv(executor.GaggleEnvVar)
	if gaggle == "" {
		return nil
	}
	set, report, err := instance.LoadConfigDir(layoutFor(root).ConfigDir())
	if err != nil || report == nil || set == nil {
		warnStageGaggleConfigUnavailable("backlog.labels (the ADO claim scope)", gaggle, err)
		return nil
	}
	labels, ado := gaggleADOBacklogLabels(set, gaggle)
	if ado && len(labels) == 0 {
		pf(stderr, "warning: gaggle %q has an Azure DevOps backlog but declares no spec.backlog.labels; "+
			"it selects matching work items from the whole project %q, including items meant for other gaggles\n",
			gaggle, gaggleBacklogRef(set, gaggle).Project)
	}
	return labels
}

// gaggleADOBacklogLabels returns the named gaggle's spec.backlog.labels and
// whether its backlog is on Azure DevOps. A gaggle that is absent or whose
// backlog is not ADO reports false and no labels.
func gaggleADOBacklogLabels(set *instance.ConfigSet, gaggle string) ([]string, bool) {
	backlog := gaggleBacklogRef(set, gaggle)
	if backlog.Provider != apiv1.ProviderADO {
		return nil, false
	}
	return compactLabels(backlog.Labels...), true
}

// appendMissingLabels appends each of extra that labels does not already
// name. The comparison ignores case because Azure DevOps tags do.
func appendMissingLabels(labels, extra []string) []string {
	out := append([]string(nil), labels...)
	for _, label := range extra {
		if !containsLabelFold(out, label) {
			out = append(out, label)
		}
	}
	return out
}

func containsLabelFold(labels []string, label string) bool {
	for _, existing := range labels {
		if strings.EqualFold(existing, label) {
			return true
		}
	}
	return false
}
