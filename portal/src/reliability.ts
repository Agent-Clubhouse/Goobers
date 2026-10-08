import type { ReliabilityBudget, RunReliability } from "./api/types";

function count(value: number | null): string {
  return value === null ? "?" : String(value);
}

function budgetLabel(budget: ReliabilityBudget): string {
  return `${budget.kind} ${count(budget.consumed)} used/${count(budget.remaining)} left`;
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
  const parts = [
    `Reliability ${state}`,
    `failure ${reliability.failure.classification} (${reliability.failure.evidenceRule})`,
    `budgets ${reliability.budgets.map(budgetLabel).join(", ")}`,
    `verdict ${reliability.latestVerdict}`,
    `acceptance ${reliability.acceptance.state}`,
    `next ${reliability.nextAction}`,
  ];
  if (reliability.humanInterventionReason) {
    parts.push(`needs human: ${reliability.humanInterventionReason}`);
  }
  return parts.join("; ");
}
