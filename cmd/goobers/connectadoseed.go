package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Boards tags live on work items; there is no GitHub-style label catalog to
// create. Only the selector tags belong on the starter task.
type connectADOSeeder interface {
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
}

var newConnectADOSeeder = func(repo providers.RepositoryRef, token string) connectADOSeeder {
	return providers.NewADOProvider(repo.Owner, repo.Project, token)
}

func connectADOSeedRepository(opts connectOptions) (providers.RepositoryRef, error) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: opts.owner, Name: opts.name}
	set, report, err := instance.LoadConfigDir(filepath.Join(opts.root, "config"))
	if err != nil {
		return repo, fmt.Errorf("load connected ADO configuration: %w", err)
	}
	if report.HasErrors() {
		return repo, fmt.Errorf("invalid connected ADO configuration: %v", report)
	}
	target := connectTargetProject(opts)
	for _, gaggle := range set.Gaggles {
		p := gaggle.Spec.Project
		if p.Provider != target.Provider || p.Owner != target.Owner || p.Project != target.Project || p.Name != target.Name {
			continue
		}
		backlog := gaggle.Spec.Backlog
		if backlog.Provider != "ado" || backlog.Project == "" {
			return repo, fmt.Errorf("ADO seeding requires an Azure Boards backlog project")
		}
		if repo.Project != "" && repo.Project != backlog.Project {
			return repo, fmt.Errorf("connected gaggles use multiple Azure Boards projects; seed each backlog explicitly")
		}
		repo.Project = backlog.Project
	}
	if repo.Project == "" {
		return repo, fmt.Errorf("no connected Azure Boards backlog to seed")
	}
	return repo, nil
}

func connectSeedADORepository(opts connectOptions, selectors []string, result *onboardingActionResult, stderr io.Writer) int {
	catalog := connectADOSeedCatalog(opts, selectors)
	if os.Getenv(opts.tokenEnv) == "" {
		appendPendingSeedIssues(result, catalog, "credentials unavailable")
		return 0
	}
	repo, err := connectADOSeedRepository(opts)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), stubSampleProviderTimeout)
		defer cancel()
		err = seedADOStarter(ctx, newConnectADOSeeder(repo, os.Getenv(opts.tokenEnv)), repo, catalog, result)
	}
	if err != nil {
		pf(stderr, "error: seed Azure Boards: %v\n", err)
		return 1
	}
	return 0
}

func connectADOSeedCatalog(opts connectOptions, selectors []string) onboardingSeedCatalog {
	catalog := connectSeedCatalog(selectors, nil)
	// Work-item identity is project-scoped, not repository-scoped. Two
	// repositories using one Boards project must not suppress each other's seed.
	identity := sha256.Sum256([]byte(opts.ado.String()))
	catalog.Sample.ID += fmt.Sprintf("-%x", identity[:16])
	return catalog
}

func seedADOStarter(ctx context.Context, seeder connectADOSeeder, repo providers.RepositoryRef, catalog onboardingSeedCatalog, result *onboardingActionResult) error {
	existing, err := listADOSeedItems(ctx, seeder, repo)
	if err != nil {
		return err
	}
	for _, issue := range catalog.Issues {
		runID := onboardingSeedRunID(catalog, issue, connectAction)
		if strings.Contains(existing, "goobers run-id: "+runID) {
			result.Skipped = append(result.Skipped, "issue:"+issue.ID)
			continue
		}
		_, err := seeder.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
			Repository: repo, Title: issue.Title, Body: issue.Body,
			Labels: append([]string(nil), issue.Labels...), Type: "Task", RunID: runID,
		})
		if err != nil {
			return fmt.Errorf("create starter task: %w", err)
		}
		result.Created = append(result.Created, "issue:"+issue.ID)
	}
	return nil
}

// Refuse to create on an incomplete scan: an unseen previous seed must not
// turn a repeat invocation into a duplicate task. Bound both calls and items.
func listADOSeedItems(ctx context.Context, seeder connectADOSeeder, repo providers.RepositoryRef) (string, error) {
	var bodies strings.Builder
	cursor := ""
	seen := map[string]bool{"": true}
	for range 10 {
		var page providers.ListWorkItemsPageInfo
		items, err := seeder.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository: repo, State: "all", Limit: 100, Cursor: cursor, PageInfo: &page,
		})
		if err != nil {
			return "", err
		}
		if len(items) > 100 || page.CandidateCount > 100 {
			return "", fmt.Errorf("azure Boards seed scan exceeded its page limit")
		}
		for _, item := range items {
			if bodies.Len()+len(item.Body) > 4*1024*1024 {
				return "", fmt.Errorf("azure Boards seed scan exceeded its size limit")
			}
			bodies.WriteString(item.Body)
			bodies.WriteByte('\n')
		}
		if !page.HasNext {
			return bodies.String(), nil
		}
		if seen[page.NextCursor] {
			return "", fmt.Errorf("azure Boards seed scan did not advance")
		}
		cursor = page.NextCursor
		seen[cursor] = true
	}
	return "", fmt.Errorf("azure Boards seed scan exceeded 1000 candidates; seed the backlog explicitly")
}
