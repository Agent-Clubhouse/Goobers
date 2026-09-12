import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const portalRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repositoryRoot = resolve(portalRoot, "..");
const temporaryRoot = mkdtempSync(join(tmpdir(), "goobers-real-e2e-"));
const binary = join(temporaryRoot, process.platform === "win32" ? "goobers.exe" : "goobers");
const instance = join(temporaryRoot, "instance");
const port = process.env.PORTAL_E2E_REAL_PORT ?? "4174";

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

const dashboard = spawn(binary, ["dashboard", `--port=${port}`, "--no-open", instance], {
  cwd: repositoryRoot,
  env: process.env,
  stdio: "inherit",
});

let stopping = false;
function stop(signal) {
  if (stopping) return;
  stopping = true;
  if (!dashboard.killed) dashboard.kill(signal);
}

process.on("SIGINT", () => stop("SIGINT"));
process.on("SIGTERM", () => stop("SIGTERM"));

dashboard.on("error", (error) => {
  console.error(error);
  rmSync(temporaryRoot, { force: true, recursive: true });
  process.exitCode = 1;
});
dashboard.on("exit", (code, signal) => {
  rmSync(temporaryRoot, { force: true, recursive: true });
  if (!stopping && (code ?? 1) !== 0) {
    console.error(`real dashboard exited unexpectedly (${signal ?? code})`);
    process.exitCode = code ?? 1;
  }
});
