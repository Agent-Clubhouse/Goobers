import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { readPreferences, setFilterPreferences, setThemePreference } from "./preferences.mjs";

test("preferences preserve theme and normalized multi-value filters", async () => {
    const previous = process.env.COPILOT_HOME;
    const home = await fs.mkdtemp(path.join(os.tmpdir(), "goobers-portal-preferences-"));
    process.env.COPILOT_HOME = home;
    try {
        await setThemePreference("dark");
        await setFilterPreferences({
            phase: ["failed", "escalated", "failed"],
            workflow: ["implement"],
            showNoWork: true,
            ignored: "value",
        });
        assert.deepEqual(await readPreferences(), {
            theme: "dark",
            filters: {
                phase: ["failed", "escalated"],
                workflow: ["implement"],
                showNoWork: true,
            },
        });
        await setFilterPreferences({});
        assert.deepEqual(await readPreferences(), { theme: "dark", filters: {} });
    } finally {
        if (previous === undefined) delete process.env.COPILOT_HOME;
        else process.env.COPILOT_HOME = previous;
        await fs.rm(home, { recursive: true, force: true });
    }
});
