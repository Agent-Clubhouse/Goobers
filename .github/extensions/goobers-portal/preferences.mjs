import { promises as fs } from "node:fs";
import path from "node:path";
import os from "node:os";

const THEMES = new Set(["system", "light", "dark"]);

function copilotHome() {
    return process.env.COPILOT_HOME || path.join(os.homedir(), ".copilot");
}

function preferencesPath() {
    return path.join(copilotHome(), "extensions", "goobers-portal", "artifacts", "preferences.json");
}

export async function readPreferences() {
    try {
        const raw = await fs.readFile(preferencesPath(), "utf8");
        const parsed = JSON.parse(raw);
        return { theme: THEMES.has(parsed?.theme) ? parsed.theme : "system" };
    } catch (err) {
        if (err && err.code === "ENOENT") return { theme: "system" };
        throw err;
    }
}

export async function setThemePreference(theme) {
    if (!THEMES.has(theme)) {
        throw new Error(`invalid theme preference: ${theme}`);
    }
    const file = preferencesPath();
    await fs.mkdir(path.dirname(file), { recursive: true });
    const tmp = file + ".tmp";
    await fs.writeFile(tmp, JSON.stringify({ theme }, null, 2) + "\n", "utf8");
    await fs.rename(tmp, file);
    return { theme };
}
