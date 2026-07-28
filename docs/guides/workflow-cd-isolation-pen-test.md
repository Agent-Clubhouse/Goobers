# Workflow CD credential-isolation pen test

This runbook checks the Workflow CD boundary with two disposable private
repositories and two fine-grained GitHub personal access tokens:

| Credential | Workflow-config repo | Code repo |
|---|---|---|
| `CD_PAT` | Contents: read-only | No access |
| `CODE_PAT` | No access | Contents: read and write |

Use throwaway repositories, never production credentials. Disable shell tracing
before exporting either token, and do not put a token in a Git URL, command
argument, config file, or captured output. The live automated scoped-token leg is
tracked separately; the repository test is hermetic:

```console
go test ./cmd/goobers -run '^TestWorkflowCDAdversarialIsolation$' -count=1
```

## Prepare the probes

1. Create private repositories `<owner>/workflow-config-probe` and
   `<owner>/code-probe`, each with a committed `main` branch. Populate
   `workflow-config-probe` with a valid config source tree (`manifest.yaml` and
   its referenced `gaggles/` definitions), not an empty repository.
2. Mint `CD_PAT` with **Only select repositories** set to
   `workflow-config-probe` and Contents: Read-only.
3. Mint `CODE_PAT` with **Only select repositories** set to `code-probe` and
   Contents: Read and write. The write grant supplies a positive control for
   the code-repository stage boundary.
4. Install `curl` and `jq`, start a dedicated `sh` process, and paste every
   remaining shell block into that same process. Do not source the commands
   into a long-lived shell: exiting this subprocess runs the cleanup trap.

```sh
sh
set +x
PROBE_DIR=
DAEMON_PID=
ORIGINAL_GOOBER=
PROBE_GOOBER_FILE=
cleanup() {
  status=$?
  trap - EXIT HUP INT QUIT TERM
  if [ -n "${DAEMON_PID:-}" ]; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  if [ -n "${ORIGINAL_GOOBER:-}" ] &&
     [ -f "$ORIGINAL_GOOBER" ] &&
     [ -n "${PROBE_GOOBER_FILE:-}" ]; then
    if ! cp "$ORIGINAL_GOOBER" "$PROBE_GOOBER_FILE.next" ||
       ! mv "$PROBE_GOOBER_FILE.next" "$PROBE_GOOBER_FILE"; then
      echo 'FAIL: could not restore the probe Goober' >&2
      status=1
    fi
  fi
  if [ -n "${PROBE_DIR:-}" ] && ! rm -rf -- "$PROBE_DIR"; then
    echo 'FAIL: could not remove the probe directory' >&2
    status=1
  fi
  unset CONFIG_REPO CODE_REPO CD_PAT CODE_PAT
  unset WORKFLOW_SOURCE_PROBE_TOKEN CODE_REPO_PROBE_TOKEN
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 131' QUIT
trap 'exit 143' TERM

export CONFIG_REPO='<owner>/workflow-config-probe'
export CODE_REPO='<owner>/code-probe'
export CD_PAT='<workflow-config-only token>'
export CODE_PAT='<code-only token>'

PROBE_DIR="$(mktemp -d)" || {
  echo 'FAIL: could not create the probe directory' >&2
  exit 1
}
PROBE_HOME="$PROBE_DIR/home"
ASKPASS="$PROBE_DIR/askpass.sh"
mkdir -p "$PROBE_HOME" "$PROBE_DIR/xdg" || exit 1
: >"$PROBE_DIR/gitconfig" || exit 1
cat >"$ASKPASS" <<'EOF'
#!/bin/sh
case "$1" in
  Username*) printf '%s' 'x-access-token' ;;
  *) printf '%s' "$GOOBERS_GIT_TOKEN" ;;
esac
EOF
chmod 700 "$ASKPASS" || exit 1

CONFIG_URL="https://github.com/$CONFIG_REPO.git"
CODE_URL="https://github.com/$CODE_REPO.git"

probe_git() {
  token=$1
  shift
  env -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS \
    HOME="$PROBE_HOME" \
    XDG_CONFIG_HOME="$PROBE_DIR/xdg" \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_CONFIG_SYSTEM="$PROBE_DIR/gitconfig" \
    GIT_CONFIG_GLOBAL="$PROBE_DIR/gitconfig" \
    GIT_CONFIG_COUNT=4 \
    GIT_CONFIG_KEY_0=credential.helper \
    GIT_CONFIG_VALUE_0= \
    GIT_CONFIG_KEY_1=http.extraHeader \
    GIT_CONFIG_VALUE_1= \
    GIT_CONFIG_KEY_2=http.cookieFile \
    GIT_CONFIG_VALUE_2= \
    GIT_CONFIG_KEY_3=http.saveCookies \
    GIT_CONFIG_VALUE_3=false \
    GOOBERS_GIT_TOKEN="$token" \
    GIT_ASKPASS="$ASKPASS" \
    GIT_TERMINAL_PROMPT=0 \
    git "$@"
}
```

