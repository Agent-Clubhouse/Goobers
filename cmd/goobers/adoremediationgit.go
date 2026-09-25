package main

import (
	"context"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

type gitAuthEnvironmentResolver func(context.Context, string) ([]string, error)

func tokenGitAuthEnvironment(token string) gitAuthEnvironmentResolver {
	return func(context.Context, string) ([]string, error) {
		return gitAuthEnv(token), nil
	}
}

// adoRemediationGitAuthEnvironment authenticates a remediation stage's Git
// fetches and pushes against Azure DevOps with the repo:push credential the
// stage was delivered (GOOBERS_CRED_REPO_PUSH), in the scheme the daemon stated
// beside it. It reads no instance config: every ADO auth kind backs repo:push
// in the daemon (docs/design/ado-parity-dsl-2-0.md §4.1), so a stage that did
// not declare repo:push gets no Git credential, locally or in a pod.
func adoRemediationGitAuthEnvironment() (gitAuthEnvironmentResolver, error) {
	token, err := providerToken(capability.RepoPush)
	if err != nil {
		return nil, err
	}
	source, err := stageADOCredentialSource(capability.RepoPush, token)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, remoteURL string) ([]string, error) {
		return providers.ADOGitAuthEnvironment(ctx, source, nil, remoteURL)
	}, nil
}
