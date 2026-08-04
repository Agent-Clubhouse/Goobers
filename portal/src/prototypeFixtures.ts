import type { ValidationWarning } from "./api/types";

export type StageKind = "deterministic" | "agentic" | "gate";

export interface WorkflowStage {
  id: string;
  name: string;
  description: string;
  kind: StageKind;
  goober?: string;
  evaluator?: string;
  retry: string;
  x: number;
  y: number;
  yaml: string;
}

export const workflowWarnings: Record<string, readonly ValidationWarning[]> = {
  implementation: [
    {
      code: "VER003",
      severity: "warning",
      scope:
        "gaggles/goobers/workflows/implementation.yaml Gaggle/goobers Workflow/implementation",
      explanation:
        "expectedOutputs is declared but the stage has no inputs.resultFile to emit it through",
    },
  ],
};

export const instanceWarnings: readonly ValidationWarning[] = [
  ...workflowWarnings.implementation,
  {
    code: "MODEL002",
    severity: "warning",
    scope: "Goober/coder",
    explanation: "requested model is unavailable; using the harness default",
  },
];