The isolated `HOME`, disabled system/global config, empty credential-helper
chain, and cleared HTTP header/cookie settings are part of the test. Do not
remove them: otherwise a keychain helper, cached cookie, or configured
`Authorization` header can silently authenticate as a different identity.

## Verify read isolation

First establish the two positive controls:

```sh
if ! probe_git "$CD_PAT" ls-remote --exit-code \
  "$CONFIG_URL" refs/heads/main; then
  echo 'FAIL: CD_PAT could not read the workflow-config repository' >&2
  exit 1
fi
if ! probe_git "$CODE_PAT" ls-remote --exit-code \
  "$CODE_URL" refs/heads/main; then
  echo 'FAIL: CODE_PAT could not read the code repository' >&2
  exit 1
fi
```

Both commands must succeed. Then cross the credentials:

```sh
if probe_git "$CD_PAT" ls-remote "$CODE_URL"; then
  echo 'FAIL: CD_PAT reached the code repository' >&2
  exit 1
fi

if probe_git "$CODE_PAT" ls-remote "$CONFIG_URL"; then
  echo 'FAIL: CODE_PAT reached the workflow-config repository' >&2
  exit 1
fi

if ! probe_git "$CD_PAT" ls-remote --exit-code \
  "$CONFIG_URL" refs/heads/main ||
   ! probe_git "$CODE_PAT" ls-remote --exit-code \
  "$CODE_URL" refs/heads/main; then
  echo 'FAIL: an own-repository control failed after the crossed probes' >&2
  exit 1
fi
```

Both crossed commands must fail. GitHub normally reports an authentication or
not-found response for a fine-grained token outside its selected repositories.
A timeout or network failure is not evidence of isolation; the repeated
positive controls make that condition fail the procedure.

## Verify write isolation

Clone the code repository into the disposable probe directory. Every push below
uses `--dry-run`, so no branch is created:

```sh
if ! probe_git "$CODE_PAT" clone --quiet "$CODE_URL" "$PROBE_DIR/code"; then
  echo 'FAIL: CODE_PAT could not clone the code repository' >&2
  exit 1
fi

if ! probe_git "$CODE_PAT" -C "$PROBE_DIR/code" push --dry-run \
  "$CODE_URL" HEAD:refs/heads/wcd-isolation-probe; then
  echo 'FAIL: CODE_PAT could not write the code repository' >&2
  exit 1
fi

if probe_git "$CD_PAT" -C "$PROBE_DIR/code" push --dry-run \
  "$CODE_URL" HEAD:refs/heads/wcd-isolation-probe; then
  echo 'FAIL: CD_PAT could write the code repository' >&2
  exit 1
fi

if probe_git "$CODE_PAT" -C "$PROBE_DIR/code" push --dry-run \
  "$CONFIG_URL" HEAD:refs/heads/wcd-isolation-probe; then
  echo 'FAIL: CODE_PAT could write the workflow-config repository' >&2
  exit 1
fi

if probe_git "$CD_PAT" -C "$PROBE_DIR/code" push --dry-run \
  "$CONFIG_URL" HEAD:refs/heads/wcd-isolation-probe; then
  echo 'FAIL: read-only CD_PAT could write the workflow-config repository' >&2
  exit 1
fi

if ! probe_git "$CODE_PAT" -C "$PROBE_DIR/code" push --dry-run \
  "$CODE_URL" HEAD:refs/heads/wcd-isolation-probe; then
  echo 'FAIL: the code-repository write control failed after crossed probes' >&2
  exit 1
fi
```

## Verify crossed configuration fails closed

Use a disposable instance created from the probe config tree, with at least one
workflow in a known gaggle. Configure its supported integration points with
distinct environment references:

```yaml
repos:
  - provider: github
    owner: <owner>
    name: code-probe
    token:
      env: CODE_REPO_PROBE_TOKEN
workflowSource:
  kind: git
  url: https://github.com/<owner>/workflow-config-probe.git
  ref: main
  token:
    env: WORKFLOW_SOURCE_PROBE_TOKEN
```

