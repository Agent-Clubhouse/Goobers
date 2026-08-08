# Daemon supervision (systemd · launchd · Windows Service)

Runs the `goobers` daemon through a stable, auto-restarting service host that
launches `goobers up` from the instance's mutable binary slot. This
resolves **DEP-Q6** (`docs/requirements/deployment.md`): tier 1–2 daemon
supervision is **systemd** on Linux, **launchd** on macOS, and a **Windows
Service** wrapper on Windows.

Use the native CLI on an initialized instance:

```sh
goobers service install /absolute/path/to/instance
goobers service status  /absolute/path/to/instance
goobers service stop    /absolute/path/to/instance
goobers service start   /absolute/path/to/instance
goobers service uninstall /absolute/path/to/instance
```

`install` registers, enables, and starts the daemon with crash restart and
backoff configured. `uninstall` drives the graceful shutdown contract before
removing the registration. `status --json` exposes the same state for scripts.
An existing registration is not overwritten; uninstall it first when changing
the stable host binary or instance path. Product updates keep the registration.

`stop`/`start` (#2073) halt and resume the daemon without touching that
registration — the goobers-native equivalent of the platform sections'
`systemctl stop`/`launchctl stop`/`sc.exe stop` (and their `start` pairs)
below, useful for a maintenance window that shouldn't require reinstalling.
Both are idempotent: stopping an already-stopped (or starting an
already-running) daemon succeeds as a no-op. The platform sections below
document the generated registration and its native supervisor commands for
troubleshooting.

**One shutdown contract, three triggers.** The daemon has a single
graceful-shutdown path: cancel the root context, stop admitting work, drain
in-flight runs (up to `drainGrace` = 30s + a 5s HTTP grace, see
`cmd/goobers/up.go`), then exit. Each supervisor drives that same path:

| Platform | Stop trigger | Reaches the graceful path via |
|---|---|---|
| Linux (systemd) | `systemctl stop` | `SIGTERM` (systemd default `KillSignal`) |
| macOS (launchd) | `launchctl bootout` | `SIGTERM` (launchd unload) |
| Windows (service) | `sc stop` / service stop | `SERVICE_CONTROL_STOP` → context cancel (`internal/winsvc`) |

A second SIGTERM/SIGINT force-exits immediately (the wedged-shutdown backstop in
`internal/signals`); the supervisors' hard-kill timeouts below are the final
fallback beyond that.

## Supervised self-update

The hourly workflow defaults to a checksum-verified release; `manual` pins a tag and
`on-main` builds the API-pinned commit. Candidates pass version, validation, and
config-diff checks. The host journals activation, retains the old binary, and rolls
back with escalation on failed health. Config delivery remains owned by Workflow CD.

> **Credentials & PATH.** Run the daemon as the user that owns the instance's
> provider token, so it inherits per-user credentials — this is why the Linux
> and macOS templates default to a *user* service. Remember the daemon's
> `local-ci` stage runs as a subprocess and inherits the **service's** PATH, not
> your login shell's: put the Go toolchain and `golangci-lint` on it (each
> template shows where).

---

## Linux (systemd)

