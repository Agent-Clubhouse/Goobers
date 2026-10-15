import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { WorkflowGraphNode } from "../api/types";
import { LiveDataProvider } from "../liveData";
import type { MapVisit } from "../runDetailData";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { RunStageInspector } from "./RunStageInspector";

const mapNode: WorkflowGraphNode = { id: "run-shards", kind: "map" };
const DIGEST_A = `sha256:${"a1".repeat(32)}`;
const DIGEST_B = `sha256:${"b2".repeat(32)}`;

function renderMapInspector(mapVisit: MapVisit | undefined) {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  const listStageAttempts = vi.spyOn(client, "listStageAttempts");
  render(
    <LiveDataProvider client={client}>
      <RunStageInspector client={client} mapVisit={mapVisit} node={mapNode} runId="run-123" selectedSeq={9} />
    </LiveDataProvider>,
  );
  return listStageAttempts;
}

describe("run stage inspector map items (#7386)", () => {
  it("lists every frozen roster item with its identity, digest and state", () => {
    const listStageAttempts = renderMapInspector({
      map: "run-shards",
      startedSeq: 3,
      sourceTask: "plan",
      sourceSlot: "shards",
      items: [
        { branch: 1, identity: "unit-a", digest: DIGEST_A, state: "succeeded", seq: 5 },
        { branch: 2, identity: "unit-b", digest: DIGEST_B, state: "pending" },
      ],
    });
    const list = screen.getByRole("region", { name: "run-shards map items" });
    expect(
      within(list).getByText("2 items — 1 succeeded, 1 pending, frozen from plan.shards at sequence 3."),
    ).toBeInTheDocument();
    const rows = within(list).getAllByRole("listitem");
    expect(rows.map((row) => row.textContent)).toEqual([
      "unit-aa1a1a1a1a1a1Succeeded",
      "unit-bb2b2b2b2b2b2Pending",
    ]);
    expect(within(rows[0]).getByText("a1a1a1a1a1a1")).toHaveAttribute("title", DIGEST_A);
    // A map is a fan-out container: it has items, never stage attempts.
    expect(listStageAttempts).not.toHaveBeenCalled();
  });

  it("says the roster is not frozen before the map starts", () => {
    renderMapInspector(undefined);
    expect(screen.getByText("The map freezes its roster when it starts.")).toBeInTheDocument();
  });
});
