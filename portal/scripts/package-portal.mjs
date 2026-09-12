import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { cp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { resolve, join } from "node:path";
import { build } from "vite";
import react from "@vitejs/plugin-react";

const portal = fileURLToPath(new URL("../", import.meta.url));
const repository = resolve(portal, "..");
const output = join(portal, ".portal-package");
const pkg = join(output, "package");
const source = JSON.parse(await readFile(join(portal, "package.json"), "utf8"));
const commit = execFileSync("git", ["rev-parse", "HEAD"], { cwd: repository, encoding: "utf8" }).trim();
const dirty = execFileSync("git", ["status", "--porcelain", "--untracked-files=normal"], {
  cwd: repository, encoding: "utf8",
}).trim().length > 0;
const version = process.env.GOOBERS_PORTAL_PACKAGE_VERSION || `${source.version}-dev.${commit.slice(0, 12)}`;
const semver = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;
if (!semver.test(version)) throw new Error("GOOBERS_PORTAL_PACKAGE_VERSION must be a SemVer version.");
if (!version.includes("-dev.") && dirty) throw new Error("Release packages require a clean worktree.");
await rm(pkg, { recursive: true, force: true });
await mkdir(pkg, { recursive: true });
await build({
  configFile: false,
  root: portal,
  publicDir: false,
  plugins: [react()],
  build: {
    outDir: join(pkg, "dist"),
    emptyOutDir: true,
    lib: { entry: join(portal, "src/package-entry.ts"), formats: ["es"], fileName: "portal", cssFileName: "portal" },
    rolldownOptions: { external: [/^react(?:\/|$)/, /^react-dom(?:\/|$)/, /^three(?:\/|$)/] },
  },
});
execFileSync(process.execPath, [join(portal, "node_modules/typescript/bin/tsc"), "-p", "tsconfig.package.json"], {
  cwd: portal, stdio: "inherit",
});
await cp(join(repository, "LICENSE"), join(pkg, "LICENSE"));
await cp(join(portal, "PACKAGE.md"), join(pkg, "README.md"));
await cp(join(portal, "public"), join(pkg, "assets"), { recursive: true });
await writeFile(join(pkg, "package.json"), JSON.stringify({
  name: "@goobers/portal",
  version,
  private: true,
  type: "module",
  license: "MIT",
  description: "Reusable Goobers portal workbench with an injected daemon client",
  files: ["dist", "types", "assets", "portal-artifact.json", "README.md", "LICENSE"],
  exports: {
    ".": { types: "./types/package.d.ts", import: "./dist/portal.js" },
    "./styles.css": "./dist/portal.css",
    "./portal-artifact.json": "./portal-artifact.json",
    "./assets/*": "./assets/*",
  },
  peerDependencies: { react: source.dependencies.react, "react-dom": source.dependencies["react-dom"] },
  dependencies: { three: source.dependencies.three },
  repository: { type: "git", url: "https://github.com/Agent-Clubhouse/Goobers.git", directory: "portal" },
}, null, 2) + "\n");
const hash = async (file) => createHash("sha256").update(await readFile(file)).digest("hex");
await writeFile(join(pkg, "portal-artifact.json"), JSON.stringify({
  artifactVersion: 1, packageVersion: version, commit, dirty,
  apiContractVersion: "v1",
  contractSha256: await hash(join(portal, "src/api/contract.generated.ts")),
  lockfileSha256: await hash(join(portal, "package-lock.json")),
  hostContract: {
    oneWorkbenchPerDocument: true,
    routing: "hash",
    assetsBasePath: "/",
    requiresInjectedClient: true,
    pollingFallback: false,
    cursorScope: "host-supplied-principal-instance-route-filter",
  },
}, null, 2) + "\n");
const npm = process.env.npm_execpath;
if (!npm) throw new Error("Run this through npm run package:portal.");
const packed = JSON.parse(execFileSync(process.execPath, [npm, "pack", pkg, "--pack-destination", output, "--json"], {
  cwd: portal, encoding: "utf8",
}));
const archive = join(output, packed[0].filename);
await writeFile(`${archive}.sha256`, `${await hash(archive)}  ${packed[0].filename}\n`);
console.log(JSON.stringify({ archive, integrity: packed[0].integrity, commit, dirty }));
