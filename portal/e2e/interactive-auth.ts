import { readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";

const evidenceDirectory = resolve(process.env.INTERACTIVE_EVIDENCE_DIR ?? `${tmpdir()}/goobers-interactive-evidence`);

// This file is private host-only qualification state, never a model artifact.
export function humanBrowserOptions() {
  const storageState = `${evidenceDirectory}/human-browser-state.json`;
  const state = JSON.parse(readFileSync(storageState, "utf8"));
  const csrf = state.origins[0]?.localStorage.find((item: {name: string}) => item.name === "goobers.interactive.human-binding")?.value;
  if (!csrf) throw new Error("Start the authenticated interactive fixture before acceptance tests");
  return { storageState, extraHTTPHeaders: { "X-Goobers-CSRF": csrf as string } };
}
