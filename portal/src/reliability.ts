import type { ReliabilityBudget, RunReliability } from "./api/types";

function count(value: number | null): string {
  return value === null ? "?" : String(value);
}

function budgetLabel(budget: ReliabilityBudget): string {
  return `${budget.kind} ${count(budget.consumed)} used/${count(budget.remaining)} left`;
}

function retainedLabel(retained: RunReliability["retained"]): string {
  const parts: string[] = [];
  if (retained.branch) {
    parts.push(`branch ${retained.branch}${retained.branchSha ? `@${retained.branchSha}` : ""}`);
  }
  if (retained.pullRequest) {
    parts.push(`pr ${retained.pullRequest.url || retained.pullRequest.id}`);
  }
  if (retained.recoveryRunId) {
    parts.push(`recovers ${retained.recoveryRunId}`);
  }
  return parts.length > 0 ? parts.join(", ") : "unknown";
}

/**
 * One-line dashboard rendering of the implementation reliability projection
 * (#5313). Unknown counts render as "?" so missing evidence never reads as zero.
 */
export function reliabilitySummary(reliability: RunReliability): string {
  let state = reliability.state;
  if (reliability.currentStage) {
    state += ` ${reliability.currentStage}`;
    if (reliability.currentAttempt) {
      state += ` attempt ${reliability.currentAttempt}`;
    }
  }
  const { failure } = reliability;
  const parts = [
    `Reliability ${state}`,
    `failure ${failure.classification}${failure.code ? ` ${failure.code}` : ""} (${failure.evidenceRule})`,
    `budgets ${reliability.budgets.map(budgetLabel).join(", ")}`,
    `verdict ${reliability.latestVerdict}`,
    `acceptance ${reliability.acceptance.state}`,
    `retained ${retainedLabel(reliability.retained)}`,
    `next ${reliability.nextAction}`,
  ];
  if (reliability.humanInterventionReason) {
    parts.push(`needs human: ${reliability.humanInterventionReason}`);
  }
  return parts.join("; ");
}
