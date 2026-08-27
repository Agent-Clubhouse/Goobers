import { promises as fs } from "node:fs";
import path from "node:path";
import os from "node:os";

function copilotHome() {
    return process.env.COPILOT_HOME || path.join(os.homedir(), ".copilot");
}

export function statePath(name) {
    return path.join(copilotHome(), "extension-state", "goobers-portal", name);
}

export function legacyStatePath(name) {
    return path.join(copilotHome(), "extensions", "goobers-portal", "artifacts", name);
}

async function readCandidate(file) {
    for (let attempt = 0; attempt < 5; attempt += 1) {
        try {
            const before = await fs.stat(file);
            const value = await fs.readFile(file, "utf8");
            const after = await fs.stat(file);
            if (
                before.dev === after.dev &&
                before.ino === after.ino &&
                before.size === after.size &&
                before.mtimeMs === after.mtimeMs
            ) {
                let valid = true;
                try {
                    JSON.parse(value);
                } catch {
                    valid = false;
                }
                return { value, modified: after.mtimeMs, valid };
            }
        } catch (err) {
            if (err && err.code === "ENOENT") return undefined;
            throw err;
        }
    }
    throw new Error(`Portal state changed repeatedly while reading ${file}`);
}

export async function readState(name) {
    const candidates = [];
    for (const file of [statePath(name), legacyStatePath(name)]) {
        const candidate = await readCandidate(file);
        if (candidate) candidates.push(candidate);
    }
    if (candidates.length === 0) return undefined;
    candidates.sort((a, b) => b.modified - a.modified);
    return candidates.find((candidate) => candidate.valid)?.value ?? candidates[0].value;
}

export async function writeState(name, value) {
    const file = statePath(name);
    await fs.mkdir(path.dirname(file), { recursive: true });
    const tmp = `${file}.tmp`;
    await fs.writeFile(tmp, value, "utf8");
    await fs.rename(tmp, file);
}
