import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { join } from "node:path";

const portal = fileURLToPath(new URL("../", import.meta.url));
const output = join(portal, ".portal-package");
const manifest = JSON.parse(await readFile(join(output, "package/portal-artifact.json"), "utf8"));
const archive = join(output, `goobers-portal-${manifest.packageVersion}.tgz`);
const consumer = join(output, "consumer");
await rm(consumer, { recursive: true, force: true });
await mkdir(consumer, { recursive: true });
await writeFile(join(consumer, "package.json"), JSON.stringify({
  name: "portal-consumer-test", private: true, type: "module",
  dependencies: Object.fromEntries(["react", "react-dom", "three"].map(name =>
    [name, `file:${join(portal, "node_modules", name).replaceAll("\\", "/")}`])),
}));
const npm = process.env.npm_execpath;
assert.ok(npm, "Run npm run test:package.");
execFileSync(process.execPath, [npm, "install", "--offline", "--ignore-scripts", "--no-audit", "--no-fund",
  "--package-lock=false", archive], { cwd: consumer, stdio: "inherit" });
await writeFile(join(consumer, "smoke.mjs"), `
import assert from "node:assert/strict";
import { createElement } from "react";
import { renderToString } from "react-dom/server";
import { JSDOM } from "jsdom";
import { PortalWorkbench, HttpDaemonClient } from "@goobers/portal";
const dom = new JSDOM('<html><head></head><body></body></html>', { url: "http://localhost/#/overview" });
globalThis.window = dom.window;
globalThis.document = dom.window.document;
let requests = 0;
const client = new HttpDaemonClient({ fetch: async () => { requests++; throw new Error("Unexpected network request"); } });
const html = renderToString(createElement(PortalWorkbench, { client, scope: "user:instance:events" }));
assert.ok(html.includes("main-content"), "The installed package must render the real portal shell.");
assert.equal(requests, 0, "Import/server rendering must not make implicit daemon requests.");
assert.throws(() => renderToString(createElement(PortalWorkbench, { client, scope: "" })));
dom.window.close();
console.log("Packed portal rendered with an injected client.");
`);
await writeFile(join(consumer, "smoke.tsx"), `
import { PortalWorkbench, HttpDaemonClient, type PortalWorkbenchProps, type DaemonClient } from "@goobers/portal";
const client: DaemonClient = new HttpDaemonClient();
const props: PortalWorkbenchProps = { client, scope: "instance:principal:events" };
export const portal = <PortalWorkbench {...props} />;
// @ts-expect-error An embedding host must supply the daemon client.
export const invalid = <PortalWorkbench scope="instance" />;
`);
await writeFile(join(consumer, "tsconfig.json"), JSON.stringify({
  compilerOptions: {
    strict: true, noEmit: true, skipLibCheck: true, jsx: "react-jsx",
    target: "ES2022", module: "ESNext", moduleResolution: "Bundler",
  },
  include: ["smoke.tsx"],
}));
execFileSync(process.execPath, [join(portal, "node_modules/typescript/bin/tsc"), "-p", join(consumer, "tsconfig.json")],
  { cwd: consumer, stdio: "inherit" });
execFileSync(process.execPath, [join(consumer, "smoke.mjs")], { cwd: consumer, stdio: "inherit" });
const installed = join(consumer, "node_modules/@goobers/portal");
const packaged = JSON.parse(await readFile(join(installed, "package.json"), "utf8"));
assert.ok(packaged.peerDependencies.react);
assert.equal(packaged.dependencies.react, undefined);
assert.equal(packaged.private, true);
assert.ok((await readFile(join(installed, "dist/portal.css"))).length > 0);
assert.ok((await readdir(join(installed, "assets"))).includes("goober-mascot.png"));
assert.equal(manifest.hostContract.pollingFallback, false);
console.log("Tarball exports, declarations, peer dependencies, assets and provenance verified.");
