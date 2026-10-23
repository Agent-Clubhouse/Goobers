package main

import (
	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
)

func childRepoCloneURL(ref apiv1.RepoRef) (string, error) {
	if runtimewiring.RepoCloneURL != nil {
		return runtimewiring.RepoCloneURL(ref)
	}
	return runner.DefaultRepoCloneURL(ref)
}
