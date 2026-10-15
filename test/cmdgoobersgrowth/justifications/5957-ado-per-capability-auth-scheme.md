# #5957: per-capability Azure DevOps authorization scheme

Growth: about +150 non-test lines and +1 file in `cmd/goobers`
(`adocredentialscheme.go`).

## Why the growth belongs in the command package

- **`adocredentialscheme.go`** maps each credential key granted to an Azure
  DevOps gaggle to the scheme of the source that backs it. It has to read three
  things together:
  - the resolver ref naming that `buildRoleCredentials` uses
    (`<org>/<project>/<name>` for repository bindings);
  - `credentialRefName` for `credentials:` entries;
  - the instance's repository auth kinds (`adoauth.AuthScheme`).

  All of these are composition-root details of `cmd/goobers`. The two callers
  are the credential plane (`credentialplane.go`) and the local executor wiring
  (`runnerwiring_executors.go`), both of which are also in this package.
- The other `cmd/goobers` edits change existing consumers so they use the
  per-credential scheme: the stage credential kind, the pod checkout and
  scrubber, the recovery fetch, child publication and the terminal branch-delete
  arm. `gitToken` is replaced by `gitCredential` rather than added beside it.

## Could any of it live elsewhere?

The transport pieces are already outside `cmd/goobers`:
- the `Scheme` field on the minted-credential wire types (`internal/httpapi`,
  `internal/dispatcher`);
- the variable name (`internal/capability`);
- the local executor's stamping (`internal/executor`).

Only the ref-to-scheme mapping stays here, because the ref names it keys on are
private to this package's credential wiring.
