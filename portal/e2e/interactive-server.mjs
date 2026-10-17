import { spawn, execFileSync } from "node:child_process";
import { existsSync, mkdirSync, writeFileSync, appendFileSync, readFileSync, cpSync } from "node:fs";
import { createHash } from "node:crypto";
import { createConnection } from "node:net";
import { tmpdir } from "node:os";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repository = resolve(process.env.INTERACTIVE_REPOSITORY_ROOT ?? resolve(dirname(fileURLToPath(import.meta.url)), "../.."));
const evidence = resolve(process.env.INTERACTIVE_EVIDENCE_DIR ?? `${tmpdir()}/goobers-interactive-evidence`);
const state = resolve(process.env.INTERACTIVE_STATE_DIR ?? `${evidence}/server-state`);
const configSource = resolve(process.env.INTERACTIVE_CONFIG_SOURCE ?? `${repository}/samples/interactive-config`);
const config = resolve(`${evidence}/config`);
const port = process.env.INTERACTIVE_PORT ?? "8097";
if (!/^\d+$/.test(port)) throw new Error("INTERACTIVE_PORT must be numeric");
if (existsSync(state)) throw new Error(`Qualification requires fresh state; choose a new INTERACTIVE_STATE_DIR: ${state}`);
if (existsSync(config)) throw new Error(`Qualification requires a fresh config copy; choose a new evidence directory: ${config}`);
await new Promise((resolveProbe, reject) => {
  const socket = createConnection({ host: "127.0.0.1", port: Number(port) });
  socket.once("connect", () => { socket.destroy(); reject(new Error(`Port ${port} is already occupied; refusing to test another server`)); });
  socket.once("error", error => { socket.destroy(); if (error.code === "ECONNREFUSED") resolveProbe(); else reject(error); });
  socket.setTimeout(2000, () => { socket.destroy(); reject(new Error("Local listener probe timed out")); });
});
mkdirSync(evidence, { recursive: true });
cpSync(configSource, config, { recursive: true, errorOnExist: true });
const startedAt = new Date().toISOString();
const start = performance.now();
const args = ["--listen", `127.0.0.1:${port}`, "--state-dir", state,
  "--source", "samples/interactive-investigation", "--assets", "internal/portalassets/dist",
  "--goobers-bin", ".tools/goobers", "--config-dir", config];
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
if (runtime.capabilities?.modelEnabled !== false || runtime.capabilities?.liveModel !== false ||
  runtime.capabilities?.liveDelegatedAuth !== false || runtime.executionModes?.some(mode => mode.id === "codex-local" && mode.available)) {
  server.kill("SIGTERM");
  throw new Error("Qualification refuses a host with model access or live provider claims enabled");
}
const startupMs = performance.now() - start;
const commit = execFileSync("git", ["rev-parse", "HEAD"], { cwd: repository, encoding: "utf8" }).trim();
const dirtyPaths = execFileSync("git", ["status", "--porcelain"], { cwd: repository, encoding: "utf8" }).trim().split("\n").filter(Boolean);
const binarySha256 = createHash("sha256").update(readFileSync(resolve(repository, ".tools/interactive-prototype"))).digest("hex");
const assetsRoot = resolve(repository, "internal/portalassets/dist");
const assetsIndex = readFileSync(resolve(assetsRoot, "index.html"));
const assets = { indexSha256: createHash("sha256").update(assetsIndex).digest("hex"), files: [] };
for (const match of assetsIndex.toString("utf8").matchAll(/(?:src|href)="(\/assets\/[^".][^"]*)"/g)) {
  assets.files.push({ path: match[1], sha256: createHash("sha256").update(readFileSync(resolve(assetsRoot, `.${match[1]}`))).digest("hex") });
}
writeFileSync(`${evidence}/startup.json`, JSON.stringify({ startedAt, startupMs, repository, commit, dirtyPaths, binarySha256, node: process.version, serverPID: server.pid,
  args, runtime, assets, qualification: "explicit local-tools sessions only; real deterministic repository/workflow execution; controlled providers", state, configSource, config,
}, null, 2));
console.log(`Qualification backend ready in ${startupMs.toFixed(1)} ms; evidence: ${evidence}`);
