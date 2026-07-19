# Contributing to Goobers

Thanks for your interest in Goobers — an open, self-hosted agent-workforce platform.
This guide covers the GitHub-based contribution flow. For what the project is and where
it's going, start with [`README.md`](README.md), [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
and [`docs/VISION.md`](docs/VISION.md).

## Ground rules

- Be respectful — see the [Code of Conduct](CODE_OF_CONDUCT.md).
- Contributions are accepted under the repository's [MIT License](LICENSE); by opening a
  pull request you agree your contribution is licensed under it.
- Found a security issue? **Do not open a public issue** — follow [SECURITY.md](SECURITY.md).

## Development setup

You need the Go toolchain declared in [`go.mod`](go.mod) (currently Go 1.26) and
`make`. Lint uses [`golangci-lint`](https://golangci-lint.run) `v2.12.2` (schema-v2
config in [`.golangci.yml`](.golangci.yml)).

```sh
make help     # list all targets
make ci       # the full local gate: fmt-check · vet · build · test · lint
```

`make ci` is the full gate CI enforces on Ubuntu and macOS (see
[`.github/workflows/ci.yml`](.github/workflows/ci.yml)). If it's green locally,
it exercises the same checks as those required jobs.

### CI platform matrix

| Runner | Command | PR status | What it gates |
|---|---|---|---|
| `ubuntu-latest` | `make ci` | Required via the aggregate CI check | The full Linux build, test, race, format, vet, and lint gate |
| `macos-latest` | `make ci` | Required via the aggregate CI check | The full macOS build, test, race, format, vet, and lint gate |
| `windows-latest` | `go vet ./...` and `go build ./...` | Advisory (allowed to fail) | Windows compilation while the platform abstractions and portable toolchain tracked by #620, #623, #625, #627, and #630 land |

All three statuses are reported on every pull request, and one platform failure
does not cancel the others. The required `make ci (fmt-check · vet · build ·
test · lint)` status aggregates the matrix and fails when either Ubuntu or macOS
fails. Go module and build caches are scoped to each runner OS. Once the listed
Windows prerequisites land and the job is stable, Windows will run `make ci`
and become a required check.

## Workflow

1. **Fork** the repo (external contributors) or **branch** from `main` (maintainers).
2. Create a topic branch: `git checkout -b <area>/<short-description>`.
3. Make your change. Keep the diff scoped to one logical concern.
4. **Add tests** for new behavior and error paths — untested new behavior will be sent back.
5. Run `make ci` locally until green.
6. Open a **pull request against `main`**, filling in the
   [PR template](.github/PULL_REQUEST_TEMPLATE.md).
7. The required Ubuntu and macOS CI checks must pass. Address review feedback; keep the
   branch up to date with `main`.

## Merge requirements

`main` is protected. A PR merges once:

- **CI is green** — the required Ubuntu and macOS `make ci` checks pass on the latest commit.
- **Review** — approval from a [CODEOWNER](.github/CODEOWNERS) where required.

Prefer small, reviewable PRs. Squash-merge is the default so `main` stays linear.

## Commit messages

Use a short imperative subject (`area: do the thing`), a body explaining *why* when it's
not obvious, and reference issues (`Closes #123`). Keep unrelated changes out of the commit.
