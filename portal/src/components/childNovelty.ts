import type { ChildNovelty } from "../api/types";

/** Describes pinned advisory novelty evidence; it never implies authority. */
export function describeChildNovelty(novelty: ChildNovelty): string {
  const assignment = novelty.scored
    ? novelty.outOfDistribution
      ? `novel shape; nearest ${novelty.nearest ?? "cluster"} at distance ${novelty.distance.toFixed(2)}`
      : `similar to ${novelty.clusterId ?? novelty.nearest ?? "cluster"} at distance ${novelty.distance.toFixed(2)}`
    : `unscored (${novelty.reason ?? "model unavailable"})`;
  const parts = [assignment];
  if (novelty.modelVersion) parts.push(`Model ${novelty.modelVersion}`);
  if (novelty.historicalOutcome) {
    parts.push(`${(100 * novelty.historicalOutcome.successRate).toFixed(0)}% success across ${novelty.historicalOutcome.runCount} runs`);
  }
  if (novelty.humanApproval) parts.push(`Approved by ${novelty.humanApproval.actor}`);
  else if (novelty.humanApprovalRequired) parts.push("Human approval required");
  return parts.join(" · ");
}
