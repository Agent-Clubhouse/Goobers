import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { ContinuationLineagePanel } from "./ContinuationLineagePanel";

it("marks partial history and replaces bounded continuation pages", async () => {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  const getRun = vi.spyOn(client, "getRun").mockResolvedValue({ id: "source", lineage: { historicalRepassCount: 2, continuations: [{ id: "child-next", phase: "completed" }] } } as Awaited<ReturnType<typeof client.getRun>>);
  render(<ContinuationLineagePanel client={client} runId="source" navigate={vi.fn()} lineage={{
    source: { id: "ancestor" }, historicalRepassCount: 2, historicalRepassCountPartial: true,
    continuations: [{ id: "child-first" }], continuationsTruncated: true, continuationsNextCursor: "child-first", continuationsIncomplete: true,
  }} />);
  expect(screen.getByText(/partial history/)).toBeInTheDocument();
  expect(screen.getByText(/incomplete or unavailable/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Next continuations" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "child-next" })).toBeInTheDocument());
  expect(getRun).toHaveBeenCalledWith("source", expect.objectContaining({ continuationsAfter: "child-first" }));
  expect(screen.queryByRole("button", { name: "child-first" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Next continuations" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "First continuations" }));
  expect(screen.getByRole("button", { name: "child-first" })).toBeInTheDocument();
});
