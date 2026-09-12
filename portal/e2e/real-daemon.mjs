import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const portalRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repositoryRoot = resolve(portalRoot, "..");
const temporaryRoot = mkdtempSync(join(tmpdir(), "goobers-real-e2e-"));
const cacheRoot = join(portalRoot, "node_modules", ".cache", "real-daemon");
mkdirSync(cacheRoot, { recursive: true });
const binary = join(cacheRoot, process.platform === "win32" ? "goobers.exe" : "goobers");
const instance = join(temporaryRoot, "instance");
const port = process.env.PORTAL_E2E_REAL_PORT ?? "4174";
const shutdownRequest = join(cacheRoot, `shutdown-${port}.request`);
const shutdownAck = join(cacheRoot, `shutdown-${port}.ack`);
rmSync(shutdownRequest, { force: true });
rmSync(shutdownAck, { force: true });

let cleaned = false;
function cleanup() {
  if (cleaned) return;
  cleaned = true;
  rmSync(temporaryRoot, { force: true, recursive: true });
}

function run(args, options = {}) {
  const result = spawnSync(binary, args, {
    cwd: repositoryRoot,
    encoding: "utf8",
    stdio: "inherit",
    ...options,
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(`goobers ${args[0]} exited with status ${result.status}`);
  }
}

try {
  const build = spawnSync(
    "go",
    ["build", "-tags", "embed_portal", "-o", binary, "./cmd/goobers"],
    { cwd: repositoryRoot, encoding: "utf8", stdio: "inherit" },
  );
  if (build.error) throw build.error;
  if (build.status !== 0) throw new Error(`go build exited with status ${build.status}`);

  run(["init", "--allow-ephemeral", "--demo", "--insecure", instance]);
  run(["run", "demo", instance], {
    env: { ...process.env, GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE: "1" },
  });
} catch (error) {
  cleanup();
  throw error;
}

const dashboard = spawn(binary, ["dashboard", `--port=${port}`, "--no-open", instance], {
  cwd: repositoryRoot,
  env: process.env,
  stdio: "inherit",
});

// Playwright force-kills web servers on Windows, where process signals cannot
// run cleanup handlers. Global teardown writes a cache-local sentinel; this
// wrapper stops the child and acknowledges only after removing the instance.
// No extra TCP listener or fixed control port can collide with another process.
const shutdownPoll = setInterval(() => {
  if (!existsSync(shutdownRequest)) return;
  stop();
}, 100);

let stopping = false;
function stop(signal) {
  if (stopping) return;
  stopping = true;
  // Playwright may terminate the wrapper before asynchronous child-exit
  // callbacks run. Remove the disposable instance synchronously first; the
  // executable lives in node_modules/.cache so Windows never has to unlink a
  // running binary.
  cleanup();
  if (!dashboard.killed) dashboard.kill(signal);
}

process.on("SIGINT", () => stop("SIGINT"));
process.on("SIGTERM", () => stop("SIGTERM"));

dashboard.on("error", (error) => {
  console.error(error);
  cleanup();
  clearInterval(shutdownPoll);
  process.exitCode = 1;
});
dashboard.on("exit", (code, signal) => {
  cleanup();
  clearInterval(shutdownPoll);
  if (stopping) writeFileSync(shutdownAck, "stopped\n");
  if (!stopping && (code ?? 1) !== 0) {
    console.error(`real dashboard exited unexpectedly (${signal ?? code})`);
    process.exitCode = code ?? 1;
  }
});
