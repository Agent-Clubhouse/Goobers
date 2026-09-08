// The local finding ledger.
//
// Lives outside the repo (XDG state) so a monitor running every few minutes
// never dirties `git status` and its history survives branch switches.
//
// A failing check is not a finding. Real systems produce transient failures —
// a daemon restart, a slow first paint — and a monitor that files every blip
// gets muted, which is the same as not having one. A check must fail
// `minConsecutiveFailures` times in a row before it is promoted.
import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync, existsSync, readdirSync, rmSync } from "node:fs";
import path from "node:path";

export function stateDir(config) {
  if (config.stateDir) return config.stateDir;
  const base =
    process.env.XDG_STATE_HOME ?? path.join(process.env.HOME ?? ".", ".local", "state");
  return path.join(base, "goobers-uiwatch");
}

// Normalize volatile text out of a failure so the same defect fingerprints
// identically across runs.
//
// Two rules learned the hard way:
//
//  1. Percent-encoding defeats \b. In "...T05%3A02%3A21Z" the "A02" run has no
//     word boundary, so \b\d+\b never matches and the timestamp survives —
//     giving one defect a new fingerprint on every run. Decode first.
//  2. Never mask digits indiscriminately. "HTTP 503" and "HTTP 404" are
//     DIFFERENT defects; collapsing both to "HTTP <n>" merges unrelated
//     failures. Mask specific volatile shapes, not all numbers.
//
// Query strings are dropped wholesale: they carry the moving time windows and
// almost never identify the defect, whereas the endpoint path always does.
function normalize(detail) {
  let s = String(detail ?? "");
  try {
    s = decodeURIComponent(s);
  } catch {
    // Malformed escapes: fingerprint the raw form rather than losing the finding.
  }
  return s
    .replace(/\d+(\.\d+)?ms\b/g, "<ms>")
    .replace(/\?[^\s|]*/g, "?<query>")
    .replace(/\d{4}-\d{2}-\d{2}T[\d:.]+Z?/g, "<ts>")
    .replace(/\b01[A-Z0-9]{24}\b/g, "<ulid>")
    .replace(/\b[0-9a-f]{12,}\b/gi, "<hex>")
    .trim()
    .slice(0, 300);
}

export function fingerprint(check) {
  const key = [check.browser, check.kind, check.route, normalize(check.detail)].join("|");
  return createHash("sha1").update(key).digest("hex").slice(0, 12);
}

function readJSON(file, fallback) {
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch {
    return fallback;
  }
}

function writeJSON(file, value) {
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, JSON.stringify(value, null, 2) + "\n");
}

/**
 * Folds one probe result into the ledger.
 * Returns { promoted, cleared, findings } where `promoted` are findings that
 * crossed the consecutive-failure threshold on THIS run — the only ones worth
 * telling a human about.
 */
export function record(config, result) {
  const dir = stateDir(config);
  const findingsDir = path.join(dir, "findings");
  const threshold = config.thresholds.minConsecutiveFailures ?? 2;

  writeJSON(path.join(dir, "runs", `${result.at.replace(/[:.]/g, "-")}.json`), result);

  const promoted = [];
  const cleared = [];

  // Collapse to one entry per fingerprint PER RUN. Several distinct URLs can
  // normalize to the same defect, and counting each of them would make a
  // single run look like a multi-run streak — promoting on first sight and
  // defeating the whole point of the threshold. The streak counts runs.
  const byFingerprint = new Map();
  for (const check of result.checks) {
    if (!check.ok) byFingerprint.set(fingerprint(check), check);
  }
  const failing = new Set(byFingerprint.keys());

  for (const [fp, check] of byFingerprint) {
    const metaFile = path.join(findingsDir, fp, "meta.json");
    const prior = readJSON(metaFile, null);

    {
      const consecutive = (prior?.consecutive ?? 0) + 1;
      const meta = {
        id: fp,
        status: prior?.status ?? "watching",
        browser: check.browser,
        kind: check.kind,
        route: check.route,
        signature: normalize(check.detail),
        sampleDetail: check.detail,
        firstSeen: prior?.firstSeen ?? result.at,
        lastSeen: result.at,
        occurrences: (prior?.occurrences ?? 0) + 1,
        consecutive,
        evidence: [...(prior?.evidence ?? []), ...result.artifacts].slice(-10),
      };
      const wasBelow = (prior?.consecutive ?? 0) < threshold;
      if (consecutive >= threshold && wasBelow && meta.status === "watching") {
        meta.status = "open";
        promoted.push(meta);
      }
      writeJSON(metaFile, meta);
    }
  }

  // Recovery is detected by ABSENCE, not by a matching passing check. An
  // exploded per-error check simply stops being emitted once the error stops,
  // so there is no passing counterpart to match against.
  for (const meta of listFindings(config)) {
    if (failing.has(meta.id) || (meta.consecutive ?? 0) === 0) continue;
    const updated = { ...meta, consecutive: 0, lastRecovered: result.at };
    if (meta.status === "open") {
      updated.status = "recovered";
      cleared.push(updated);
    }
    writeJSON(path.join(findingsDir, meta.id, "meta.json"), updated);
  }

  return { promoted, cleared, findings: listFindings(config) };
}

export function listFindings(config) {
  const dir = path.join(stateDir(config), "findings");
  if (!existsSync(dir)) return [];
  return readdirSync(dir)
    .map((id) => readJSON(path.join(dir, id, "meta.json"), null))
    .filter(Boolean)
    .sort((a, b) => String(b.lastSeen).localeCompare(String(a.lastSeen)));
}

/**
 * Bounded retention. A failing run writes ~1.5MB of traces and screenshots;
 * at a five-minute cadence that is hundreds of MB per day, so an unbounded
 * monitor eventually fills the disk it is meant to watch.
 *
 * Evidence still referenced by an open or acknowledged finding is never
 * pruned — that is exactly the evidence a human is about to look at.
 */
export function prune(config, { keepRuns = 200, keepEvidence = 40 } = {}) {
  const dir = stateDir(config);
  const referenced = new Set();
  for (const f of listFindings(config)) {
    if (f.status === "open" || f.status === "acknowledged") {
      for (const e of f.evidence ?? []) referenced.add(path.basename(path.dirname(e)));
    }
  }

  const removed = { runs: 0, evidence: 0 };
  const sweep = (sub, keep, protectedNames) => {
    const base = path.join(dir, sub);
    if (!existsSync(base)) return 0;
    const entries = readdirSync(base).sort().reverse();
    let n = 0;
    for (const name of entries.slice(keep)) {
      if (protectedNames?.has(name)) continue;
      rmSync(path.join(base, name), { recursive: true, force: true });
      n += 1;
    }
    return n;
  };
  removed.runs = sweep("runs", keepRuns);
  removed.evidence = sweep("evidence", keepEvidence, referenced);
  return removed;
}

export function setStatus(config, id, status) {
  const file = path.join(stateDir(config), "findings", id, "meta.json");
  const meta = readJSON(file, null);
  if (!meta) return null;
  meta.status = status;
  writeJSON(file, meta);
  return meta;
}
