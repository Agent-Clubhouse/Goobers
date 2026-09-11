# Run Copilot workflows through Agency

This example uses Agency for normal Copilot agent runs while keeping Copilot SDK
model discovery directly connected to Copilot. It uses the account already
authenticated by `copilot`; no token environment variables or Goobers
credential grants are required.

## Prerequisites

1. Install `agency`, `copilot`, and `goobers` on every runner that can execute
   the workflow.
2. Run `copilot` interactively once as the runner identity and complete login.
3. Confirm `copilot --version` and `agency --version` work from the same
   environment that starts Goobers.

Do not set `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN` for this
example. Do not add an `agent:model` entry under the instance's `credentials`.
The workflow and goober may still declare the `agent:model` capability; that
declares model use, not a requirement to configure a token.

## Build the launcher

Windows:

```powershell
go build -o C:\tools\agency-copilot-launcher.exe .\agency-copilot-launcher.go
```

Linux or macOS:

```bash
go build -o /usr/local/bin/agency-copilot-launcher ./agency-copilot-launcher.go
```

Set `AGENCY_BIN` or `COPILOT_BIN` only when those commands are not available on
`PATH`. Each variable must contain one executable path, not a command line.

The launcher handles three invocation types:

- `--goobers-launcher-contract`: returns the Goobers adapter-managed contract.
- Copilot SDK `--headless --stdio`: runs Copilot directly so SDK model discovery
  retains its JSON-RPC and persisted-login authentication path.
- All normal prompt invocations: runs `agency copilot` with the original
  arguments, environment, and standard streams.

## Configure the instance

Merge the `runner` block from [`instance.yaml`](instance.yaml) into the
instance's existing `instance.yaml`, replacing the launcher path for the target
operating system. Do not add a credential block.

Restart Goobers after changing the launcher or its configuration. Launcher
contracts are cached for the lifetime of the process.

Custom Copilot launchers automatically receive Goobers' required
`task_complete` tool. Workflow and goober definitions do not need to add it
manually.

## Validate

Start or restart the instance. Goobers preflight verifies the launcher contract,
performs authenticated model discovery, and runs a harmless completion probe
before accepting agentic work. Then run a small manual workflow before enabling
scheduled production work.

