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

You need the Go toolchain declared in [`go.mod`](go.mod) (currently Go 1.26),
Node.js 24 with npm, Git, and
[`golangci-lint`](https://golangci-lint.run) `v2.12.2` (schema-v2 config in
[`.golangci.yml`](.golangci.yml)).

```sh
go run ./test/ci      # portable full Go and portal merge gate
make ci               # optional Unix compatibility alias
make help             # list Unix convenience targets
make validate-configs # build the validator and check every shipped config tree
make portal-ci        # install, type-check, build, test, and check the portal contract
make portal-contract  # regenerate and verify the Go/TypeScript wire contract
```

`go run ./test/ci` is the cross-platform entrypoint CI enforces (see
[`.github/workflows/ci.yml`](.github/workflows/ci.yml)). It launches each tool
without Bash or POSIX-shell syntax and runs the existing Go format, vet, build,
shipped-config validation, race-test, and lint checks plus the portal build,
typecheck, tests, and stale-fixture check. On Windows, the runner uses stock
`cmd.exe` only to invoke Node's standard `npm.cmd` shim. The strategy
deliberately keeps the Go toolchain as the only task-runner prerequisite: GNU
Make is **not** required on Windows.
`make ci` is a thin compatibility shim for existing Darwin and Linux workflows.
The other Make targets remain optional POSIX-shell conveniences;
`make validate-configs` builds the validator and checks every config shipped by
the repository without network or credentials. Run it locally before pushing a
config change; warnings are printed, while validation errors fail the target.
`make portal-ci` reproduces the portal portion alone, and
`make portal-contract` narrows that to the generated Go/TypeScript wire seam.

The portability audit found the old CI graph depended on a globally selected
Bash, shell command substitution and conditionals for `fmt-check`, POSIX
environment-prefix assignments for tests, and shell calls for build metadata.
The Go runner replaces those CI-path constructs with direct process and
environment APIs. Non-CI Make conveniences such as `help`, `cover`, and `clean`
still use Unix tools and are not the supported Windows path.

### Platform prerequisites

| Platform | Required tools | Full gate invocation |
|---|---|---|
| Linux | Go from `go.mod`, Node.js 24 with npm, Git, `golangci-lint` v2.12.2 | `go run ./test/ci` (`make ci` also works with GNU Make and a POSIX shell) |
| macOS | Go from `go.mod`, Node.js 24 with npm, Git, `golangci-lint` v2.12.2 | `go run ./test/ci` (`make ci` also works with GNU Make and a POSIX shell) |
| Windows | Go from `go.mod`, Node.js 24 with npm, Git for Windows, `golangci-lint` v2.12.2, 64-bit MinGW-w64 GCC | `go run ./test/ci` from PowerShell or Command Prompt; Bash and GNU Make are not required |

The Windows compiler is required by Go's race detector, not by the CI task
runner. Install a 64-bit MinGW-w64 GCC with runtime version 8 or newer, put its
`bin` directory on `PATH`, and set `CC` when the compiler executable is not
named `gcc`. The portable runner sets `CGO_ENABLED=1` for the race-test step.
Verify the runtime with `gcc --print-file-name libsynchronization.a`: a
compatible installation prints the full path to that library rather than the
bare filename. See Go's
[race-detector requirements](https://go.dev/doc/articles/race_detector#Requirements).
No Bash or MSYS shell is required by the gate; tests that specifically
exercise Unix process and shell semantics are platform-gated on Windows.

### CI platform matrix

| Runner | Command | PR status | What it gates |
|---|---|---|---|
| `ubuntu-latest` | `go run ./test/ci` | Required via the aggregate CI check | The full Linux Go and portal gate |
| `macos-latest` | `go run ./test/ci` | Required via the aggregate CI check | The full macOS Go and portal gate |
| `windows-latest` | Not yet enabled (#633) | None | The portable entrypoint is ready; remaining Windows runtime abstractions land separately |

The required `make ci (fmt-check · vet · build · test · lint)` status keeps its
existing name for branch-protection compatibility and fails when either current
platform leg fails. Go module and build caches are scoped to each runner OS.
Once the remaining Windows prerequisites land, Windows will run the same
portable command and become a required leg without adding Make or Bash.

## Workflow

1. **Fork** the repo (external contributors) or **branch** from `main` (maintainers).
2. Create a topic branch: `git checkout -b <area>/<short-description>`.
3. Make your change. Keep the diff scoped to one logical concern.
4. **Add tests** for new behavior and error paths — untested new behavior will be sent back.
5. Run `go run ./test/ci` locally until green (`make ci` is the Unix alias).
6. Open a **pull request against `main`**, filling in the
   [PR template](.github/PULL_REQUEST_TEMPLATE.md).
7. The required Ubuntu and macOS CI checks must pass. Address review feedback; keep the
   branch up to date with `main`.

## Merge requirements

`main` is protected. The active repository rules require:

- **CI is green** — the required Ubuntu and macOS portable CI checks pass on the latest commit.
- **Approvals** — none. The required approval count is zero, and
  [CODEOWNER](.github/CODEOWNERS) approval is not required. CODEOWNERS are still
  requested for review, but those requests are advisory.

Required-review enforcement is the repository policy decision tracked in
[#763](https://github.com/Agent-Clubhouse/Goobers/issues/763). If that decision changes
the repository rules, update this section as part of the same settings change so the
documented and enforced policies do not drift.

Prefer small, reviewable PRs. Squash-merge is the default so `main` stays linear.

## Commit messages

Use a short imperative subject (`area: do the thing`), a body explaining *why* when it's
not obvious, and reference issues (`Closes #123`). Keep unrelated changes out of the commit.
