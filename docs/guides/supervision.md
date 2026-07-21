# Daemon supervision (systemd · launchd · Windows Service)

Runs the `goobers` daemon (`goobers up`) as a supervised, auto-restarting
background service on each platform, instead of a foreground shell. This
resolves **DEP-Q6** (`docs/requirements/deployment.md`): tier 1–2 daemon
supervision is **systemd** on Linux, **launchd** on macOS, and a **Windows
Service** wrapper on Windows.

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

> **Credentials & PATH.** Run the daemon as the user that owns the instance's
> provider token, so it inherits per-user credentials — this is why the Linux
> and macOS templates default to a *user* service. Remember the daemon's
> `local-ci` stage runs as a subprocess and inherits the **service's** PATH, not
> your login shell's: put the Go toolchain and `golangci-lint` on it (each
> template shows where).

---

## Linux (systemd)

Template: [`packaging/systemd/goobers.service`](../../packaging/systemd/goobers.service)
(a **user** service — recommended, so it runs as you with your credentials).

**Install / enable:**

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
systemctl --user start   goobers      # start
systemctl --user stop    goobers      # graceful stop (SIGTERM → drain)
systemctl --user status  goobers      # status
journalctl --user -u goobers -f       # logs (follow)
```

**Upgrade:** replace the binary, then `systemctl --user restart goobers`. If you
edit the unit file, `systemctl --user daemon-reload` first.

`TimeoutStopSec=45` in the template gives the drain window headroom before
systemd escalates to `SIGKILL`. For a **system-wide** install instead, drop the
unit in `/etc/systemd/system/`, add `User=`/`Group=`, and use `systemctl` without
`--user` (you own credential delivery to that user).

---

## macOS (launchd)

Template: [`packaging/launchd/com.agent-clubhouse.goobers.plist`](../../packaging/launchd/com.agent-clubhouse.goobers.plist)
(a per-user **LaunchAgent**).

**Install / enable:**

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
launchctl kickstart -k gui/$(id -u)/com.agent-clubhouse.goobers   # (re)start
launchctl bootout gui/$(id -u)/com.agent-clubhouse.goobers        # graceful stop (SIGTERM)
tail -f "$LOG_DIR"/goobers.err.log                                # logs
```

**Upgrade:** replace the binary, then `launchctl kickstart -k …`. If you edit the
plist, `launchctl bootout …` then `launchctl bootstrap …` again.

`ExitTimeOut=45` allows the drain window before launchd sends `SIGKILL`;
`KeepAlive.SuccessfulExit=false` restarts on crashes without fighting an operator
stop.

---

## Windows (Windows Service)

Unlike the unix unit files, Windows services are not config-only — the process
must speak the Service Control Manager (SCM) protocol. That handler ships in
[`internal/winsvc`](../../internal/winsvc): when `goobers up` detects it was
launched by the SCM (`svc.IsWindowsService()`), it runs under the SCM and
translates `SERVICE_CONTROL_STOP`/`SHUTDOWN` into the **same** context
cancellation `SIGTERM` drives on unix — so the graceful drain is identical. Off
Windows the handler is a no-op stub, so the unix signal path is untouched.

**Install / enable** (from an elevated PowerShell or Command Prompt). First put
`goobers.exe` on disk — download and verify a release per the
[Windows quickstart](quickstart-windows.md), placing it at
`C:\Program Files\goobers\goobers.exe` (the path the service below references):

```powershell
# Create the service (note the spaces after '=' in sc.exe syntax):
sc.exe create goobers binPath= "\"C:\Program Files\goobers\goobers.exe\" up \"C:\ProgramData\goobers\instance\"" start= auto DisplayName= "Goobers daemon"
sc.exe description goobers "Goobers agent-workforce daemon (scheduler + local runner)"
sc.exe start goobers
```

**Operate:**

```powershell
sc.exe query   goobers      # status
sc.exe stop    goobers      # graceful stop (SERVICE_CONTROL_STOP → drain)
sc.exe start   goobers      # start
sc.exe delete  goobers      # uninstall (stop first)
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

## Stage 2 (deferred): a `goobers service` subcommand

A `goobers service install|uninstall|status` subcommand that writes/registers
these units natively per platform is an explicitly deferred follow-up (see #639,
"Stage 2"). Today's supported path is the documented, copy-in unit files above.
