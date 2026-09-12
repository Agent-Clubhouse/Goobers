import { spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
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
const controlPort = Number.parseInt(process.env.PORTAL_E2E_CONTROL_PORT ?? "4175", 10);

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
// run cleanup handlers. Its global teardown asks this private loopback control
// server to stop the dashboard first, so cleanup completes on every platform.
const control = createServer((request, response) => {
  if (request.method !== "POST" || request.url !== "/shutdown") {
    response.writeHead(404).end();
    return;
  }
  stopping = true;
  const force = setTimeout(() => dashboard.kill("SIGKILL"), 5_000);
  dashboard.once("exit", () => {
    clearTimeout(force);
    cleanup();
    response.writeHead(200).end("stopped\n");
    control.close();
  });
  if (!dashboard.killed) dashboard.kill();
});
control.listen(controlPort, "127.0.0.1");

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
  control.close();
}

process.on("SIGINT", () => stop("SIGINT"));
process.on("SIGTERM", () => stop("SIGTERM"));

dashboard.on("error", (error) => {
  console.error(error);
  cleanup();
  control.close();
  process.exitCode = 1;
});
dashboard.on("exit", (code, signal) => {
  cleanup();
  if (!stopping && (code ?? 1) !== 0) {
    console.error(`real dashboard exited unexpectedly (${signal ?? code})`);
    process.exitCode = code ?? 1;
  }
});
