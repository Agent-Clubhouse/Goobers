package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

type mergeInventorySource interface {
	MergeInventory(context.Context, providers.MergeInventoryRequest) ([]providers.MergeInventoryEntry, error)
}

func compareGitHubMerges(root, repository, identities string, db *rollup.DB, query rollup.MergeReportQuery) (*rollup.MergeComparison, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repository, "?#\\ \t\r\n") {
		return nil, fmt.Errorf("--compare-github requires an explicit owner/repository")
	}
	if strings.TrimSpace(identities) == "" {
		return nil, fmt.Errorf("--shared-identities is required with --compare-github; identity is never inferred from PR authors")
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: parts[0], Name: parts[1]}
	source, err := newProviderForStageSurface[mergeInventorySource](root, repo, true, withStageProviderCapability(capability.GitHubPRRead))
	if err != nil {
		return nil, err
	}
	// Residuals have no known gaggle. Compare against every retained fleet's
	// proof even when the displayed telemetry table selects just one fleet.
	query.InstanceID, query.Gaggle, query.RepositoryAPIURL = "", "", ""
	inventory, err := source.MergeInventory(context.Background(), providers.MergeInventoryRequest{Repository: repo, Since: query.Since, Until: query.Until, Limit: rollup.MaxMergeReportEvents})
	if err != nil {
		return nil, err
	}
	proof, err := db.MergeProvenanceForInventory(context.Background(), query, inventory)
	if err != nil {
		return nil, err
	}
	comparison, err := rollup.CompareMergeInventory(proof, inventory, strings.Split(identities, ","))
	if err != nil {
		return nil, err
	}
	return &comparison, nil
}
