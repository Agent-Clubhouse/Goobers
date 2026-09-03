import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { legacyStatePath, readState, statePath, writeState } from "./storage.mjs";

test("state storage reads legacy data and writes outside installed code", async () => {
    const previous = process.env.COPILOT_HOME;
    const home = await fs.mkdtemp(path.join(os.tmpdir(), "goobers-portal-state-"));
    process.env.COPILOT_HOME = home;
    try {
        const legacy = legacyStatePath("sources.json");
        await fs.mkdir(path.dirname(legacy), { recursive: true });
        await fs.writeFile(legacy, "legacy\n", "utf8");
        assert.equal(await readState("sources.json"), "legacy\n");

        await writeState("sources.json", "current\n");
        assert.equal(await fs.readFile(statePath("sources.json"), "utf8"), "current\n");
        assert.equal(await readState("sources.json"), "current\n");
        assert.equal(statePath("sources.json").includes(path.join("extensions", "goobers-portal")), false);
    } finally {
        if (previous === undefined) delete process.env.COPILOT_HOME;
        else process.env.COPILOT_HOME = previous;
        await fs.rm(home, { recursive: true, force: true });
    }
});

test("state storage reads a newer valid legacy write from a running old extension", async () => {
    const previous = process.env.COPILOT_HOME;
    const home = await fs.mkdtemp(path.join(os.tmpdir(), "goobers-portal-state-"));
    process.env.COPILOT_HOME = home;
    try {
        const current = statePath("preferences.json");
        const legacy = legacyStatePath("preferences.json");
        await fs.mkdir(path.dirname(current), { recursive: true });
        await fs.mkdir(path.dirname(legacy), { recursive: true });
        await fs.writeFile(current, '{"theme":"light"}\n', "utf8");
        await fs.writeFile(legacy, '{"theme":"dark"}\n', "utf8");
        const oldTime = new Date("2025-01-01T00:00:00Z");
        const newTime = new Date("2025-01-01T00:00:01Z");
        await fs.utimes(current, oldTime, oldTime);
        await fs.utimes(legacy, newTime, newTime);

        assert.equal(await readState("preferences.json"), '{"theme":"dark"}\n');
    } finally {
        if (previous === undefined) delete process.env.COPILOT_HOME;
        else process.env.COPILOT_HOME = previous;
        await fs.rm(home, { recursive: true, force: true });
    }
});
