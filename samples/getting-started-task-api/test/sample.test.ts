import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { describe, it } from "node:test";

interface SampleManifest {
  schemaVersion: number;
  id: string;
  version: string;
  compatibleTemplates: string[];
  seedIssues: string;
  localCI: {
    command: string[];
  };
  state: string;
}

interface PackageManifest {
  version: string;
}

interface SeedIssue {
  id: string;
  title: string;
  body: string;
  labels: string[];
}

interface SeedCatalog {
  schemaVersion: number;
  sample: {
    id: string;
    version: string;
  };
  labels: Array<{ name: string }>;
  issues: SeedIssue[];
}

async function readJSON<T>(path: string): Promise<T> {
  return JSON.parse(await readFile(path, "utf8"));
}

describe("tutorial fixture", () => {
  it("keeps the app and issue catalog on the same version", async () => {
    const manifest = await readJSON<SampleManifest>("sample.json");
    const packageManifest = await readJSON<PackageManifest>("package.json");
    const seeds = await readJSON<SeedCatalog>(manifest.seedIssues);

    assert.equal(manifest.schemaVersion, 1);
    assert.equal(seeds.schemaVersion, 1);
    assert.equal(manifest.id, seeds.sample.id);
    assert.equal(manifest.version, packageManifest.version);
    assert.equal(manifest.version, seeds.sample.version);
    assert.deepEqual(manifest.compatibleTemplates, ["quickstart@v1"]);
    assert.deepEqual(manifest.localCI.command, ["npm", "run", "ci"]);
    assert.equal(manifest.state, "memory-only");
  });

  it("provides complete, ordered issues with declared labels", async () => {
    const seeds = await readJSON<SeedCatalog>("seed-issues.json");
    const declaredLabels = new Set(seeds.labels.map((label) => label.name));

    assert.deepEqual(
      seeds.issues.map((issue) => issue.id),
      [
        "reject-empty-task-titles",
        "make-completion-idempotent",
        "filter-tasks-by-status"
      ]
    );

    for (const issue of seeds.issues) {
      assert.notEqual(issue.title.trim(), "");
      assert.match(issue.body, /## Acceptance criteria/);
      assert.match(issue.body, /Run `npm test`\./);
      assert.ok(issue.labels.includes("goobers:approved"));
      assert.ok(issue.labels.includes("goobers:ready"));
      assert.ok(issue.labels.every((label) => declaredLabels.has(label)));
    }
  });
});
