// Labs is the Portal's single home for experimental features (#8320). Each
// feature is one registry entry plus, once it has a UI, one page component.
// Labs features are advisory: they never write configuration or bypass
// validation, promotion, or approval gates. The only Labs writes are opt-in,
// per-user presentation data (Fluid UI customizations, #8415) that affect
// nobody else and never reach runtime state.

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
  /** Planned improvements, shown as "What's next" so users can see where the feature is heading. */
  roadmap?: LabsRoadmapItem[];
}

export interface LabsRoadmapItem {
  title: string;
  /** Why it matters to users, in one or two sentences. */
  detail: string;
  issue: number;
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
      "Retained child runs show occupancy, diversity, fitness, and drift across the authoring stage's model and harness versions. " +
      "Recurring successful shapes become template candidates. Results are advisory only.",
    maturity: "experimental",
    availability: "available",
    prerequisites: [
      "At least three configured workflows for the cluster map (more give more stable clusters)",
      "Agent-authored child workflows enabled (preview DSL 3.1) for dynamic-workflow analysis",
      "Retained run journals for behavior clustering",    ],
    issues: [8302, 8303, 8304, 8305, 8306, 8307, 8308, 8309, 8310, 8311, 8340, 8341, 8342, 8343, 8344, 8345, 8373, 8374, 8375, 8376, 8377, 8378],
    surfaces: [
      {
        kind: "cli",
        command: "goobers workflow clusters [--fitness] [--cohort <label>] [--stratify <axis>] [--ablation] [--json]",
        description:
          "Fit structural clusters, score how well each cluster's runs go, and check clusters against gaggle, task, " +
          "template, model, harness, skills, and tool confounds.",
      },
      {
        kind: "cli",
        command: "goobers fleet join --projection labs.workflowFeatures ...",
        description: "Let the fleet collect this instance's privacy-safe workflow features for fleet-wide convergence.",
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
        title: "See which shapes actually work",
        detail:
          "Fitness joins retained runs to each cluster: run count, success rate with a 95% interval, wall time, cost, " +
          "and how often a human gate, escalation, or retry loop was hit. Clusters with fewer than 5 runs are marked " +
          "insufficient and never ranked, so convergence toward a good shape is distinguishable from the model's habits.",
        code: "goobers workflow clusters --fitness --since 720h ./instance",
      },
      {
        title: "Compare like with like",
        detail:
          "Workflows are grouped into task cohorts from triggers, work-item kinds, and side effects (never names or goal " +
          "text), or from an explicit goobers.dev/task-class label. Cluster one cohort at a time, or check whether clusters " +
          "just mirror the task.",
        code: "goobers workflow clusters --cohort <label> ./instance\ngoobers workflow clusters --stratify cohort ./instance",
      },
      {
        title: "Is it the model, not the workflow?",
        detail:
          "Stratify by the model, harness, skill set, or tool set that ran each workflow. A strong association means the " +
          "cluster reflects the runtime rather than a converged shape.",
        code: "goobers workflow clusters --stratify model ./instance",
      },
      {
        title: "Look beyond the starter templates",
        detail:
          "Workflows within 0.35 structural distance of a shipped template are marked template-derived. Clusters whose " +
          "typical workflow is farther than that from every template are reported as convergence beyond templates.",
        code: "goobers workflow clusters --stratify template ./instance",
      },
      {
        title: "Find what drives the clusters",
        detail:
          "Ablation refits with each feature dropped and reports how much the clusters move, flagging features that " +
          "dominate. Large populations use a deterministic sample, so thousands of workflows stay fast.",
        code: "goobers workflow clusters --ablation ./instance",
      },
      {
        title: "Cluster by how runs behave",
        detail:
          "Behavior clustering reads retained run journals (stages run, retries, gate outcomes, children spawned) " +
          "instead of workflow structure. Workflows that look different can behave alike, and the reverse.",
        code: "goobers workflow clusters --features behavior --since 168h ./instance",
      },
      {
        title: "Track agent-authored shape drift",
        detail:
          "The cluster map compares retained child workflows against its frozen model and reports occupancy, entropy, " +
          "pairwise shape distance, and completion fitness by the authoring stage's model and harness version. " +
          "Authoring-skill cohorts wait for a journal digest scoped to the authoring goober's skills; until then those samples are counted as missing provenance. " +
          "Convergence and mode-collapse labels are observational signals, not null-baseline claims.",
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
      {
        title: "Turn recurring shapes into templates",
        detail:
          "Given joined child-proposal evidence and a saved model, each stable cluster that clears the evidence bar " +
          "(stability, support across parent runs and repositories, no policy findings, outcomes no worse than " +
          "alternatives) is proposed as a review-required template. Nothing is written or registered.",
        code: "goobers workflow clusters --templates evidence.json --cluster-model clusters.json",
      },
      {
        title: "Run convergence across your fleet",
        detail:
          "Grant the workflow-features projection when an instance joins the fleet. The fleet collects feature vectors " +
          "only (no workflow, gaggle, goober, or goal text; names are salted hashes) and its portal shows which shapes " +
          "the whole fleet settles on, which clusters are shared or private, and which instances are outliers. Cloned " +
          "workflows count once. Without a fleet service, collect snapshots by hand and cluster them.",
        code:
          "goobers fleet join --url <fleet-url> --projection labs.workflowFeatures ./instance\n" +
          "goobers workflow clusters --snapshot ./instance > snapshots/<instance>.json\n" +
          "goobers workflow clusters --fleet snapshots",
      },
    ],
    roadmap: [
      {
        title: "Keep self-improvement diverse",
        detail:
          "A quality-diversity archive (MAP-Elites style) so self-improving search does not collapse onto one shape.",
        issue: 8308,
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
    prerequisites: ["A configured decision model", "Opt-in per stage; off by default", "For agent stages: decisionGate.agentTool.tasks lists the task"],
    issues: [6073, 6330, 6334, 8090, 8263, 8264, 8265],
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
        title: "Let an agent stage ask (opt-in, budgeted)",
        detail:
          "Grant the decide tool to specific agent tasks with decisionGate.agentTool. It is denied by default, limited to " +
          "configured question names, budgeted per stage, and the agent never sees the endpoint or credential. " +
          "Shadow mode always answers uncertain. Local runner only for now.",
        code:
          "decisionGate:\n  mode: shadow\n  agentTool:\n    tasks: [review-handoff]   # specific task ids\n    maxCalls: 5\n  gate:\n    thresholds:\n      handoff-complete: {accept: 0.9, reject: 0.1}",
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
  {
    id: "fluid-ui",
    name: "Fluid UI",
    summary:
      "Describe how you want the Portal to look or behave, and an agent builds it as your own per-user customization.",
    description:
      "You write a request such as \"make my run list compact and put failing runs first\". A Goobers agent implements " +
      "it as a versioned, per-user customization of supported layout, density, panels, and ordering, validates it, and " +
      "lets you preview, apply, revise, or reset it. Customizations never change other users' experience, global " +
      "defaults, authorization, or workflow gates. Recurring, well-adopted customizations (ranked by distinct users, " +
      "not raw requests) become candidates for the standard Portal.",
    maturity: "experimental",
    availability: "available",
    prerequisites: [
      "A running Goobers daemon or dashboard (the per-user customization store is built in)",
      "Natural-language requests need a running daemon with the goobers reference gaggle (fluid-ui-customize)",
      "Authenticated Portal users on shared instances; local dashboards use one local profile",
    ],
    issues: [8338, 8415, 8416, 8417, 8418, 8419],
    surfaces: [
      { kind: "portal", label: "Labs → Fluid UI", route: { page: "labs", feature: "fluid-ui" } },
      { kind: "docs", label: "Fluid UI customization API", path: "docs/labs-fluid-ui.md" },
    ],
    tryIt: [
      {
        title: "Ask in plain language",
        detail:
          "Open the Feedback tab below and describe the change you want. The shipped fluid-ui-customize workflow " +
          "turns it into a validated customization; follow its run from the request. Only you can see your requests.",
      },
      {
        title: "Preview, then apply",
        detail:
          "\"Your UX is ready\" appears only once the run has produced a validated candidate. Preview it in this " +
          "browser, then Apply it; nothing is applied automatically. Revert or Undo restores your previous " +
          "customization. A failed request shows why and can be retried; a request Fluid UI cannot do yet is " +
          "marked unsupported.",
      },
      {
        title: "Or submit a request through the API",
        detail:
          "Submitting starts a workflow run through trigger ingest, so it needs a running daemon (goobers up). " +
          "Status always reflects that run.",
        code:
          "curl -X POST \"$GOOBERS_API/api/v1/labs/fluid-ui/requests\" \\\n" +
          "  -H 'Content-Type: application/json' -d '{\"prompt\":\"make my run list compact, failing runs first\"}'\n" +
          "curl \"$GOOBERS_API/api/v1/labs/fluid-ui/requests\"\n" +
          "curl -X POST \"$GOOBERS_API/api/v1/labs/fluid-ui/requests/<id>/apply\" -H 'Content-Type: application/json' -d '{}'",
      },
      {
        title: "Edit and preview",
        detail:
          "Use the editor below to choose density, run-list sort and phases, which Overview panels show and in what " +
          "order, and saved views. Preview applies the draft in this browser only; nothing is saved.",
      },
      {
        title: "Apply, undo, or reset",
        detail:
          "Apply saves your customization so it survives a reload. Undo restores your previous revision; reset " +
          "returns you to the standard Portal. It affects nobody else and never touches configuration or gates.",
      },
      {
        title: "Or save a customization through the API",
        detail:
          "The same validated document can be stored directly. An invalid or incompatible document falls back to " +
          "the standard Portal with a notice.",
        code:
          "curl -X PUT \"$GOOBERS_API/api/v1/labs/fluid-ui/customization\" \\\n" +
          "  -H 'Content-Type: application/json' \\\n" +
          "  -d '{\"version\":\"portal.customization/v1\",\"density\":\"compact\",\"runList\":{\"sort\":\"failing-first\"}}'",
      },
      {
        title: "Undo or reset through the API",
        detail: "Undo restores your previous revision; reset returns you to the standard Portal and can itself be undone.",
        code:
          "curl -X POST \"$GOOBERS_API/api/v1/labs/fluid-ui/customization/undo\" -H 'Content-Type: application/json' -d '{}'\n" +
          "curl -X DELETE \"$GOOBERS_API/api/v1/labs/fluid-ui/customization\" -H 'Content-Type: application/json' -d '{}'",
      },
    ],
    integrate: [
      {
        title: "Request, preview, apply",
        detail:
          "Enter a request in the Feedback tab, follow its run while the fluid-ui-customize workflow builds a " +
          "validated customization document (never code), then preview and apply. Undo, revert, or reset at any time.",
      },
      {
        title: "Run the shipped workflow in your gaggle",
        detail:
          "The goobers reference gaggle ships fluid-ui-customize and its fluid-ui-designer goober. The daemon " +
          "validates every candidate with the same rules as the store, gives invalid output one retry with the " +
          "errors, and records out-of-surface requests as unsupported with a short unmet-need label.",
      },
    ],
    roadmap: [
      {
        title: "Popular customizations become defaults",
        detail: "Customizations adopted by many distinct users are ranked as candidates for the standard Portal.",
        issue: 8419,
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
