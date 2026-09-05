// Persisted registry of known Goobers sources (local instance roots and/or
// remote control-plane URLs) that the portal can connect to. Stored per-user
// (not per-session, not in-repo) since "sources I connect to" is a durable,
// cross-session preference, not conversation- or artifact-scoped state.
//
// Liveness/health is never cached here — the registry only remembers *where*
// to look. Whether a source is actually reachable right now is always
// re-probed live (see client.mjs), so this file never goes stale in a way
// that could mislead the UI.

import { parseWorkflowURL } from "./actions-source.mjs";
import { readState, writeState } from "./storage.mjs";

async function readRegistry() {
    const raw = await readState("sources.json");
    if (raw === undefined) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed?.sources) ? parsed.sources : [];
}

async function writeRegistry(sources) {
    await writeState("sources.json", JSON.stringify({ sources }, null, 2) + "\n");
}

function normalizeId(kind, value) {
    return `${kind}:${value.replace(/[\\/]+$/, "")}`;
}

/** List all known (persisted) sources. */
export async function listKnownSources() {
    return await readRegistry();
}

/**
 * Add a source to the registry. `kind` is "local" (root is an instance root
 * directory) or "remote" (root is a base URL like http://host:port).
 * Idempotent by (kind, normalized value).
 */
export async function addSource({ kind, value, label, token }) {
    if (kind !== "local" && kind !== "remote" && kind !== "github-actions") {
        throw new Error(`invalid source kind: ${kind}`);
    }
    let normalizedValue = value;
    let defaultLabel = value;
    if (kind === "github-actions") {
        const parsed = parseWorkflowURL(value);
        normalizedValue = parsed.workflowURL;
        defaultLabel = `${parsed.owner}/${parsed.repo} · ${parsed.workflow}`;
    }
    const id = normalizeId(kind, normalizedValue);
    const sources = await readRegistry();
    const existingIdx = sources.findIndex((s) => s.id === id);
    const entry = {
        id,
        kind,
        value: normalizedValue,
        label: label || defaultLabel,
        token: token || undefined,
        addedAt: new Date().toISOString(),
    };
    if (existingIdx >= 0) {
        sources[existingIdx] = { ...sources[existingIdx], ...entry, addedAt: sources[existingIdx].addedAt };
    } else {
        sources.push(entry);
    }
    await writeRegistry(sources);
    return entry;
}

export async function removeSource(id) {
    const sources = await readRegistry();
    const next = sources.filter((s) => s.id !== id);
    await writeRegistry(next);
    return next.length !== sources.length;
}

export { normalizeId };
