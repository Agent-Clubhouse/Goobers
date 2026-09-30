package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// Bounded work-item parent ancestry for gather-issue-context (#6125). The
// traversal is off unless the stage sets parentTraversal, so a brief written
// without it is byte-for-byte what it was before.

const (
	ancestryDefaultMaxDepth      = 3
	ancestryDefaultMaxItems      = 10
	ancestryDefaultMaxFieldBytes = 4096

	// Hard ceilings keep a misconfigured stage from walking an unbounded
	// graph or pasting whole documents into the brief.
	ancestryMaxDepthCeiling      = 10
	ancestryMaxItemsCeiling      = 100
	ancestryMaxFieldBytesCeiling = 65536
)

// issueAncestryConfig is the parsed parent* stage inputs.
type issueAncestryConfig struct {
	enabled bool
	options providers.AncestryOptions
}

// parseIssueAncestryConfig reads and validates the parent* inputs. An
// invalid value is a usage error: silently clamping it would hide a
// misconfigured bound.
func parseIssueAncestryConfig() (issueAncestryConfig, error) {
	enabled, err := strconv.ParseBool(providerInput("parentTraversal", "false"))
	if err != nil {
		return issueAncestryConfig{}, fmt.Errorf("parentTraversal: %w", err)
	}
	opts := providers.AncestryOptions{
		IncludeTypes: splitCommaList(providerInput("parentIncludeTypes", "")),
		Fields:       splitCommaList(providerInput("parentFields", "")),
		CrossProject: strings.ToLower(strings.TrimSpace(providerInput("parentCrossProject", providers.AncestryCrossProjectDeny))),
	}
	if opts.CrossProject != providers.AncestryCrossProjectDeny && opts.CrossProject != providers.AncestryCrossProjectAllow {
		return issueAncestryConfig{}, fmt.Errorf("parentCrossProject must be %q or %q, got %q", providers.AncestryCrossProjectDeny, providers.AncestryCrossProjectAllow, opts.CrossProject)
	}
	bounds := []struct {
		name, raw    string
		def, ceiling int
		into         *int
	}{
		{"parentMaxDepth", providerInput("parentMaxDepth", ""), ancestryDefaultMaxDepth, ancestryMaxDepthCeiling, &opts.MaxDepth},
		{"parentMaxItems", providerInput("parentMaxItems", ""), ancestryDefaultMaxItems, ancestryMaxItemsCeiling, &opts.MaxItems},
		{"parentMaxFieldBytes", providerInput("parentMaxFieldBytes", ""), ancestryDefaultMaxFieldBytes, ancestryMaxFieldBytesCeiling, &opts.MaxFieldBytes},
	}
	for _, bound := range bounds {
		value, err := boundedIntInput(bound.name, bound.raw, bound.def, bound.ceiling)
		if err != nil {
			return issueAncestryConfig{}, err
		}
		*bound.into = value
	}
	return issueAncestryConfig{enabled: enabled, options: opts}, nil
}

// boundedIntInput parses a positive integer input no larger than ceiling,
// defaulting when unset.
func boundedIntInput(name, raw string, def, ceiling int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 1 || value > ceiling {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d, got %q", name, ceiling, raw)
	}
	return value, nil
}

// gatherIssueAncestry walks the originating issues' parents through the
// issues provider's parent relation reader. A provider without one yields an
// explicit "unsupported" ancestry rather than an absent section. Only a
// cancelled context is an error; every unreadable parent is an omission.
func gatherIssueAncestry(ctx context.Context, reader issueContextIssueReader, repo providers.RepositoryRef, items []providers.WorkItem, cfg issueAncestryConfig) (*apiv1.RemediationAncestry, error) {
	result := providers.UnsupportedWorkItemAncestry()
	if parents, ok := reader.(providers.WorkItemParentReader); ok {
		roots := make([]providers.WorkItemNode, len(items))
		for i, item := range items {
			roots[i] = parents.AncestryRoot(repo, item)
		}
		var err error
		result, err = providers.TraverseWorkItemAncestry(ctx, parents, repo, roots, cfg.options)
		if err != nil {
			return nil, err
		}
	}
	return remediationAncestry(result, repo, cfg.options), nil
}

// remediationAncestry converts a provider walk into the brief's section.
func remediationAncestry(result providers.WorkItemAncestry, repo providers.RepositoryRef, opts providers.AncestryOptions) *apiv1.RemediationAncestry {
	out := &apiv1.RemediationAncestry{
		Status:       result.Status,
		Provider:     string(repo.Provider),
		MaxDepth:     opts.MaxDepth,
		MaxItems:     opts.MaxItems,
		CrossProject: opts.CrossProject,
		IncludeTypes: opts.IncludeTypes,
		Items:        make([]apiv1.RemediationAncestor, 0, len(result.Items)),
		Omissions:    make([]apiv1.RemediationAncestryOmission, 0, len(result.Omissions)),
	}
	for _, item := range result.Items {
		out.Items = append(out.Items, remediationAncestor(item))
	}
	for _, omission := range result.Omissions {
		out.Omissions = append(out.Omissions, apiv1.RemediationAncestryOmission(omission))
	}
	return out
}

func remediationAncestor(item providers.WorkItemAncestor) apiv1.RemediationAncestor {
	fields := make([]apiv1.RemediationAncestorField, 0, len(item.Fields))
	for _, field := range item.Fields {
		fields = append(fields, apiv1.RemediationAncestorField(field))
	}
	return apiv1.RemediationAncestor{
		QualifiedID: item.Key(),
		Provider:    string(item.Provider),
		Project:     item.Project,
		ID:          item.ID,
		Depth:       item.Depth,
		ParentOf:    item.ParentOf,
		Type:        item.Type,
		Title:       item.Title,
		State:       item.State,
		URL:         item.URL,
		Fields:      fields,
		Integrity:   item.Integrity,
	}
}

// ancestryIntegrities returns every included parent's provenance grade, so
// the brief's integrity is no stronger than the weakest parent it carries.
func ancestryIntegrities(ancestry *apiv1.RemediationAncestry) []apiv1.Integrity {
	if ancestry == nil {
		return nil
	}
	grades := make([]apiv1.Integrity, 0, len(ancestry.Items))
	for _, item := range ancestry.Items {
		grades = append(grades, item.Integrity)
	}
	return grades
}
