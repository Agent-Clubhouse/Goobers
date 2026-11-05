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
  /** Steps to try the feature on your own instance, or to prepare for it while it is coming soon. */
  tryIt: LabsStep[];
  /** How to wire the feature into workflows once it has been tried. */
  integrate: LabsStep[];
}

export interface LabsStep {
  title: string;
  detail: string;
  /** Command or configuration to copy, shown as a code block. */
  code?: string;
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
      "Clusters configured and agent-authored (dynamic) child workflows with HDBSCAN over versioned structural features " +
      "and journal-derived run behavior. Stable clusters reveal what self-spawning workflows converge to; proposals that " +
      "match no known cluster are flagged as novel; confound checks warn when clusters merely mirror a gaggle. " +
      "Recurring successful shapes become template candidates. Results are advisory only. " +
      "The cluster map below groups the instance's configured workflows by structural shape today; run-behavior descriptors and child proposals join it as they land.",
    maturity: "experimental",
    availability: "available",
    prerequisites: [
      "At least three configured workflows for the cluster map (more give more stable clusters)",
      "Agent-authored child workflows enabled (preview DSL 3.1) for dynamic-workflow analysis",
      "Retained run journals for behavior clustering",    ],
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
    tryIt: [
      {
        title: "Look at the cluster map",
        detail:
          "The map above clusters this instance's configured workflows by structure. Each cluster lists its members, " +
          "its most typical workflow (medoid), and the features that set it apart. Workflows that fit no cluster are noise.",
      },
      {
        title: "Run it from the CLI",
        detail:
          "Fit the same model in a terminal. Stability below 0.6 (UNSTABLE) means the cluster may not survive resampling, " +
          "which is common on instances with only a handful of workflows.",
        code: "goobers workflow clusters ./instance",
      },
      {
        title: "Check the clusters are not just your gaggles",
        detail:
          "If cluster membership simply mirrors which gaggle a workflow belongs to, the clusters tell you about " +
          "organization, not convergence. The confound check reports Cramér's V and a permutation p-value.",
        code: "goobers workflow clusters --stratify gaggle ./instance",
      },
      {
        title: "Cluster by how runs behave",
        detail:
          "Behavior clustering reads retained run journals (stages run, retries, gate outcomes, children spawned) " +
          "instead of workflow structure. Workflows that look different can behave alike, and the reverse.",
        code: "goobers workflow clusters --features behavior --since 168h ./instance",
      },
    ],
    integrate: [
      {
        title: "Save a reference model",
        detail: "Fit a model on workflows you trust and keep the JSON next to your instance configuration.",
        code: "goobers workflow clusters --json ./instance > clusters.json",
      },
      {
        title: "Score dynamic child workflows for novelty",
        detail:
          "When an agent proposes a child workflow, validate it against the saved model. The report says which " +
          "cluster the proposal resembles, or flags it as novel so a human reviews it more closely. Novelty is advisory " +
          "and never blocks validation.",
        code:
          "goobers workflow validate-child --gaggle <gaggle> --parent <workflow> --stage <stage> \\\n" +
          "  --cluster-model clusters.json proposal.yaml ./instance",
      },
      {
        title: "Refit as workflows change",
        detail:
          "Refit after adding or changing workflows. The model hash changes whenever the feature schema or inputs change, " +
          "so compare hashes to know whether two reports are comparable.",
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
    tryIt: [
      {
        title: "Observe a whole gaggle in shadow mode",
        detail:
          "Shadow mode changes nothing: it never files findings or affects gates. Set it on a Gaggle to observe all " +
          "of its workflows without editing them.",
        code: "kind: Gaggle\nspec:\n  backprop:\n    mode: shadow",
      },
      {
        title: "Let some runs finish, then read the report",
        detail: "Only runs that start after you enable shadow mode are analyzed. The report shows what Backprop would have attributed.",
        code: "goobers telemetry shadow ./instance",
      },
      {
        title: "Browse attribution in the Portal",
        detail: "Insights → Backprop shows cohorts, credit and blame per stage, and fault-audit findings.",
      },
    ],
    integrate: [
      {
        title: "Enroll a workflow",
        detail:
          "Backprop needs DSL 3.0 or 3.1 with the preview acknowledgement. Run goobers fix --to 3.0 to migrate an older workflow first.",
        code:
          'dslVersion: "3.0"\nmetadata:\n  annotations:\n    goobers.dev/allow-preview-features: "true"\nspec:\n  backprop:\n    enabled: true\n    version: v1',
      },
      {
        title: "Validate and confirm",
        detail: "goobers status prints backprop: active (v1) under each enrolled workflow.",
        code: "goobers validate ./instance\ngoobers status ./instance",
      },
      {
        title: "Act on findings",
        detail: "Record fixes against findings and let Backprop verify them on later runs. See the Backprop guide for details.",
      },
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
    tryIt: [
      {
        title: "Turn on the decision gate in shadow mode",
        detail:
          "Add decisionGate to the instance config. Endpoint and model names come from environment variables, never inline. " +
          "In shadow mode the model is called and recorded, but every decision is reported as uncertain.",
        code:
          "decisionGate:\n  mode: shadow          # off | shadow | enforce\n  auth: entra           # omit to use a key from keyEnv\n  baseURLEnv: DECISION_BASE_URL\n  modelEnv: DECISION_MODEL\n  fallback: agent       # agent | escalate | fail",
      },
      {
        title: "Ask a question by hand",
        detail: "Try a yes/no question against a JSON file before putting it in a workflow.",
        code: 'goobers decide --name handoff-complete --question "Does the summary address every requested item?" --state-file handoff.json',
      },
    ],
    integrate: [
      {
        title: "Add a decide stage",
        detail:
          "A deterministic stage writes yes, no, uncertain or unavailable to its result file. Pass --optional so the stage " +
          "is safe in shared workflows when the gate is off.",
        code:
          "- name: judge-handoff\n  type: deterministic\n  run:\n    command: [goobers, decide, --name, handoff-complete,\n              --question, \"Does the summary address every requested item?\",\n              --state-file, handoff.json, --accept, \"0.9\", --reject, \"0.1\", --optional]\n  inputs:\n    resultFile: decide.json",
      },
      {
        title: "Compare against an LLM judge, then enforce",
        detail:
          "Use the A/B harness to compare the decision model with the LLM-judge baseline. Switch decisionGate.mode to " +
          "enforce only once its accuracy is acceptable.",
      },
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
    tryIt: [
      {
        title: "Not implemented yet",
        detail:
          "There is nothing to run today. To get ready, keep run journals retained: replay suites will be built from them.",
      },
    ],
    integrate: [
      {
        title: "Planned: gate promotion on a tournament",
        detail:
          "A candidate workflow version will run against the same replayed and synthetic tasks as the current version, " +
          "and will only be promoted to shadow or A/B when it does not regress.",
      },
    ],
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
    tryIt: [
      {
        title: "Not implemented yet",
        detail:
          "There is nothing to run today. To get ready, link bug-fix pull requests to their bug issues: those links are " +
          "the ground truth R-SZZ traces back to the change that introduced the bug.",
      },
    ],
    integrate: [
      {
        title: "Planned: score reviewer changes offline",
        detail:
          "Before changing the merge-review agent's prompt, model, or instructions, you will be able to replay it on " +
          "historical bug-introducing changes and compare its recall and precision with the current reviewer.",
      },
    ],
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
