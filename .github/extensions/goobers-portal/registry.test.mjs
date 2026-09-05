import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import test from "node:test";

import { addSource, normalizeId } from "./registry.mjs";

const execFileAsync = promisify(execFile);

test("registry normalizes ids and updates duplicate sources", async () => {
    const home = await fs.mkdtemp(path.join(os.tmpdir(), "goobers-portal-registry-"));
    try {
        assert.equal(normalizeId("local", "root///"), "local:root");
        const script = `
            import { addSource, listKnownSources, removeSource } from "./.github/extensions/goobers-portal/registry.mjs";
            const first = await addSource({ kind: "remote", value: "http://example.test", label: "old" });
            const updated = await addSource({ kind: "remote", value: "http://example.test", label: "new" });
            console.log(JSON.stringify({
                first, updated, sources: await listKnownSources(),
                removed: await removeSource(updated.id),
                empty: await listKnownSources(),
                removedAgain: await removeSource(updated.id),
            }));
        `;
        const { stdout } = await execFileAsync(process.execPath, ["--input-type=module", "-e", script], {
            cwd: process.cwd(),
            env: { ...process.env, COPILOT_HOME: home },
        });
        const result = JSON.parse(stdout);
        assert.equal(result.updated.id, result.first.id);
        assert.equal(result.updated.label, "new");
        assert.match(result.updated.addedAt, /^\d{4}-\d{2}-\d{2}T/);
        assert.equal(result.sources.length, 1);
        assert.equal(result.sources[0].id, result.updated.id);
        assert.equal(result.sources[0].label, "new");
        assert.equal(result.removed, true);
        assert.deepEqual(result.empty, []);
        assert.equal(result.removedAgain, false);
    } finally {
        await fs.rm(home, { recursive: true, force: true });
    }
});

test("registry rejects invalid kinds and malformed GitHub Actions URLs", async () => {
    await assert.rejects(
        addSource({ kind: "unsupported", value: "value" }),
        /invalid source kind/,
    );
    await assert.rejects(
        addSource({ kind: "github-actions", value: "https://example.test/not-a-workflow" }),
        /Expected https:\/\/github.com/,
    );
});
