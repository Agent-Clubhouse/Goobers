// Locates a usable Playwright without depending on NODE_PATH.
//
// NODE_PATH is a CommonJS-only mechanism — it has no effect on ESM `import`,
// so a monitor run from a systemd unit cannot rely on the environment being
// prepared. Resolve an absolute path and require it directly instead.
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "..", "..", "..");

function globalRoot() {
  try {
    return execFileSync("npm", ["root", "-g"], { encoding: "utf8" }).trim();
  } catch {
    return null;
  }
}

function npxCacheRoots() {
  const base = path.join(process.env.HOME ?? "", ".npm", "_npx");
  if (!existsSync(base)) return [];
  return readdirSync(base).map((d) => path.join(base, d, "node_modules"));
}

/** Returns the playwright module, or throws with the paths that were tried. */
export function loadPlaywright() {
  const candidates = [
    path.join(repoRoot, "portal", "node_modules"),
    path.join(repoRoot, "node_modules"),
    globalRoot(),
    ...npxCacheRoots(),
  ].filter(Boolean);

  for (const dir of candidates) {
    const entry = path.join(dir, "playwright");
    if (!existsSync(path.join(entry, "package.json"))) continue;
    try {
      return { playwright: require(entry), from: entry };
    } catch {
      // Present but unusable (partial install); keep looking.
    }
  }
  throw new Error(
    "playwright not found. Install it with `npm i -g playwright` or " +
      "`npm --prefix portal ci`. Looked in:\n  " + candidates.join("\n  "),
  );
}
