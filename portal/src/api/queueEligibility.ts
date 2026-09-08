import { isRecord, MalformedResponseError } from "./errors";
import type { QueueEligibilityView } from "./types";

const claimStates = new Set(["unknown", "unclaimed", "expired", "held-by-this-run", "held-by-other-run", "held-in-legacy-namespace"]);
const comparisons = new Set(["unavailable", "no-local-lease-or-provider-label", "local-lease-and-provider-label", "provider-label-without-live-local-lease", "local-lease-without-provider-label"]);
const boundedText = (value: unknown, max: number): value is string => typeof value === "string" && value.length <= max;
const timestamp = (value: unknown): boolean => boundedText(value, 64) && Number.isFinite(Date.parse(value));
const count = (value: unknown): value is number => typeof value === "number" && Number.isSafeInteger(value) && value >= 0;

// This guards rendering, scope, and report accounting. The daemon additionally
// validates the canonical closed schema and lease semantics before serving it.
export function assertQueueEligibility(value: unknown, gaggle: string, workflow: string): asserts value is QueueEligibilityView {
  if (!isRecord(value) || value.gaggle !== gaggle || value.workflow !== workflow || !timestamp(value.asOf) ||
      !["observed", "not-observed", "unavailable"].includes(String(value.status)) ||
      (value.problem !== undefined && !boundedText(value.problem, 4096)) ||
      (value.readState !== undefined && !validReadState(value.readState))) {
    throw new MalformedResponseError("The daemon returned mismatched queue eligibility evidence.");
  }
  if (value.status !== "observed") {
    if (value.report !== undefined) throw new MalformedResponseError("Unavailable queue evidence included a report.");
    return;
  }
  const report = value.report;
  if (!isRecord(report) || report.version !== 1 || report.gaggle !== gaggle || report.workflow !== workflow ||
      !boundedText(value.sourceRunId, 256) || !value.sourceRunId || report.runId !== value.sourceRunId ||
      !boundedText(report.repositoryKey, 4096) || !report.repositoryKey || !timestamp(report.observedAt) ||
      typeof report.completeSnapshot !== "boolean" || !count(report.matchingItems) || !count(report.omittedItems) ||
      !Array.isArray(report.items) || report.items.length > 1000 ||
      report.matchingItems - report.omittedItems !== report.items.length) {
    throw new MalformedResponseError("The daemon returned invalid queue report metadata.");
  }
  const numbers = new Set<number>();
  for (const item of report.items) {
    if (!isRecord(item) || !count(item.number) || item.number === 0 || numbers.has(item.number) ||
        typeof item.eligible !== "boolean" || !boundedText(item.nextStep, 2048) ||
        (item.eligible ? item.reason !== undefined && item.reason !== "" : !boundedText(item.reason, 256) || !item.reason) ||
        !validClaim(item.claim)) {
      throw new MalformedResponseError("The daemon returned invalid per-PR queue evidence.");
    }
    numbers.add(item.number);
  }
}

function validReadState(state: unknown): boolean {
  return isRecord(state) && ["complete", "partial"].includes(String(state.completeness)) &&
    typeof state.lagSeconds === "number" && Number.isFinite(state.lagSeconds) && state.lagSeconds >= 0 &&
    Array.isArray(state.degraded) && state.degraded.length <= 64 &&
    state.degraded.every((condition: unknown) => boundedText(condition, 256));
}

function validClaim(claim: unknown): boolean {
  if (!isRecord(claim) || !claimStates.has(String(claim.state)) || !comparisons.has(String(claim.comparison)) ||
      typeof claim.providerClaimLabel !== "boolean" || !boundedText(claim.nextStep, 2048)) return false;
  const owned = claim.state !== "unknown" && claim.state !== "unclaimed";
  return owned
    ? boundedText(claim.ownerRunId, 256) && claim.ownerRunId.length > 0 && timestamp(claim.expiresAt)
    : claim.ownerRunId === undefined && claim.expiresAt === undefined;
}
