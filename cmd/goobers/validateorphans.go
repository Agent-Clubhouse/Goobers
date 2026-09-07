package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/providers"
)

// A validation invocation reads at most 1,000 raw ready candidates per repo.
// Hitting this ceiling is explicitly inconclusive, never an all-clear.
const orphanScanPages = 10
const orphanScanPageSize = 100

type backlogRoutingScope struct {
	name       string
	required   []string
	excluded   []string
	expression string
}

func appendBacklogRoutingDemand(cfg *instance.Config, set *instance.ConfigSet, demand map[int]*repoRealityDemand) {
	for index, d := range demand {
		for _, gaggle := range set.Gaggles {
			bound, ok := configuredRepoForProject(cfg, gaggle.Spec.Project)
			if !ok || !sameRoutingRepository(bound, cfg.Repos[index]) {
				continue
			}
			d.orphans = append(d.orphans, localBacklogRoutingScopes(gaggle, set.Workflows)...)
			for _, sibling := range gaggle.Spec.Siblings {
				siblingRepo := instance.RepoRef{Provider: string(sibling.Project.Provider), Owner: sibling.Project.Owner, Project: sibling.Project.Project, Name: sibling.Project.Name, BaseURL: sibling.Project.BaseURL}
				if sameRoutingRepository(bound, siblingRepo) {
					d.orphans = append(d.orphans, backlogRoutingScope{name: "declared sibling " + sibling.Label, required: sibling.RequireLabels})
				}
			}
		}
	}
}

func sameRoutingRepository(a, b instance.RepoRef) bool {
	return a.Provider == b.Provider && a.Owner == b.Owner && a.Project == b.Project && a.Name == b.Name && a.BaseURL == b.BaseURL
}

func localBacklogRoutingScopes(gaggle apiv1.Gaggle, workflows []apiv1.Workflow) []backlogRoutingScope {
	var scopes []backlogRoutingScope
	for _, wf := range workflows {
		if wf.Spec.Gaggle != gaggle.Name {
			continue
		}
		for _, task := range wf.Spec.Tasks {
			if !isGoobersStageCommand(task, "backlog-query") || !stageCommandHasFlag(task, "--claim") {
				continue
			}
			required := append([]string(nil), gaggle.Spec.RequireLabels...)
			if value, overridden := task.Inputs["requireLabels"]; overridden {
				required = splitLabelList(value)
			}
			if trust := strings.TrimSpace(task.Inputs["trustLabel"]); trust != "" {
				required = append(required, trust)
			}
			scopes = append(scopes, backlogRoutingScope{
				name:     gaggle.Name + "/" + wf.Name + "/" + task.Name,
				required: required, excluded: splitLabelList(task.Inputs["excludeLabels"]), expression: task.Inputs["labelPredicate"],
			})
		}
	}
	return scopes
}

func compileRoutingScopes(scopes []backlogRoutingScope) ([]*labelpredicate.Predicate, string, error) {
	filters := make([]*labelpredicate.Predicate, 0, len(scopes))
	var descriptions []string
	for _, scope := range scopes {
		filter, err := labelpredicate.Compile(scope.expression, scope.required, scope.excluded)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", scope.name, err)
		}
		filters = append(filters, filter)
		descriptions = append(descriptions, fmt.Sprintf("%s requires %v, excludes %v, predicate %q", scope.name, scope.required, scope.excluded, scope.expression))
	}
	return filters, strings.Join(descriptions, "; "), nil
}

func matchesAnyRoutingScope(labels []string, filters []*labelpredicate.Predicate) (bool, error) {
	for _, filter := range filters {
		matched, err := filter.Matches(labels)
		if err != nil || matched {
			return matched, err
		}
	}
	return false, nil
}

func warnOrphanBacklogItems(label string, repo instance.RepoRef, token string, scopes []backlogRoutingScope, stdout io.Writer, collectors ...*diagnosticCollector) {
	if len(scopes) == 0 {
		return
	}
	add := func(code, message string) {
		message = label + ": " + message
		pf(stdout, "%s: %s\n", code, message)
		addDiagnostic(collectors, "instance.yaml", "/repos", code, string(validate.Warning), message)
	}
	filters, routes, err := compileRoutingScopes(scopes)
	if err != nil {
		add("SIB003", "ready-item routing not checked: "+scrubRepositoryError(err, token))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
	defer cancel()
	err = scanOrphanBacklogItems(ctx, validateRealityLister(token), providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}, filters, func(item providers.WorkItem) {
		add("SIB002", fmt.Sprintf("ready issue #%s matches no declared label-routing scope; candidate routes: %s. Correct its routing labels or the declared filters; this does not establish claim eligibility", item.ID, routes))
	})
	if err != nil {
		add("SIB003", "ready-item routing scan incomplete: "+scrubRepositoryError(err, token))
	}
}

func scanOrphanBacklogItems(ctx context.Context, lister repoWorkItemLister, repo providers.RepositoryRef, filters []*labelpredicate.Predicate, orphan func(providers.WorkItem)) error {
	cursor := ""
	seen := make(map[string]bool)
	for page := 1; page <= orphanScanPages; page++ {
		info := &providers.ListWorkItemsPageInfo{}
		items, err := lister.ListWorkItems(ctx, providers.ListWorkItemsRequest{Repository: repo, State: "open", Labels: []string{providers.LabelReady}, Limit: orphanScanPageSize, Page: page, Cursor: cursor, PageInfo: info})
		if err != nil {
			return err
		}
		if len(items) > orphanScanPageSize {
			return fmt.Errorf("provider exceeded the %d-item page bound", orphanScanPageSize)
		}
		for _, item := range items {
			if seen[item.ID] || !item.HasLabel(providers.LabelReady) {
				continue
			}
			seen[item.ID] = true
			matched, err := matchesAnyRoutingScope(item.Labels, filters)
			if err != nil {
				return err
			}
			if !matched {
				orphan(item)
			}
		}
		if !info.HasNext && len(items) < orphanScanPageSize {
			return nil
		}
		if info.NextCursor != "" && info.NextCursor == cursor {
			return fmt.Errorf("provider repeated its continuation cursor")
		}
		cursor = info.NextCursor
	}
	return fmt.Errorf("reached the %d-page/%d-candidate limit; additional ready issues were not checked", orphanScanPages, orphanScanPages*orphanScanPageSize)
}
