package main

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
)

func childRepoCloneURL(ref apiv1.RepoRef) (string, error) {
	if repoCloneURL != nil {
		return repoCloneURL(ref)
	}
	return runner.DefaultRepoCloneURL(ref)
}
