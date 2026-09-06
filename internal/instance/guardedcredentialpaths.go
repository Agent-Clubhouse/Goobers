package instance

// GuardedCredentialPaths enumerates every on-disk path a loaded Config
// references via a file-backed TokenRef — a repo's PAT or GitHub App private
// key, the workflow source's token/private key, the daemon identity's
// token/private key, and any credential grant's token file (#4273).
//
// It is the input to the deterministic executor's narrow stage-command
// refusal (internal/executor.ShellExecutor.GuardedCredentialPaths): a set of
// paths a stage's argv/env must never reference literally, derived from
// config rather than hardcoded, so a future TokenRef consumer is covered
// automatically without a matching code change here.
//
// This does NOT enumerate every secret an instance holds — only paths THIS
// config names. A secretStores entry backed by ambient identity (Azure
// workload identity, `az cli`) carries no file path of its own and is not,
// and cannot be, represented here.
func GuardedCredentialPaths(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var paths []string
	addRef := func(ref *TokenRef) {
		if ref != nil && ref.File != "" {
			paths = append(paths, ref.File)
		}
	}
	addValue := func(ref TokenRef) {
		if ref.File != "" {
			paths = append(paths, ref.File)
		}
	}
	for i := range cfg.Repos {
		repo := &cfg.Repos[i]
		addValue(repo.Token)
		if repo.Auth != nil {
			addRef(repo.Auth.PrivateKey)
		}
	}
	if cfg.WorkflowSource != nil {
		addRef(cfg.WorkflowSource.Token)
		if cfg.WorkflowSource.Auth != nil {
			addRef(cfg.WorkflowSource.Auth.PrivateKey)
		}
	}
	if cfg.DaemonIdentity != nil {
		addRef(cfg.DaemonIdentity.Token)
		addRef(cfg.DaemonIdentity.PrivateKey)
	}
	for _, grant := range cfg.Credentials {
		addValue(grant.Token)
	}
	return paths
}