Set `PROBE_GOOBER_FILE` to a Goober definition used by a workflow in
`PROBE_GAGGLE`. The file must already contain a `spec.capabilities` list. The
aliases below make the credential assigned to each integration point explicit
without changing the original token variables:

```sh
export INSTANCE_ROOT='<disposable-instance-root>'
export PROBE_GAGGLE='<gaggle-name-from-the-config-repo>'
export PROBE_GOOBER_FILE="$INSTANCE_ROOT/config/gaggles/$PROBE_GAGGLE/goobers/<goober-path>/goober.yaml"
export WORKFLOW_SOURCE_PROBE_TOKEN="$CD_PAT"
export CODE_REPO_PROBE_TOKEN="$CODE_PAT"

if ! env -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS \
  HOME="$PROBE_HOME" \
  XDG_CONFIG_HOME="$PROBE_DIR/xdg" \
  GIT_CONFIG_NOSYSTEM=1 \
  GIT_CONFIG_SYSTEM="$PROBE_DIR/gitconfig" \
  GIT_CONFIG_GLOBAL="$PROBE_DIR/gitconfig" \
  goobers validate --check-repos "$INSTANCE_ROOT"; then
  echo 'FAIL: the correctly scoped instance did not validate' >&2
  exit 1
fi
env -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS \
  HOME="$PROBE_HOME" \
  XDG_CONFIG_HOME="$PROBE_DIR/xdg" \
  GIT_CONFIG_NOSYSTEM=1 \
  GIT_CONFIG_SYSTEM="$PROBE_DIR/gitconfig" \
  GIT_CONFIG_GLOBAL="$PROBE_DIR/gitconfig" \
  goobers up --quiet --watch-config "$INSTANCE_ROOT" \
    >"$PROBE_DIR/daemon.log" 2>&1 &
DAEMON_PID=$!

i=0
while [ ! -s "$INSTANCE_ROOT/scheduler/api.address" ] && [ "$i" -lt 30 ]; do
  sleep 1
  i=$((i + 1))
done
[ -s "$INSTANCE_ROOT/scheduler/api.address" ] || {
  echo 'FAIL: daemon did not publish its API address' >&2
  exit 1
}
API_ADDRESS="$(cat "$INSTANCE_ROOT/scheduler/api.address")"
ACTIVE_URL="http://$API_ADDRESS/api/v1/gaggles/$PROBE_GAGGLE/workflows"
curl --fail --silent --show-error "$ACTIVE_URL" \
  >"$PROBE_DIR/active-before.raw.json" || {
  echo 'FAIL: could not read active configuration before the probe' >&2
  exit 1
}
jq -S 'del(.items[].concurrency.activeRuns)' \
  "$PROBE_DIR/active-before.raw.json" >"$PROBE_DIR/active-before.json" || {
  echo 'FAIL: active configuration before the probe was not valid JSON' >&2
  exit 1
}
```

`configrepo:read` is runner-only: no code-repository stage or Goober may
request it. Forge that crossed declaration into the selected Goober and let
the supported config-directory watcher reconcile the edit. The atomic rename
avoids exposing a partially written YAML file:

