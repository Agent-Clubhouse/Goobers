package apicontract

import (
	"strings"

	"github.com/goobers/goobers/internal/workbench"
)

// WorkbenchSourcesPath and related routes expose bounded human source reads.
const (
	WorkbenchSourcesPath          = V1Prefix + "/gaggles/{gaggle}/workbench/sources"
	WorkbenchItemsPath            = WorkbenchSourcesPath + "/{source}/items"
	WorkbenchItemPath             = WorkbenchItemsPath + "/{item}"
	RouteWorkbenchSources RouteID = "workbenchSources"
	RouteWorkbenchItems   RouteID = "workbenchItems"
	RouteWorkbenchItem    RouteID = "workbenchItem"
)

// WorkbenchSourcePage describes explicit source scope, without credentials.
type WorkbenchSourcePage = workbench.SourcePage

// BacklogPage is one bounded provider window with explicit coverage.
type BacklogPage = workbench.BacklogPage

// BacklogItem distinguishes stable source identity from a native locator.
type BacklogItem = workbench.BacklogItem

func workbenchReadRoute(id RouteID) bool {
	return id == RouteWorkbenchSources || id == RouteWorkbenchItems || id == RouteWorkbenchItem
}
func withWorkbenchFixtures(fixtures wireFixtures) wireFixtures {
	fixtures.WorkbenchSources = workbench.SourcePage{Generation: strings.Repeat("a", 64), Items: []workbench.SourceView{{BindingID: "backlog", Kind: "backlog", Provider: "github", Owner: "acme", Repository: "issues", WriteFields: []string{"title", "description"}}}}
	fixtures.WorkbenchItem = workbench.BacklogItem{Ref: workbench.NodeRef{GaggleID: "web", SourceBindingID: "backlog", Kind: "work-item", SourceID: "987654"}, Locator: workbench.SourceLocator{ID: "42", URL: "https://github.com/acme/issues/issues/42"}, Revision: "2026-10-04T12:00:00Z", RevisionSemantics: "timestamp-preflight", Type: "Issue", Title: "Reliable processing", State: "open", Objective: true, RelationshipCoverage: workbench.RelationshipCoverage{Parents: "not-loaded", Blockers: "not-loaded", Milestones: "complete"}}
	fixtures.WorkbenchItems = workbench.BacklogPage{Items: []workbench.BacklogItem{fixtures.WorkbenchItem}, Exhausted: true, Candidates: 1, SourceTargetDigest: strings.Repeat("b", 64)}
	return fixtures
}
