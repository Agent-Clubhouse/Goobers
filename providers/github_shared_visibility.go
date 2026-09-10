package providers

import (
	"context"
	"fmt"
	"slices"
	"strconv"
)

// GitHubSharedClaimVisibility uses issue-write credentials, independently of
// the contents-write credentials held by the authoritative coordination store.
// It never writes comments, replaces all labels, or changes ownership.
type GitHubSharedClaimVisibility struct {
	Provider   *GitHubProvider
	Repository RepositoryRef
}

func (v GitHubSharedClaimVisibility) validate(key string) error {
	number, err := strconv.ParseUint(key, 10, 64)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != key {
		return fmt.Errorf("shared visibility requires a canonical issue number")
	}
	if v.Provider == nil || v.Repository.Provider != ProviderGitHub {
		return fmt.Errorf("shared visibility requires a GitHub provider")
	}
	return requireOwnerRepo(v.Repository)
}

// ReadClaimed reads the issue's current human-visible claim label.
func (v GitHubSharedClaimVisibility) ReadClaimed(ctx context.Context, key string) (bool, error) {
	if err := v.validate(key); err != nil {
		return false, err
	}
	item, err := v.Provider.GetWorkItem(ctx, v.Repository, key)
	if err != nil {
		return false, err
	}
	return slices.Contains(item.Labels, LabelClaimed), nil
}

// SetClaimed adds or removes only the claim label, without replacing other labels.
func (v GitHubSharedClaimVisibility) SetClaimed(ctx context.Context, key string, present bool) error {
	if err := v.validate(key); err != nil {
		return err
	}
	if present {
		return v.Provider.applyLabelChanges(ctx, v.Repository, key, []string{LabelClaimed}, nil)
	}
	return v.Provider.applyLabelChanges(ctx, v.Repository, key, nil, []string{LabelClaimed})
}
