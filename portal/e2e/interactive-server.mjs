import { spawn, execFileSync } from "node:child_process";
import { existsSync, mkdirSync, writeFileSync, appendFileSync, readFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repository = resolve(process.env.INTERACTIVE_REPOSITORY_ROOT ?? resolve(dirname(fileURLToPath(import.meta.url)), "../.."));
const evidence = resolve(process.env.INTERACTIVE_EVIDENCE_DIR ?? `${tmpdir()}/goobers-interactive-evidence`);
const state = resolve(process.env.INTERACTIVE_STATE_DIR ?? `${evidence}/server-state`);
const port = process.env.INTERACTIVE_PORT ?? "8097";
if (!/^\d+$/.test(port)) throw new Error("INTERACTIVE_PORT must be numeric");
if (existsSync(state)) throw new Error(`Qualification requires fresh state; choose a new INTERACTIVE_STATE_DIR: ${state}`);
mkdirSync(evidence, { recursive: true });
const startedAt = new Date().toISOString();
const start = performance.now();
const args = ["--listen", `127.0.0.1:${port}`, "--state-dir", state,
  "--source", "samples/interactive-investigation", "--assets", "internal/portalassets/dist",
  "--goobers-bin", ".tools/goobers", "--executor", "local-tools"];
// This fixture deliberately cannot opt into an external model. Its only
// executable work is the local-tools executor and native deterministic workflows.
const server = spawn(resolve(repository, ".tools/interactive-prototype"), args, { cwd: repository });
let exited = false;
let stopping = false;
for (const [stream, output] of [[server.stdout, process.stdout], [server.stderr, process.stderr]]) {
  stream.on("data", chunk => { appendFileSync(`${evidence}/server.log`, chunk); output.write(chunk); });
}
server.on("error", error => { console.error(error); process.exitCode = 1; });
server.on("exit", (code, signal) => {
  exited = true;
  if (!stopping && code !== 0) { console.error(`Prototype server exited: ${signal ?? code}`); process.exitCode = code || 1; }
});
for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, () => { stopping = true; server.kill(signal); });
let runtime;
while (performance.now() - start < 30_000 && !exited) {
  try {
    const response = await fetch(`http://127.0.0.1:${port}/api/v1/interactive/status`);
    if (response.ok) { runtime = await response.json(); break; }
  } catch { /* Server is not listening yet. */ }
  await new Promise(resolve => setTimeout(resolve, 50));
}
if (!runtime) {
  server.kill("SIGTERM");
  throw new Error("Prototype did not become ready within 30 seconds");
}
const startupMs = performance.now() - start;
const commit = execFileSync("git", ["rev-parse", "HEAD"], { cwd: repository, encoding: "utf8" }).trim();
const dirtyPaths = execFileSync("git", ["status", "--porcelain"], { cwd: repository, encoding: "utf8" }).trim().split("\n").filter(Boolean);
const binarySha256 = createHash("sha256").update(readFileSync(resolve(repository, ".tools/interactive-prototype"))).digest("hex");
writeFileSync(`${evidence}/startup.json`, JSON.stringify({ startedAt, startupMs, repository, commit, dirtyPaths, binarySha256, node: process.version, serverPID: server.pid,
  args, runtime, qualification: "local-tools only; real deterministic repository/workflow execution; controlled providers", state,
}, null, 2));
console.log(`Qualification backend ready in ${startupMs.toFixed(1)} ms; evidence: ${evidence}`);
