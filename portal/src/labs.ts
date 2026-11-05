// Labs is the Portal's single home for experimental features (#8320). Each
// feature is one registry entry plus, once it has a UI, one page component.
// Labs features are read-only and advisory: they never write configuration
// or bypass validation, promotion, or approval gates.

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
}

export const labsIssueBaseUrl = "https://github.com/Agent-Clubhouse/Goobers/issues/";

export const labsFeatures: readonly LabsFeature[] = [
  {
    id: "workflow-clustering",
    name: "Workflow clustering",
    summary:
      "Groups agent-authored child workflows by shape and behavior to surface recurring patterns, novel proposals, and drift.",
    description:
      "Clusters agent-authored (dynamic) child workflow proposals using structural features and aggregated run behavior. " +
      "Recurring successful shapes become template candidates, proposals that match no known cluster are flagged for closer review, " +
      "and cluster occupancy is tracked across model, harness, and authoring-skill versions. Results are advisory only. " +
      "The cluster map below groups the instance's configured workflows by structural shape today; run-behavior descriptors and child proposals join it as they land.",
    maturity: "experimental",
    availability: "available",
    prerequisites: [
      "At least three configured workflows for the cluster map (more give more stable clusters)",
      "Agent-authored child workflows enabled (preview DSL 3.1)",
      "Retained child workflow proposals and run outcomes in the journal",
      "Enough proposals per task cohort to form stable clusters",
    ],
    issues: [8302, 8303, 8304, 8305, 8307, 8309, 8310, 8311],
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
