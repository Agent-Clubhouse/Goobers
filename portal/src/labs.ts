// Labs is the Portal's single home for experimental features (#8320). Each
// feature is one registry entry plus, once it has a UI, one page component.
// Labs features are read-only and advisory: they never write configuration
// or bypass validation, promotion, or approval gates.

import type { Route } from "./routing";

export type LabsMaturity = "concept" | "experimental" | "preview";

export type LabsAvailability = "available" | "coming-soon";

export interface LabsFeature {
  id: string;
  name: string;
  summary: string;
  description: string;
  maturity: LabsMaturity;
  availability: LabsAvailability;
  /** Data the feature reads and what must exist before it shows results. */
  prerequisites: string[];
  /** Tracking issue numbers in the Goobers repository. */
  issues: number[];
  /** Where the feature can be used today, for features that ship outside Labs. */
  surfaces?: LabsSurface[];
}

export type LabsSurface =
  | { kind: "portal"; label: string; route: Route }
  | { kind: "cli"; command: string; description: string }
  | { kind: "docs"; label: string; path: string };

export const labsDocsBaseUrl = "https://github.com/Agent-Clubhouse/Goobers/blob/main/";

export const labsIssueBaseUrl = "https://github.com/Agent-Clubhouse/Goobers/issues/";

export const labsFeatures: readonly LabsFeature[] = [
  {
    id: "workflow-clustering",
    name: "Workflow clustering",
    summary:
      "Unsupervised clustering of workflows, including agent-authored child workflows, to see what they converge to.",
    description:
      "Clusters configured and agent-authored (dynamic) child workflows with HDBSCAN over versioned structural features, " +
      "and later aggregated run behavior. Stable clusters reveal what self-spawning workflows converge to; proposals that " +
      "match no known cluster are flagged as novel; confound checks warn when clusters merely mirror a gaggle. " +
      "Recurring successful shapes become template candidates. Results are advisory only.",
    maturity: "experimental",
    availability: "coming-soon",
    prerequisites: [
      "At least a few configured or agent-authored workflows to cluster",
      "Agent-authored child workflows enabled (preview DSL 3.1) for dynamic-workflow analysis",
      "Retained run journals for behavior clustering",
    ],
    issues: [8302, 8303, 8304, 8305, 8306, 8307, 8308, 8309, 8310, 8311],
    surfaces: [
      {
        kind: "cli",
        command: "goobers workflow clusters [--stratify gaggle] [--json]",
        description: "Fit and inspect structural clusters, with an optional gaggle confound check.",
      },
      {
        kind: "cli",
        command: "goobers workflow clusters --features behavior [--since <time>]",
        description: "Cluster runs by journal-derived behavior descriptors (stages, retries, gates, children).",
      },
      {
        kind: "cli",
        command: "goobers workflow validate-child --cluster-model <model.json> ...",
        description: "Score a child workflow proposal's novelty against a saved cluster model.",
      },
    ],
  },
  {
    id: "backprop",
    name: "Backprop",
    summary:
      "Attributes run outcomes back to the stages, models, and tools that caused them so workflows can improve themselves.",
    description:
      "A gitops-native self-improving control loop. A credit graph assigns contribution and blame across stages, " +
      "subagents, models, and tools; a fault auditor classifies recurring failures; and shadow mode reports what Backprop " +
      "would change without acting. Attribution accuracy is evaluated before any finding is trusted.",
    maturity: "preview",
    availability: "available",
    prerequisites: [
      "Backprop enabled on a workflow (shadow or active mode)",
      "Completed runs with attribution recorded in the journal",
    ],
    issues: [2836, 4664, 6334, 7120, 7317, 7318, 7319, 8312],
    surfaces: [
      { kind: "portal", label: "Insights → Backprop", route: { page: "insight", filters: { section: "backprop" } } },
      {
        kind: "cli",
        command: "goobers telemetry shadow [--gaggle=name] [--workflow=name]",
        description: "Report what Backprop would attribute and change, without acting.",
      },
      { kind: "docs", label: "Backprop guide", path: "docs/guides/backprop.md" },
    ],
  },
  {
    id: "system-one",
    name: "System One decisions",
    summary:
      "Fast, thresholded decision models for semantic calls, so a hallucinating LLM cannot silently fail a stage.",
    description:
      "Code decides facts; an optional System One model handles semantic yes/no and classification calls such as " +
      "handoff validity, refusals, failure classes, and review-comment severity. Decisions are thresholded, abstain " +
      "when unsure, run in shadow first, and are benchmarked against an LLM-judge baseline.",
    maturity: "experimental",
    availability: "available",
    prerequisites: ["A configured decision model", "Opt-in per stage; off by default"],
    issues: [6073, 6330, 6334, 8090],
    surfaces: [
      {
        kind: "cli",
        command: "goobers decide --name <id> --question <text> --state-file <path>",
        description: "Ask the decision model a thresholded yes/no question as a workflow stage.",
      },
      { kind: "docs", label: "Decide step guide", path: "docs/guides/decision-gate-decide-step.md" },
      { kind: "docs", label: "A/B harness guide", path: "docs/guides/decision-gate-ab-harness.md" },
    ],
  },
  {
    id: "workflow-gym",
    name: "Workflow Gym",
    summary:
      "Benchmarks, exact replay, and synthetic and adversarial tasks where workflow versions compete before promotion.",
    description:
      "A versioned task registry with historical replay against virtualized side effects, synthetic edge-case and " +
      "adversarial scenarios, and a tournament runner that compares workflow versions on identical suites, with " +
      "regression gates, Pareto reporting, and overfitting detection before shadow or A/B promotion.",
    maturity: "concept",
    availability: "coming-soon",
    prerequisites: ["Retained historical runs to replay", "Candidate workflow versions to compare"],
    issues: [3594, 3598],
  },
  {
    id: "rszz-review-eval",
    name: "R-SZZ review evaluation",
    summary:
      "Measures the merge-review agent's recall and precision on real bugs traced back to the change that introduced them.",
    description:
      "Uses fixed bugs as ground truth: refactoring-aware R-SZZ traces each fix back through blame to the bug-introducing " +
      "change, then checks whether the merge-review agent flagged it. This scores reviewer prompt, model, or instruction " +
      "changes offline and feeds Backprop as an evaluation signal.",
    maturity: "concept",
    availability: "coming-soon",
    prerequisites: ["Merged bug fixes linked to bug issues", "Recorded merge-review agent comments"],
    issues: [7112],
  },
];
export function findLabsFeature(id: string | undefined): LabsFeature | undefined {
  return id ? labsFeatures.find((feature) => feature.id === id) : undefined;
}

export function labsMaturityLabel(maturity: LabsMaturity): string {
  switch (maturity) {
    case "concept":
      return "Concept";
    case "experimental":
      return "Experimental";
    case "preview":
      return "Preview";
  }
}
