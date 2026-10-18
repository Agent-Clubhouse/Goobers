import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { WorkflowGraph } from "../api/types";
import type { MapVisit } from "../runDetailData";
import { WorkflowTopologyGraph } from "./WorkflowTopologyGraph";

const mapGraph: WorkflowGraph = {
  name: "sharded-tests",
  version: 1,
  digest: "sha256:mapped",
  start: "plan",
  nodes: [
    { id: "plan", kind: "agentic", owner: "planner" },
    { id: "run-shards", kind: "map" },
    { id: "run-shard", kind: "deterministic" },
    { id: "collate", kind: "agentic", owner: "lead" },
  ],
  edges: [
    { source: "plan", target: "run-shards" },
    { source: "run-shards", target: "run-shard" },
    { source: "run-shard", target: "collate" },
    { source: "collate", target: "", terminal: "complete" },
  ],
};

describe("workflow topology graph map node (#7386)", () => {
  it("renders a map node distinctly with a per-item fan-out edge", () => {
    render(<WorkflowTopologyGraph graph={mapGraph} onSelectStage={() => {}} />);
    const node = screen.getByRole("button", {
      name: "run-shards, Map, Runs one task per roster item, configured",
    });
    expect(node).toHaveAttribute("data-node-kind", "map");
    expect(node).toHaveClass("workflow-node-map");
    expect(screen.getByText("per item", { selector: "text" })).toBeInTheDocument();
  });

  it("labels the fan-out edge with the frozen roster's item states in words", () => {
    const visit: MapVisit = {
      map: "run-shards",
      startedSeq: 3,
      sourceTask: "plan",
      sourceSlot: "shards",
      items: [
        { branch: 1, identity: "unit-a", digest: "sha256:a", state: "succeeded", seq: 5 },
        { branch: 2, identity: "unit-b", digest: "sha256:b", state: "running", seq: 6 },
      ],
    };
    render(
      <WorkflowTopologyGraph
        graph={mapGraph}
        mapVisits={new Map([["run-shards", visit]])}
        nodeStates={{ plan: "completed", "run-shards": "running", "run-shard": "running", collate: "pending" }}
        onSelectStage={() => {}}
        stateSeq={6}
      />,
    );
    expect(
      screen.getByText("2 items — 1 succeeded, 1 running", { selector: "text" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Outgoing: 2 items — 1 succeeded, 1 running to run-shard/),
    ).toBeInTheDocument();
  });
});