```sh
EVENTS="$INSTANCE_ROOT/scheduler/events.jsonl"
if [ -s "$EVENTS" ]; then
  BEFORE_SEQ="$(jq -s 'map(.seq) | max // 0' "$EVENTS")"
else
  BEFORE_SEQ=0
fi

case "$PROBE_GOOBER_FILE" in
  "$INSTANCE_ROOT"/config/*) ;;
  *)
    echo 'FAIL: PROBE_GOOBER_FILE is outside the active config tree' >&2
    exit 1
    ;;
esac
grep -q '^  capabilities:[[:space:]]*$' "$PROBE_GOOBER_FILE" || {
  echo 'FAIL: probe Goober has no spec.capabilities list' >&2
  exit 1
}
ORIGINAL_GOOBER="$PROBE_DIR/goober.original.yaml"
if ! cp "$PROBE_GOOBER_FILE" "$ORIGINAL_GOOBER"; then
  ORIGINAL_GOOBER=
  echo 'FAIL: could not preserve the probe Goober' >&2
  exit 1
fi
awk '
  { print }
  /^  capabilities:[[:space:]]*$/ {
    print "    - configrepo:read"
    inserted = 1
  }
  END {
    if (!inserted) exit 1
  }
' "$ORIGINAL_GOOBER" >"$PROBE_GOOBER_FILE.next" || {
  rm -f "$PROBE_GOOBER_FILE.next"
  echo 'FAIL: could not forge the crossed capability' >&2
  exit 1
}
if ! mv "$PROBE_GOOBER_FILE.next" "$PROBE_GOOBER_FILE"; then
  echo 'FAIL: could not install the crossed capability' >&2
  exit 1
fi

i=0
REJECTION=
while [ "$i" -lt 30 ]; do
  REJECTION="$(
    jq -c --argjson seq "$BEFORE_SEQ" '
      select(
        .seq > $seq and
        .type == "config.reload.rejected"
      )
    ' "$EVENTS" | tail -n 1
  )"
  [ -n "$REJECTION" ] && break
  sleep 1
  i=$((i + 1))
done
[ -n "$REJECTION" ] || {
  echo 'FAIL: crossed stage capability was not rejected' >&2
  exit 1
}
if ! printf '%s\n' "$REJECTION" | jq -e '
    .schema == "goobers.dev/journal/event/v1" and
    .error.code == "config_reload_rejected" and
    (.runner.oldDigest | type == "string" and length > 0) and
    (.runner.newDigest | type == "string" and length > 0) and
    (.runner.oldDigest != .runner.newDigest) and
    (.error.message |
      contains("runner-only capability \"configrepo:read\""))
  ' >/dev/null; then
  echo 'FAIL: crossed capability rejection had an unexpected shape' >&2
  exit 1
fi

if jq -e --argjson seq "$BEFORE_SEQ" '
  select(
    .seq > $seq and
    .type == "config.reloaded"
  )
' "$EVENTS" >/dev/null; then
  echo 'FAIL: crossed stage capability became active' >&2
  exit 1
fi

curl --fail --silent --show-error "$ACTIVE_URL" \
  >"$PROBE_DIR/active-after.raw.json" || {
  echo 'FAIL: could not read active configuration after the probe' >&2
  exit 1
}
jq -S 'del(.items[].concurrency.activeRuns)' \
  "$PROBE_DIR/active-after.raw.json" >"$PROBE_DIR/active-after.json" || {
  echo 'FAIL: active configuration after the probe was not valid JSON' >&2
  exit 1
}
cmp -s "$PROBE_DIR/active-before.json" "$PROBE_DIR/active-after.json" || {
  echo 'FAIL: rejected crossed capability changed active configuration' >&2
  exit 1
}

if ! cp "$ORIGINAL_GOOBER" "$PROBE_GOOBER_FILE.next" ||
   ! mv "$PROBE_GOOBER_FILE.next" "$PROBE_GOOBER_FILE"; then
  echo 'FAIL: could not restore the probe Goober' >&2
  exit 1
fi
ORIGINAL_GOOBER=
```

Finally, cross the code-repository integration itself. Override only the
environment reference named by `repos[0].token.env`; the target-repository
preflight must fail:

```sh
if env -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS \
  HOME="$PROBE_HOME" \
  XDG_CONFIG_HOME="$PROBE_DIR/xdg" \
  GIT_CONFIG_NOSYSTEM=1 \
  GIT_CONFIG_SYSTEM="$PROBE_DIR/gitconfig" \
  GIT_CONFIG_GLOBAL="$PROBE_DIR/gitconfig" \
  CODE_REPO_PROBE_TOKEN="$CD_PAT" \
  goobers validate --check-repos "$INSTANCE_ROOT"; then
  echo 'FAIL: crossed code-repository grant passed preflight' >&2
  exit 1
fi

if ! env -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS \
  HOME="$PROBE_HOME" \
  XDG_CONFIG_HOME="$PROBE_DIR/xdg" \
  GIT_CONFIG_NOSYSTEM=1 \
  GIT_CONFIG_SYSTEM="$PROBE_DIR/gitconfig" \
  GIT_CONFIG_GLOBAL="$PROBE_DIR/gitconfig" \
  CODE_REPO_PROBE_TOKEN="$CODE_PAT" \
  goobers validate --check-repos "$INSTANCE_ROOT"; then
  echo 'FAIL: the code-repository control failed after the crossed preflight' >&2
  exit 1
fi
```

The Git probes above exercise live workflow-source authorization because this
revision has no remote-source poller or config-reconcile HTTP route.
`--watch-config` is the supported local config admission mechanism. The test
passes only when own-repository access succeeds, both crossed reads and writes
fail, a forged stage request for the runner-only source capability is journaled
with the real `config.reload.rejected` shape, and the active workflow inventory
remains unchanged. Exit the dedicated probe shell to stop the daemon, restore
the Goober if interrupted mid-probe, delete the temporary directory, and unset
the token aliases:

```sh
exit
```

After the cleanup returns to the parent shell, revoke both PATs and delete the
two disposable repositories. Do not retain either credential or repository
after recording the result.
