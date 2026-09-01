# macOS quickstart (tier 1, local)

**Start here on macOS.** Complete sections 1 and 2 to install prerequisites and
the verified `goobers` release. Then choose one route:

- return to the platform-neutral [quickstart tutorial](quickstart.md) for
  disposable learning; or
- continue with section 3 and
  [Onboard an arbitrary repository](arbitrary-repo-onboarding.md) to configure
  a real instance, using `goobers init --guided` if you want the
  browser wizard.

The remaining sections contain macOS-specific Keychain, isolation, and service
guidance. They supplement rather than duplicate either route.

Goobers publishes native macOS release archives for both Apple silicon
(`darwin/arm64`) and Intel (`darwin/amd64`). The installer detects the current
architecture; no Rosetta translation is required.

## 1. Install prerequisites

Install the host tools with [Homebrew](https://brew.sh/):

```sh
brew install git
```

A release install requires `curl`, `tar`, and `shasum`, which macOS provides.
Go is needed only when building Goobers from source. The default
implementation workflow also runs the target repository's `make ci`, so its
toolchain, `make`, and `golangci-lint` must be available to the daemon:

```sh
brew install go golangci-lint
```

Use the Go version pinned in [`go.mod`](../../go.mod) when building this
checkout. Install whichever agent harness your goobers declare — only the one
they use, and authenticated as the same macOS user that will run Goobers:

- [GitHub Copilot CLI](https://docs.github.com/copilot/how-tos/copilot-cli/set-up-copilot-cli/install-copilot-cli)
  for `harness: copilot` goobers.
- Claude Code CLI for `harness: claude-code` goobers:

  ```sh
  npm install -g @anthropic-ai/claude-code
  claude auth login
  ```

Homebrew uses `/opt/homebrew/bin` on Apple silicon and `/usr/local/bin` on
Intel. A launchd service does not read shell startup files, so include the
appropriate path in its configured `PATH`.

## 2. Download and verify a release

Choose an exact stable tag from
[GitHub Releases](https://github.com/Agent-Clubhouse/Goobers/releases), then
download its archive and `SHA256SUMS`. This example selects the archive for the
current Mac:

```sh
VERSION=v0.1.0
case "$(uname -m)" in
  arm64) ARCH=arm64 ;;
  x86_64) ARCH=amd64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
ARCHIVE="goobers_${VERSION}_darwin_${ARCH}.tar.gz"
BASE="https://github.com/Agent-Clubhouse/Goobers/releases/download/${VERSION}"

curl -fLO "${BASE}/${ARCHIVE}"
curl -fLO "${BASE}/SHA256SUMS"
EXPECTED="$(awk -v name="$ARCHIVE" '$2 == name { print $1 }' SHA256SUMS)"
test -n "$EXPECTED"
ACTUAL="$(shasum -a 256 "$ARCHIVE" | awk '{ print $1 }')"
test "$ACTUAL" = "$EXPECTED" || {
  echo "checksum mismatch for $ARCHIVE" >&2
  exit 1
}
echo "OK: checksum matches"
```

The binary is Developer ID signed and notarized, but verify the checksum
too — it's recomputed after signing, so it always covers the exact
published bytes. Extract and install the verified binary:

```sh
tar -xzf "$ARCHIVE"
install -d "$HOME/.local/bin"
install -m 0755 goobers "$HOME/.local/bin/goobers"
export PATH="$HOME/.local/bin:$PATH"
goobers --version
```

Alternatively, the release's attached `install.sh` performs the same
architecture selection and checksum verification before starting guided
setup. See [Releases and packaging](releases.md#install-the-latest-stable-release).

To build from source instead:

```sh
go build -o bin/goobers ./cmd/goobers
bin/goobers --version
```

## 3. Configure credentials with Keychain

Keep the instance root outside the target repository and follow
[Onboard an arbitrary repository](arbitrary-repo-onboarding.md).
macOS can resolve provider credentials directly from the login Keychain. Add
one generic-password item per capability:

```sh
security add-generic-password -U -a "$USER" \
  -s "goobers/github-issues" -w
```

Reference only its service name in `instance.yaml`:

```yaml
credentials:
  - capability: github:issues:write
    token:
      keychain: goobers/github-issues
```

Goobers invokes `/usr/bin/security find-generic-password` on every resolution;
it does not retain the value between resolutions. A missing item, locked
Keychain, denied access, or empty value fails closed. For launchd, create the
item as the same logged-in user that owns the LaunchAgent and verify it can be
read without an interactive prompt:

```sh
security find-generic-password -s "goobers/github-issues" -w >/dev/null
```

Environment and owner-only token files remain supported, but Keychain keeps
the secret encrypted at rest. See
[GitHub token storage options](github-token-scopes.md#token-storage-options)
for rotation and scope guidance. Never place a token value in
`instance.yaml`, `config/`, or a LaunchAgent plist.

## 4. Understand macOS isolation

macOS supplies `/usr/bin/sandbox-exec`; no additional sandbox package is
required.

- A deterministic stage declaring `run.network: none` runs under a Seatbelt
  profile that denies all network operations. If the isolation wrapper cannot
  be applied, the stage fails closed rather than running with network access.
- Agentic filesystem isolation is opt-in. By default, an omitted
  `sandbox.agentic` setting resolves to `disabled`, and agentic harnesses run
  directly on the host. Enable Seatbelt confinement instance-wide with:

  ```yaml
  sandbox:
    agentic: enforced
  ```

  With `sandbox.agentic: enforced`, agentic harnesses run under a separate
  Seatbelt filesystem profile. Writes are denied outside the stage worktree
  and explicitly declared runtime roots, while installed tools, certificates,
  and the user's local authentication remain readable. If Seatbelt is
  unavailable or cannot be applied, the stage fails closed rather than running
  the harness unconfined.
- Agentic workflows generally require network access to their model and
  providers. The filesystem sandbox does not claim to block that egress; use
  `network: none` only for deterministic commands that need no connectivity.

Seatbelt is deprecated by Apple but remains the native mechanism Goobers
checks and uses when agentic sandbox enforcement is enabled. The
credential-free demo in the canonical quickstart exercises the deterministic
network-isolation path.

## 5. Run and supervise the daemon

Run interactively first:

```sh
goobers up ./my-instance
```

From another terminal, check it with
`goobers status --daemon ./my-instance`; Ctrl-C requests a graceful drain.

For unattended operation, install the per-user launchd LaunchAgent. The
Goobers service commands create and manage it while preserving the current
user's Keychain access:

```sh
goobers service install ./my-instance
goobers service status ./my-instance
goobers service stop ./my-instance
goobers service start ./my-instance
goobers service uninstall ./my-instance
```

The equivalent native launchd lifecycle commands are:

```sh
DOMAIN="gui/$(id -u)"
LABEL="com.agent-clubhouse.goobers"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"

launchctl bootstrap "$DOMAIN" "$PLIST"       # load
launchctl kickstart -k "$DOMAIN/$LABEL"      # start or restart
launchctl print "$DOMAIN/$LABEL"             # status
launchctl stop "$DOMAIN/$LABEL"              # graceful stop, remain loaded
launchctl bootout "$DOMAIN/$LABEL"           # graceful stop and unload
```

launchd sends `SIGTERM` when stopping the job, allowing in-flight runs to
drain before the configured timeout. The daemon and every workflow subprocess
inherit the LaunchAgent's `PATH`, not the interactive shell's. If a stage
cannot find Go, `golangci-lint`, Copilot CLI, or a project tool, update the
service environment and restart it.

See [Daemon supervision](supervision.md#macos-launchd) for the LaunchAgent
template, logs, timeout behavior, and upgrade procedure.
