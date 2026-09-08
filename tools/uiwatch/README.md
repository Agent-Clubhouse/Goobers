# uiwatch — local dashboard monitor

A read-only synthetic monitor for the local Goobers dashboard, with a local
finding ledger. Phase one: it **detects and tracks**. It does not file GitHub
issues and holds no credentials to do so.

## Why it exists

The portal's e2e suite runs chromium only, against a fixture daemon, in CI.
Nothing watches the *real* dashboard, and nothing runs a second browser
engine — which is how a Firefox-only SSE defect reached production. This
closes both gaps cheaply: a few seconds of browser time every few minutes.

## Run it

From the repo root — `./monitor` on unix, `monitor` on Windows:

```
./monitor start     begin watching in the background
./monitor stop      stop watching
./monitor           is it running, how many findings are open
./monitor status    list tracked findings
```

Everything else goes through the same wrapper:

```bash
./monitor run           # probe once, update the ledger
./monitor session --minutes 30   # long-session drift watch, interactive
./monitor show <id>     # full record incl. evidence paths
./monitor ack <id>      # stop it counting as needing review
```

Or call the CLI directly (`node tools/uiwatch/uiwatch.mjs <cmd>`) — the
wrappers add nothing but a shorter name and a `node` check.

### How "continuous" works

`start` spawns a detached Node process that probes every `intervalSeconds`
(default 300), writing to `uiwatch.log` in the state dir. It is a plain
process with a PID file, not a platform service — which is why the same two
commands work on Linux, macOS and Windows with no install step.

The tradeoff: it does not survive a reboot. For that, use the systemd units
(below) on Linux. `start` refuses to run if the systemd timer is already
active, since both would write to the same ledger.

`stop` sends SIGTERM; the loop finishes its current probe, closes its
browsers, and exits.

A run takes ~5s for two engines. `run` exits 0 even when checks fail — pass
`--fail-on-finding` to make it exit non-zero when something is promoted.

Playwright is located automatically: `portal/node_modules`, then the repo, then
a global install, then the npx cache. Nothing needs to be on `PATH` and
`NODE_PATH` is not used (it does not affect ESM imports).

## What it checks

| kind | check |
|------|-------|
| `daemon` | `/api/v1/health` responds 200 |
| `route` | every configured route renders its expected heading |
| `sse` | the live-update badge reaches "connected" within the threshold |
| `console` | no console errors or unhandled rejections |
| `network` | no failed requests |
| `http` | no 4xx/5xx responses |
| `readonly` | the page issued no non-GET request |

The `sse` check is the one that catches the defect class described above: the
stream can be fine at the HTTP level while the client still shows
"reconnecting".

## Long-session mode

Everything above reloads the page, so it only ever sees a **cold** dashboard.
A tab left open for hours fails differently — heap and DOM nodes accumulate, a
stream dies on suspend/resume and reconnects badly, a view drifts after
hundreds of incremental updates. Reloading can never observe any of it.

So `watch` also holds **one page open across cycles** and samples it:

| kind | fires when |
|------|-----------|
| `session-dom` | DOM node count grew past `maxNodeGrowthPct` |
| `session-heap` | JS heap grew past `maxHeapGrowthPct` (chromium only) |
| `session-sse` | the event stream reconnected more than `maxSseReconnects` times |
| `session-flap` | the connection badge changed state more than `maxBadgeFlaps` times |
| `session-state` | the badge is currently showing disconnected |
| `session-console` | any console error appeared during the session |

Run one interactively for a bounded time:

```bash
./monitor session --minutes 30
```

```
   60s  uptime 60s  nodes 562 (0%)  heap n/a  rtt 3ms  sse 0 reconnects  flaps 0  state up  errors 0
```

Notes on the measurements:

- **Heap and listener counts come from CDP and are chromium-only.** Firefox
  and WebKit report `heap n/a`; they still get node counts, flaps, reconnects
  and console errors. Run the session in chromium when hunting a leak.
- **The badge counter lives in the page**, ticking once a second, because a
  flap that resolves between samples would otherwise be invisible.
- **The baseline is the median of the first few samples**, so one cold outlier
  (first paint, lazy chunks) cannot look like a leak. No judgement is made
  until `minSamples` have been taken.
- **The session recycles every `restartAfterHours`** so the monitor's own page
  cannot become the thing that leaks.
- `rtt` is the round-trip of a trivial `evaluate` — a cheap proxy for main
  thread responsiveness.

Session findings go into the **same ledger** with the same fingerprinting,
streak threshold and recovery rules.

## It cannot modify anything

The probe never clicks, fills, or submits — it only navigates and reads. That
is deliberate: the live attention queue has `Dismiss` buttons, and a monitor
that could press one is a monitor that can destroy real state.

Non-GET requests are *observed and reported*, not intercepted and blocked.
Blocking would route every request through Playwright, which proxies response
bodies and is unreliable for SSE — and the health of that stream is one of the
things being measured. Observation cannot break what it watches.

## Findings, not failures

A failing check is not a finding. Real systems blip: a daemon restart, a slow
first paint. A check must fail `minConsecutiveFailures` **runs** in a row
(default 2) before it is promoted to `open` and worth a human's attention.

Findings are fingerprinted so one defect stays one entry. The fingerprint
normalizes volatile text — query strings, timestamps, ULIDs, durations — but
deliberately preserves status codes, because a 503 and a 404 on the same
endpoint are different defects.

Statuses: `watching` → `open` (promoted) → `acknowledged` (by you) →
`recovered` (stopped failing).

## State

Everything lives in `$XDG_STATE_HOME/goobers-uiwatch` (default
`~/.local/state/goobers-uiwatch`) — outside the repo, so the monitor never
dirties `git status` and its history survives branch switches.

```
findings/<id>/meta.json   fingerprint, streak, status, evidence paths
evidence/<timestamp>/     trace-<browser>.zip, screen-<browser>.png
runs/<timestamp>.json     every probe result
```

Traces open with `npx playwright show-trace <path>` and carry DOM snapshots
per step — usually decisive for a visual or timing bug.

A failing run writes ~1.5MB. Retention is bounded (`config.json` →
`retention`), and evidence referenced by an open or acknowledged finding is
never pruned.

## Surviving a reboot (Linux)

```bash
mkdir -p ~/.config/systemd/user
cp tools/uiwatch/systemd/goobers-uiwatch.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now goobers-uiwatch.timer

systemctl --user list-timers goobers-uiwatch.timer
journalctl --user -u goobers-uiwatch.service -n 50
```

The service is sandboxed: `ProtectHome=read-only` with write access only to
its own state directory and the Playwright browser cache.

## Configuration

`config.json`, overridable per-run by environment:
`UIWATCH_BASE_URL`, `UIWATCH_DAEMON_URL`, `UIWATCH_BROWSERS`,
`UIWATCH_STATE_DIR`. Point it at the fixture daemon instead of the live
instance with:

```bash
UIWATCH_BASE_URL=http://127.0.0.1:4173 node tools/uiwatch/uiwatch.mjs run
```

## Not in phase one

Agent triage, generated repro specs, and gated GitHub issue filing.
