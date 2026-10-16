# ADO provider hardening, cmd/goobers wiring: #5960

Growth: about +100 non-test lines and +1 file in `cmd/goobers`. Most of it is
doc comments on the fail-closed decisions. All code that does not depend on
command wiring went into `providers` instead:

- `providers.IsADORemoteURL` holds the ADO host classification.
- `providers.ParseADOPullRequestAPIURL` and `providers.IsADOGUID` hold the
  API-form pull request URL parsing.

## Why the growth belongs in the command package

- **`run_adoprurl.go` (new, `adoPullRequestURLByIDMatch`)** is part of
  `run --pr`'s targeted-PR validation (`validateTargetedPullRequest` in
  `run.go`). It decides whether a GUID-form pull request URL belongs to the
  configured repository by reading that repository's ids through the
  provider the validation already built, and it phrases the refusal. The
  configured-repository resolution, the error wording and the sibling
  name-based matcher (`pullRequestURLMatchesRepository`) all live in this
  package already.
- **`nativeDependencyVerdict` (`backlogquery.go`)** replaces
  `nativeDependencyExclusionReason` with roughly the same length, so the
  blocked verdict and the exclusion reason come from one provider read.
  The capability gate it applies is backlog-query's own.
- **The quota observer threading (`runnerwiring_counters.go`, `daemon.go`)**
  passes the daemon's `ProviderQuotaState` to the ADO open-PR lister. This is
  daemon wiring and only exists here.
- **Comments in `backloghealth.go`** record the decision to keep ADO ready ages
  on `System.ChangedDate`. They are comments only.

## Could any of it live elsewhere?

The repository-id comparison could move into `providers` as an
`ADOProvider.PullRequestURLNamesRepository` method. The refusal text and the
configured-repository resolution would still stay here, so the split would
add surface without removing much from `cmd/goobers`. It is a candidate for
the broader `run` validation extraction, not for this fix.
