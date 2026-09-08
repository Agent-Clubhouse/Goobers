// Start/stop/status for a background monitor loop.
//
// Deliberately a plain Node process with a PID file rather than a platform
// service. systemd exists only on Linux, launchd only on macOS, Scheduled
// Tasks only on Windows — a supervisor written this way behaves identically on
// all three and needs no install step. The systemd units in ./systemd remain
// the option for surviving a reboot.
import { spawn, execFileSync } from "node:child_process";
import { openSync, existsSync, readFileSync, writeFileSync, rmSync, mkdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { stateDir } from "./ledger.mjs";

const cli = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "uiwatch.mjs");

const pidFile = (config) => path.join(stateDir(config), "uiwatch.pid");
const logFile = (config) => path.join(stateDir(config), "uiwatch.log");

/** Liveness without killing: signal 0 tests existence and permission only. */
function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return e.code === "EPERM";
  }
}

export function readPid(config) {
  const file = pidFile(config);
  if (!existsSync(file)) return null;
  const pid = Number.parseInt(readFileSync(file, "utf8").trim(), 10);
  if (!Number.isInteger(pid) || !alive(pid)) {
    rmSync(file, { force: true }); // Stale: process died without cleaning up.
    return null;
  }
  return pid;
}

/** On Linux, warn rather than silently double-probe alongside the timer. */
function systemdTimerActive() {
  if (process.platform !== "linux") return false;
  try {
    const out = execFileSync("systemctl", ["--user", "is-active", "goobers-uiwatch.timer"], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    });
    return out.trim() === "active";
  } catch {
    return false;
  }
}

export function start(config) {
  const running = readPid(config);
  if (running) return { ok: true, already: true, pid: running };
  if (systemdTimerActive()) {
    return {
      ok: false,
      reason:
        "the goobers-uiwatch systemd timer is already active; both would write to the same ledger.\n" +
        "  Stop it with: systemctl --user disable --now goobers-uiwatch.timer",
    };
  }

  mkdirSync(stateDir(config), { recursive: true });
  const out = openSync(logFile(config), "a");
  const child = spawn(process.execPath, [cli, "watch"], {
    detached: true,
    stdio: ["ignore", out, out],
    windowsHide: true,
  });
  child.unref();
  writeFileSync(pidFile(config), String(child.pid));
  return { ok: true, pid: child.pid, log: logFile(config) };
}

export function stop(config) {
  const pid = readPid(config);
  if (!pid) return { ok: true, already: true };
  try {
    // SIGTERM lets the loop finish its current probe and close browsers; on
    // Windows Node has no signals and this terminates immediately, which is
    // acceptable because a probe leaves nothing to corrupt.
    process.kill(pid, "SIGTERM");
  } catch (e) {
    return { ok: false, reason: e.message };
  }
  // Portable synchronous sleep: Atomics.wait blocks this thread without
  // spawning anything. Waits up to ~10s for the loop to finish its probe.
  const clock = new Int32Array(new SharedArrayBuffer(4));
  for (let i = 0; i < 100 && alive(pid); i += 1) Atomics.wait(clock, 0, 0, 100);
  rmSync(pidFile(config), { force: true });
  return { ok: true, pid, stillRunning: alive(pid) };
}

export function state(config) {
  return {
    pid: readPid(config),
    systemdTimer: systemdTimerActive(),
    log: logFile(config),
    stateDir: stateDir(config),
  };
}