Template: [`packaging/systemd/goobers.service`](https://github.com/Agent-Clubhouse/Goobers/blob/main/packaging/systemd/goobers.service)
(a **user** service — recommended, so it runs as you with your credentials).

**Native equivalent of `goobers service install`:**

```sh
mkdir -p ~/.config/systemd/user
cp packaging/systemd/goobers.service ~/.config/systemd/user/goobers.service
# Fill in the two placeholders: %GOOBERS_BIN% and %INSTANCE_ROOT%
$EDITOR ~/.config/systemd/user/goobers.service
systemctl --user daemon-reload
systemctl --user enable --now goobers
loginctl enable-linger "$USER"        # keep running after logout / across reboots
```

**Operate:**

```sh
systemctl --user start   goobers      # start (goobers service start)
systemctl --user stop    goobers      # graceful stop, unit stays enabled (goobers service stop)
systemctl --user status  goobers      # status
journalctl --user -u goobers -f       # logs (follow)
```

**Upgrade:** use `self-update`; reinstall the service only to replace its stable
host.

`TimeoutStopSec=45` in the template gives the drain window headroom before
systemd escalates to `SIGKILL`. For a **system-wide** install instead, drop the
unit in `/etc/systemd/system/`, add `User=`/`Group=`, and use `systemctl` without
`--user` (you own credential delivery to that user).

---

## macOS (launchd)

Template: [`packaging/launchd/com.agent-clubhouse.goobers.plist`](https://github.com/Agent-Clubhouse/Goobers/blob/main/packaging/launchd/com.agent-clubhouse.goobers.plist)
(a per-user **LaunchAgent**).

**Native equivalent of `goobers service install`:**

```sh
cp packaging/launchd/com.agent-clubhouse.goobers.plist \
   ~/Library/LaunchAgents/com.agent-clubhouse.goobers.plist
# Fill in the placeholders: %GOOBERS_BIN%, %INSTANCE_ROOT%, %LOG_DIR%
$EDITOR ~/Library/LaunchAgents/com.agent-clubhouse.goobers.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agent-clubhouse.goobers.plist
launchctl enable gui/$(id -u)/com.agent-clubhouse.goobers
launchctl kickstart -k gui/$(id -u)/com.agent-clubhouse.goobers
```

**Operate:**

```sh
launchctl print gui/$(id -u)/com.agent-clubhouse.goobers   # status
launchctl kickstart gui/$(id -u)/com.agent-clubhouse.goobers      # (re)start (goobers service start)
launchctl stop gui/$(id -u)/com.agent-clubhouse.goobers           # graceful stop, stays loaded (goobers service stop)
launchctl bootout gui/$(id -u)/com.agent-clubhouse.goobers        # graceful stop + unload (goobers service uninstall)
tail -f "$LOG_DIR"/goobers.err.log                                # logs
```

**Upgrade:** use `self-update`; reinstall the LaunchAgent only to replace its
stable host.

`ExitTimeOut=45` allows the drain window before launchd sends `SIGKILL`;
`KeepAlive.SuccessfulExit=false` restarts on crashes without fighting an operator
stop.

---

## Windows (Windows Service)

The stable host uses [`internal/winsvc`](https://github.com/Agent-Clubhouse/Goobers/tree/main/internal/winsvc) to translate SCM
stop/shutdown controls into supervisor cancellation, then writes the same
cross-platform daemon drain request used for update handoffs.

Run `goobers service install <instance-root>` from an elevated PowerShell or
Command Prompt. Its native equivalent is below. First put
`goobers.exe` on disk — download and verify a release per the
[Windows quickstart](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-windows.md), placing it at
`C:\Program Files\goobers\goobers.exe` (the path the service below references):

```powershell
# Create the service (note the spaces after '=' in sc.exe syntax):
sc.exe create goobers binPath= "\"C:\Program Files\goobers\goobers.exe\" __service-supervise \"C:\ProgramData\goobers\instance\"" start= auto DisplayName= "Goobers daemon"
sc.exe description goobers "Goobers agent-workforce daemon (scheduler + local runner)"
sc.exe failure goobers reset= 86400 actions= restart/5000/restart/30000/restart/60000
sc.exe failureflag goobers 1
sc.exe start goobers
```

**Operate:**

```powershell
sc.exe query   goobers      # status
sc.exe stop    goobers      # graceful stop, registration kept (goobers service stop)
sc.exe start   goobers      # start (goobers service start)
sc.exe delete  goobers      # uninstall (stop first) (goobers service uninstall)
```

Logs go to the console the SCM captures; use the daemon's own journal
(`goobers trace`, `goobers status --daemon`) for run-level detail. Configure the
service account (`sc.exe config goobers obj= …`) so the daemon runs as the user
whose credentials the instance references.

> **Status (#639 / #633 / #752).** The handler is build-tag-gated
> (`//go:build windows`) and its compile is guaranteed on every PR by the
> `linux node validation` CI job's `GOOS=windows go build ./internal/winsvc/...`
> step. Full-binary Windows cross-compilation and live start/stop verification
> ride the Windows POSIX-abstraction work (#620–#627), the Windows CI leg
> (#633), and a live Windows environment (#752). Until then, treat the Windows
> Service wiring as reviewed-and-compiling, runtime-pending.

---

## Dirty restart journal event

Every daemon lifetime appends `daemon.started` and a successful graceful drain
appends `daemon.clean_shutdown` to `scheduler/events.jsonl`. If the supervisor
restarts Goobers after an abrupt termination, the persistent `scheduler/up.lock`
has no matching clean-shutdown event. Startup then appends
`daemon.dirty_restart` with reason
`previous daemon lock remained without a clean-shutdown event` and includes the
previous daemon identity under `runner`.
