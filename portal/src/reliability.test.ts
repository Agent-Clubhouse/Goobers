import { describe, expect, it } from "vitest";
import type { RunReliability } from "./api/types";
import { reliabilitySummary } from "./reliability";

function reliability(overrides: Partial<RunReliability>): RunReliability {
  return {
    state: "active",
    failure: { classification: "none", evidenceRule: "no-error-recorded" },
    budgets: [
      { kind: "implementation-review", consumed: 0, remaining: null, evidence: "journal" },
      { kind: "ci-poll", consumed: null, remaining: null, evidence: "unknown" },
    ],
    latestVerdict: "unknown",
    acceptance: { state: "unknown" },
    retained: {},
    nextAction: "finish implement",
    ...overrides,
  };
}

describe("reliabilitySummary", () => {
  it("renders an active run with unknown budgets as ? rather than zero", () => {
    expect(reliabilitySummary(reliability({ currentStage: "implement", currentAttempt: 1 }))).toBe(
      "Reliability active implement attempt 1; failure none (no-error-recorded); " +
        "budgets implementation-review 0 used/? left, ci-poll ? used/? left; " +
        "verdict unknown; acceptance unknown; retained unknown; next finish implement",
    );
  });

  it("renders a retrying run's classified failure and backoff action", () => {
    const line = reliabilitySummary(
      reliability({
        state: "retrying",
        failure: { classification: "infra", evidenceRule: "latestError.causes.class", code: "workspace_failed" },
        nextAction: "wait for retry backoff on implement",
      }),
    );
    expect(line).toContain("Reliability retrying;");
    expect(line).toContain("failure infra workspace_failed (latestError.causes.class)");
    expect(line).toContain("next wait for retry backoff on implement");
  });

  it("renders an escalated run's recorded budget and human-intervention reason", () => {
    const line = reliabilitySummary(
      reliability({
        state: "escalated",
        failure: { classification: "escalation", evidenceRule: "terminalCause.classification" },
        budgets: [{ kind: "local-infra", consumed: 1, remaining: 2, evidence: "terminalCause" }],
        latestVerdict: "escalate",
        acceptance: { state: "complete", digest: "sha256:acc" },
        retained: {
          branch: "goobers/5313",
          branchSha: "abc123",
          pullRequest: { provider: "github", kind: "pr", id: "42" },
          pullRequestDraft: "draft",
          recoveryRunId: "run-1",
        },
        nextAction: "human intervention required",
        humanInterventionReason: "reviewer requested a human decision",
      }),
    );
    expect(line).toContain("budgets local-infra 1 used/2 left");
    expect(line).toContain("verdict escalate");
    expect(line).toContain("acceptance complete sha256:acc");
    expect(line).toContain("retained branch goobers/5313@abc123, pr 42 (draft), recovers run-1");
    expect(line.endsWith("needs human: reviewer requested a human decision")).toBe(true);
  });
});
