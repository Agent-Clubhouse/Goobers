package runtimewiring

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/providers"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
)

// VerifyRecoveryLandingRepositories checks landing proof against the available clones.
// A receiving commit may exist only in the pinned clone, not the mirror (or
// vice versa). One complete content proof suffices; an absent object in another
// managed copy must not hide that proof. Cleanup still checks every owned ref.
func VerifyRecoveryLandingRepositories(ctx context.Context, repositories []string, record recovery.Record, landed []recovery.LandedHead, maxArchiveBytes int64) (bool, error) {
	var failures error
	for _, repository := range repositories {
		for _, head := range landed {
			verified, err := recovery.VerifyLandedRestoration(ctx, repository, record, head, maxArchiveBytes)
			if err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			if verified {
				return true, nil
			}
		}
	}
	return false, failures
}

// RecoveryRetentionCloneURL selects the clone URL used by retention operations.
func RecoveryRetentionCloneURL(cfg *instance.Config, key string) (string, error) {
	clone := RepoCloneURL
	if clone == nil {
		clone = providers.CloneURL
	}
	var url string
	for _, repo := range cfg.Repos {
		identity := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		if identity.CanonicalKey() != key {
			continue
		}
		if url != "" {
			return "", fmt.Errorf("ambiguous recovery repository configuration")
		}
		var err error
		url, err = clone(apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name})
		if err != nil {
			return "", err
		}
	}
	if url == "" {
		return "", fmt.Errorf("recovery repository is not configured")
	}
	return url, nil
}
