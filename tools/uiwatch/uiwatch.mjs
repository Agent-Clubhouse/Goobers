#!/usr/bin/env node
// goobers-uiwatch — read-only synthetic monitor for the local Goobers
// dashboard, with a local finding ledger.
//
// Phase one: detect and track. It does not file GitHub issues and holds no
// credentials to do so.
import { mkdirSync, readFileSync, existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { runProbe } from "./lib/probe.mjs";
import { record, listFindings, setStatus, stateDir, prune } from "./lib/ledger.mjs";
import { start, stop, state } from "./lib/supervisor.mjs";
import { LongSession } from "./lib/session.mjs";
import { loadPlaywright } from "./lib/resolve-playwright.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));

function loadConfig(overridePath) {
  const file = overridePath ?? path.join(here, "config.json");
  const config = JSON.parse(readFileSync(file, "utf8"));
  for (const [key, value] of Object.entries({
    UIWATCH_BASE_URL: "baseUrl",
    UIWATCH_DAEMON_URL: "daemonUrl",
    UIWATCH_STATE_DIR: "stateDir",
  })) {
    if (process.env[key]) config[value] = process.env[key];
  }
  if (process.env.UIWATCH_BROWSERS) {
    config.browsers = process.env.UIWATCH_BROWSERS.split(",").map((s) => s.trim());
  }
  return config;
}

const STATUS_MARK = { watching: "·", open: "!", acknowledged: "~", recovered: "+", filed: ">" };

function fmt(meta) {
  const mark = STATUS_MARK[meta.status] ?? "?";
  const where = meta.browser === "-" ? meta.route : `${meta.browser} ${meta.route}`;
  return `${mark} ${meta.id}  ${meta.kind.padEnd(8)} ${where.padEnd(28)} x${String(meta.occurrences).padStart(3)} (streak ${meta.consecutive})  ${meta.signature.slice(0, 60)}`;
}

async function cmdRun(config, args, session = null) {
  const dir = stateDir(config);
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const evidenceDir = path.join(dir, "evidence", stamp);
  mkdirSync(evidenceDir, { recursive: true });

  const result = await runProbe(config, evidenceDir);

  // Long-session checks join the SAME ledger, so drift findings get the same
  // fingerprinting, streak threshold and recovery handling as everything else.
  if (session) {
    try {
      const s = await session.sample();
      result.checks.push(...session.checks());
      result.session = s;
      console.log(`  session: ${session.summary()}`);
    } catch (e) {
      console.log(`  session sample failed: ${String(e?.message ?? e).split("\n")[0]}`);
    }
  }
  const failed = result.checks.filter((c) => !c.ok);
  const { promoted, cleared } = record(config, result);

  const total = result.checks.length;
  console.log(`uiwatch ${result.at}  ${total - failed.length}/${total} checks passed`);
  for (const c of failed) {
    console.log(`  FAIL ${c.kind} ${c.browser} ${c.route}: ${String(c.detail).slice(0, 120)}`);
  }
  for (const p of promoted) {
    console.log(`  >> PROMOTED ${p.id} (${p.consecutive} consecutive) — needs review`);
  }
  for (const c of cleared) {
    console.log(`  >> RECOVERED ${c.id}`);
  }
  if (failed.length === 0) console.log("  all clear");

  const swept = prune(config, config.retention);
  if (swept.runs || swept.evidence) {
    console.log(`  pruned ${swept.runs} run records, ${swept.evidence} evidence dirs`);
  }

  // A monitor must not fail its own timer on a transient blip; only an
  // explicit flag makes this exit non-zero, for interactive use.
  if (args.includes("--fail-on-finding") && promoted.length) process.exit(1);
}

function cmdStatus(config) {
  const findings = listFindings(config);
  if (!findings.length) return console.log("no findings recorded");
  const open = findings.filter((f) => f.status === "open");
  console.log(`${findings.length} tracked, ${open.length} open   [ ! open  ~ ack  + recovered  · watching ]`);
  console.log(`state: ${stateDir(config)}`);
  for (const f of findings) console.log(fmt(f));
}

function cmdShow(config, id) {
  const meta = listFindings(config).find((f) => f.id === id || f.id.startsWith(id));
  if (!meta) return console.log(`no finding ${id}`);
  console.log(JSON.stringify(meta, null, 2));
}

async function cmdSession(config, rest) {
  const minutes = Number(rest[rest.indexOf("--minutes") + 1]) || 20;
  const every = (config.session?.sampleSeconds ?? 60) * 1000;
  const { playwright } = loadPlaywright();
  const session = new LongSession(playwright, config);

  console.log(`[session] opening ${session.browserName} on ${config.baseUrl} for ${minutes}m`);
  await session.open();
  const until = Date.now() + minutes * 60_000;
  try {
    while (Date.now() < until) {
      await new Promise((r) => setTimeout(r, every));
      const s = await session.sample();
      console.log(`  ${String(s.uptimeSec).padStart(5)}s  ${session.summary()}`);
    }
    const failed = session.checks().filter((c) => !c.ok);
    console.log(failed.length ? "\nfindings:" : "\nno drift detected");
    for (const f of failed) console.log(`  ${f.kind}: ${f.detail}`);
  } finally {
    await session.close();
  }
}

async function cmdWatch(config) {
  const seconds = config.intervalSeconds ?? 300;
  let stopping = false;
  const quit = (sig) => {
    console.log(`[watch] ${sig} received; finishing current cycle`);
    stopping = true;
  };
  process.on("SIGTERM", () => quit("SIGTERM"));
  process.on("SIGINT", () => quit("SIGINT"));

  console.log(`[watch] started pid=${process.pid} interval=${seconds}s`);

  // The long session lives ACROSS cycles — that is the entire point. It is
  // recycled periodically so the monitor's own page cannot be the thing that
  // leaks, and so a wedged session eventually heals itself.
  let session = null;
  const sessionCfg = config.session ?? {};
  const openSession = async () => {
    if (!sessionCfg.enabled) return null;
    try {
      const { playwright } = loadPlaywright();
      const s = new LongSession(playwright, config);
      await s.open();
      console.log(`[watch] long session opened (${s.browserName})`);
      return s;
    } catch (e) {
      console.log(`[watch] long session failed to open: ${String(e?.message ?? e).split("\n")[0]}`);
      return null;
    }
  };
  session = await openSession();

  while (!stopping) {
    try {
      await cmdRun(config, [], session);
    } catch (e) {
      // A crashed probe must not kill the monitor; the next cycle may succeed.
      console.log(`[watch] probe error: ${String(e?.message ?? e).split("\n")[0]}`);
    }

    if (session) {
      const ageH = (Date.now() - session.openedAt) / 3.6e6;
      if (ageH >= (sessionCfg.restartAfterHours ?? 12)) {
        console.log(`[watch] recycling long session after ${ageH.toFixed(1)}h`);
        await session.close();
        session = await openSession();
      }
    }
    for (let i = 0; i < seconds && !stopping; i += 1) {
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
  console.log("[watch] stopped");
  await session?.close();
}

const [, , cmd = "run", ...rest] = process.argv;
const configFlag = rest.indexOf("--config");
const config = loadConfig(configFlag >= 0 ? rest[configFlag + 1] : undefined);

switch (cmd) {
  case "run":
    await cmdRun(config, rest);
    break;
  case "status":
    cmdStatus(config);
    break;
  case "watch":
    await cmdWatch(config);
    break;
  case "session":
    await cmdSession(config, rest);
    break;
  case "start": {
    const r = start(config);
    if (!r.ok) {
      console.error(`cannot start: ${r.reason}`);
      process.exit(1);
    }
    console.log(
      r.already
        ? `monitor already running (pid ${r.pid})`
        : `monitor started (pid ${r.pid})\n  log: ${r.log}`,
    );
    break;
  }
  case "stop": {
    const r = stop(config);
    if (!r.ok) {
      console.error(`cannot stop: ${r.reason}`);
      process.exit(1);
    }
    console.log(r.already ? "monitor was not running" : `monitor stopped (pid ${r.pid})`);
    break;
  }
  case "state": {
    const st = state(config);
    console.log(`monitor:       ${st.pid ? `running (pid ${st.pid})` : "stopped"}`);
    console.log(`systemd timer: ${st.systemdTimer ? "active" : "inactive"}`);
    console.log(`log:           ${st.log}`);
    console.log(`state dir:     ${st.stateDir}`);
    const open = listFindings(config).filter((f) => f.status === "open").length;
    console.log(`open findings: ${open}`);
    break;
  }
  case "show":
    cmdShow(config, rest[0]);
    break;
  case "ack":
    console.log(setStatus(config, rest[0], "acknowledged") ? `acknowledged ${rest[0]}` : "not found");
    break;
  case "resolve":
    console.log(setStatus(config, rest[0], "recovered") ? `resolved ${rest[0]}` : "not found");
    break;
  default:
    console.log("usage: uiwatch.mjs [start|stop|state|watch|session|run|status|show <id>|ack <id>|resolve <id>]\n              [--config f] [--fail-on-finding]");
    process.exit(2);
}
