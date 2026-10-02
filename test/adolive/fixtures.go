package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Spec fixture pair (#6125 ancestry, #6191/#6194 acceptance criteria). The
// read-only conformance leg reads them; nothing ever writes them after
// provisioning. Their titles and texts are what the leg asserts against, so
// providers/ado_live_test.go repeats them.
const (
	liveFixtureTag = "goobers-live-fixture"

	parentTitle = "goobers live ancestry parent (do not close)"
	parentBody  = "goobers-live ancestry parent intent. Read by the live conformance leg's ancestry walk (#6125). Do not edit or close it."

	specTitle              = "goobers live spec fixture (do not close)"
	specAcceptanceCriteria = "goobers-live acceptance criterion: the provider composes this field into the work item body (#6191, #6194). Do not edit it."

	acceptanceCriteriaField = "Microsoft.VSTS.Common.AcceptanceCriteria"
	hierarchyReverse        = "System.LinkTypes.Hierarchy-Reverse"
	requirementCategory     = "Microsoft.RequirementCategory"
)

// Failing CI build definition (#5652), created only with -ci-pipeline. The
// write leg commits ciPipelineYAML to its own goobers-live/ branch and queues
// the definition there.
const (
	ciPipelineName  = "goobers-live-ci-failure"
	ciPipelineYAML  = ".goobers-live/ci-failure.yml"
	ciPipelineQueue = "Azure Pipelines"
)

var errSpecParentMismatch = errors.New("the spec fixture's parent is not the ancestry parent fixture")

// provisionSpecFixtures finds or creates the ancestry parent and the spec
// fixture linked under it, and returns the spec fixture's id (0 on a dry run
// that would create it).
func provisionSpecFixtures(ctx context.Context, c *client, cfg config, out io.Writer) (int, error) {
	parentID, err := provisionParentFixture(ctx, c, cfg, out)
	if err != nil {
		return 0, err
	}
	specID, found, err := c.findWorkItem(ctx, specTitle, liveFixtureTag)
	if err != nil {
		return 0, err
	}
	if !found {
		if !cfg.apply {
			printf(out, "Spec fixture work item (#6125, #6191): would create, empty description, acceptance criteria, child of the ancestry parent\n")
			return 0, nil
		}
		itemType, err := c.specType(ctx, cfg)
		if err != nil {
			return 0, err
		}
		patch := []map[string]any{
			{"op": "add", "path": "/fields/System.Title", "value": specTitle},
			{"op": "add", "path": "/fields/System.Tags", "value": liveFixtureTag},
			{"op": "add", "path": "/fields/" + acceptanceCriteriaField, "value": specAcceptanceCriteria},
			c.parentRelation(parentID),
		}
		if specID, err = c.createWorkItem(ctx, itemType, patch); err != nil {
			return 0, fmt.Errorf("create spec fixture work item: %w", err)
		}
		printf(out, "Spec fixture work item (#6125, #6191): created #%d (%s), child of #%d\n", specID, itemType, parentID)
		return specID, nil
	}
	printf(out, "Spec fixture work item (#6125, #6191): present #%d\n", specID)
	return specID, c.ensureSpecParent(ctx, cfg, specID, parentID, out)
}

func provisionParentFixture(ctx context.Context, c *client, cfg config, out io.Writer) (int, error) {
	id, found, err := c.findWorkItem(ctx, parentTitle, liveFixtureTag)
	switch {
	case err != nil:
		return 0, err
	case found:
		printf(out, "Ancestry parent work item (#6125): present #%d\n", id)
		return id, nil
	case !cfg.apply:
		printf(out, "Ancestry parent work item (#6125): would create a %s titled %q\n", cfg.parentType, parentTitle)
		return 0, nil
	}
	id, err = c.createWorkItem(ctx, cfg.parentType, []map[string]any{
		{"op": "add", "path": "/fields/System.Title", "value": parentTitle},
		{"op": "add", "path": "/fields/System.Description", "value": parentBody},
		{"op": "add", "path": "/fields/System.Tags", "value": liveFixtureTag},
	})
	if err != nil {
		return 0, fmt.Errorf("create ancestry parent work item: %w", err)
	}
	printf(out, "Ancestry parent work item (#6125): created #%d (%s)\n", id, cfg.parentType)
	return id, nil
}

// ensureSpecParent adds the spec fixture's Hierarchy link to the parent when
// it is missing: the one write this tool makes to an existing object, and only
// an addition. A link to some other parent is refused rather than replaced.
func (c *client) ensureSpecParent(ctx context.Context, cfg config, specID, parentID int, out io.Writer) error {
	if parentID == 0 {
		printf(out, "  parent link: would add once the ancestry parent exists\n")
		return nil
	}
	current, rev, err := c.workItemParent(ctx, specID)
	if err != nil {
		return err
	}
	switch {
	case current == parentID:
		printf(out, "  parent link to #%d: present\n", parentID)
		return nil
	case current != 0:
		printf(out, "  parent link: #%d has parent #%d, not #%d; re-link it by hand\n", specID, current, parentID)
		return errSpecParentMismatch
	case !cfg.apply:
		printf(out, "  parent link to #%d: would add\n", parentID)
		return nil
	}
	endpoint, err := c.endpoint(nil, "wit", "workitems", strconv.Itoa(specID))
	if err != nil {
		return err
	}
	patch := []map[string]any{{"op": "test", "path": "/rev", "value": rev}, c.parentRelation(parentID)}
	if _, err := c.do(ctx, http.MethodPatch, endpoint, "application/json-patch+json", patch, nil); err != nil {
		return fmt.Errorf("link spec fixture #%d to parent #%d: %w", specID, parentID, err)
	}
	printf(out, "  parent link to #%d: added\n", parentID)
	return nil
}

func (c *client) parentRelation(parentID int) map[string]any {
	return map[string]any{
		"op":   "add",
		"path": "/relations/-",
		"value": map[string]any{
			"rel": hierarchyReverse,
			"url": c.organizationURL + "/_apis/wit/workItems/" + strconv.Itoa(parentID),
		},
	}
}

// workItemParent returns the id of id's Hierarchy parent (0 for none) and the
// item's revision.
func (c *client) workItemParent(ctx context.Context, id int) (int, int, error) {
	endpoint, err := c.endpoint(url.Values{"$expand": []string{"relations"}}, "wit", "workitems", strconv.Itoa(id))
	if err != nil {
		return 0, 0, err
	}
	var item struct {
		Rev       int `json:"rev"`
		Relations []struct {
			Rel string `json:"rel"`
			URL string `json:"url"`
		} `json:"relations"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, "", nil, &item); err != nil {
		return 0, 0, err
	}
	for _, relation := range item.Relations {
		if relation.Rel != hierarchyReverse {
			continue
		}
		parent, err := strconv.Atoi(relation.URL[strings.LastIndex(relation.URL, "/")+1:])
		if err != nil {
			return 0, 0, fmt.Errorf("work item %d parent link %q has no id", id, relation.URL)
		}
		return parent, item.Rev, nil
	}
	return 0, item.Rev, nil
}

// specType is -spec-type, or the project's requirement type (User Story on
// Agile, Product Backlog Item on Scrum, Issue on Basic), the type that carries
// an Acceptance Criteria field.
func (c *client) specType(ctx context.Context, cfg config) (string, error) {
	if t := strings.TrimSpace(cfg.specType); t != "" {
		return t, nil
	}
	endpoint, err := c.endpoint(nil, "wit", "workitemtypecategories", requirementCategory)
	if err != nil {
		return "", err
	}
	var category struct {
		DefaultWorkItemType struct {
			Name string `json:"name"`
		} `json:"defaultWorkItemType"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, "", nil, &category); err != nil {
		return "", fmt.Errorf("resolve the requirement work item type (or pass -spec-type): %w", err)
	}
	if category.DefaultWorkItemType.Name == "" {
		return "", errors.New("the project's requirement category names no default type; pass -spec-type")
	}
	return category.DefaultWorkItemType.Name, nil
}

// provisionCIPipeline finds or creates the failing CI build definition and
// returns its id (0 on a dry run that would create it).
func provisionCIPipeline(ctx context.Context, c *client, cfg config, repo repository, out io.Writer) (int, error) {
	endpoint, err := c.endpoint(url.Values{"name": []string{ciPipelineName}}, "build", "definitions")
	if err != nil {
		return 0, err
	}
	var existing struct {
		Value []struct {
			ID int `json:"id"`
		} `json:"value"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, "", nil, &existing); err != nil {
		return 0, err
	}
	switch {
	case len(existing.Value) > 0:
		printf(out, "CI failure build definition (#5652): present %s (id %d)\n", ciPipelineName, existing.Value[0].ID)
		return existing.Value[0].ID, nil
	case !cfg.apply:
		printf(out, "CI failure build definition (#5652): would create %s reading %s\n", ciPipelineName, ciPipelineYAML)
		return 0, nil
	}
	queueID, err := c.agentQueue(ctx)
	if err != nil {
		return 0, err
	}
	endpoint, err = c.endpoint(nil, "build", "definitions")
	if err != nil {
		return 0, err
	}
	body := map[string]any{
		"name":    ciPipelineName,
		"path":    `\`,
		"type":    "build",
		"quality": "definition",
		"queue":   map[string]any{"id": queueID},
		"process": map[string]any{"type": 2, "yamlFilename": ciPipelineYAML},
		"repository": map[string]any{
			"id":            repo.ID,
			"name":          repo.Name,
			"type":          "TfsGit",
			"defaultBranch": "refs/heads/" + strings.TrimPrefix(cfg.base, "refs/heads/"),
		},
	}
	var created struct {
		ID int `json:"id"`
	}
	if _, err := c.do(ctx, http.MethodPost, endpoint, "application/json", body, &created); err != nil {
		return 0, fmt.Errorf("create CI failure build definition: %w", err)
	}
	printf(out, "CI failure build definition (#5652): created %s (id %d)\n", ciPipelineName, created.ID)
	return created.ID, nil
}

// agentQueue returns the project's hosted Azure Pipelines agent queue id.
func (c *client) agentQueue(ctx context.Context) (int, error) {
	endpoint, err := c.endpoint(url.Values{"queueName": []string{ciPipelineQueue}, "api-version": []string{"7.1-preview.1"}}, "distributedtask", "queues")
	if err != nil {
		return 0, err
	}
	var queues struct {
		Value []struct {
			ID int `json:"id"`
		} `json:"value"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, "", nil, &queues); err != nil {
		return 0, err
	}
	if len(queues.Value) == 0 {
		return 0, fmt.Errorf("project %s has no %q agent queue; the CI failure scenario needs hosted agents", c.project, ciPipelineQueue)
	}
	return queues.Value[0].ID, nil
}
