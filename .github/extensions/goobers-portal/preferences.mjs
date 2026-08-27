import { readState, writeState } from "./storage.mjs";

const THEMES = new Set(["system", "light", "dark"]);

export async function readPreferences() {
    const raw = await readState("preferences.json");
    if (raw === undefined) return { theme: "system" };
    const parsed = JSON.parse(raw);
    return { theme: THEMES.has(parsed?.theme) ? parsed.theme : "system" };
}

export async function setThemePreference(theme) {
    if (!THEMES.has(theme)) {
        throw new Error(`invalid theme preference: ${theme}`);
    }
    await writeState("preferences.json", JSON.stringify({ theme }, null, 2) + "\n");
    return { theme };
}
