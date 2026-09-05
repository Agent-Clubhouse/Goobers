import { readState, writeState } from "./storage.mjs";

const THEMES = new Set(["system", "light", "dark"]);
const FILTER_KEYS = new Set([
    "gaggle", "workflow", "phase", "trigger", "stage", "outcome",
    "population", "showNoWork", "since", "until",
]);

function normalizeFilters(filters) {
    const normalized = {};
    for (const [key, value] of Object.entries(filters || {})) {
        if (!FILTER_KEYS.has(key)) continue;
        if (key === "showNoWork") {
            if (value === true) normalized[key] = true;
            continue;
        }
        if (Array.isArray(value)) {
            const values = [...new Set(value.map(String).map((item) => item.trim()).filter(Boolean))];
            if (values.length) normalized[key] = values;
            continue;
        }
        if (typeof value === "string" && value.trim()) normalized[key] = value.trim();
    }
    return normalized;
}

export async function readPreferences() {
    const raw = await readState("preferences.json");
    if (raw === undefined) return { theme: "system", filters: {} };
    const parsed = JSON.parse(raw);
    return {
        theme: THEMES.has(parsed?.theme) ? parsed.theme : "system",
        filters: normalizeFilters(parsed?.filters),
    };
}

export async function setThemePreference(theme) {
    if (!THEMES.has(theme)) {
        throw new Error(`invalid theme preference: ${theme}`);
    }
    const preferences = await readPreferences();
    preferences.theme = theme;
    await writeState("preferences.json", JSON.stringify(preferences, null, 2) + "\n");
    return preferences;
}

export async function setFilterPreferences(filters) {
    const preferences = await readPreferences();
    preferences.filters = normalizeFilters(filters);
    await writeState("preferences.json", JSON.stringify(preferences, null, 2) + "\n");
    return preferences;
}
