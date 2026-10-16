import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient, fixtureKey } from "../api/fixtureClient";
import type { ChildHistoryPage } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { LiveDataProvider } from "../liveData";
import { ChildHistoryPanel } from "./ChildHistoryPanel";

const runId = "01JZ402DASHBOARD";
const time = "2026-10-09T15:00:00Z";
function page(): ChildHistoryPage {
  return { runId, gaggle: "core", status: "recorded", observedAt: time, nextCursor: "next", items: [
    { childId: "a", runId: "child-a", stageOccurrence: "stage-1", invocationKey: "inspect-a", state: "running", cancellationRequested: true, acceptedAt: time, updatedAt: time },
    { childId: "b", runId: "child-b", stageOccurrence: "stage-2", invocationKey: "inspect-b", state: "cancelled", cancellationRequested: true, acceptedAt: time, updatedAt: time, terminalAt: time, acknowledgedAt: time, expiredAt: time },
  ] };
}

function mount(value = page()) {
  const fixtures = populatedDaemonFixtures();
  fixtures.runChildren = { [fixtureKey(runId, "")]: value, [fixtureKey(runId, "next")]: { ...page(), items: [{ ...page().items[0], invocationKey: "next-child", state: "queued", cancellationRequested: false }], nextCursor: "" } };
  const client = new FixtureDaemonClient(fixtures);
  const read = vi.spyOn(client, "listRunChildren");
  const navigate = vi.fn();
  render(<LiveDataProvider client={client}><ChildHistoryPanel client={client} runId={runId} gaggle="core" navigate={navigate} /></LiveDataProvider>);
  return { client, read, navigate };
}

describe("child acceptance history", () => {
  it("keeps cancellation intent separate from outcome and hides expired result links", async () => {
    const { navigate } = mount();
    expect(await screen.findByText("Cancellation requested; stopping is not yet confirmed.")).toBeInTheDocument();
    expect(screen.getByText(/Saved child result expired/)).toBeInTheDocument();
    expect(screen.getByText(/Parent acknowledged/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open child run child-b" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Open child run child-a" }));
    expect(navigate).toHaveBeenCalledWith({ page: "run", id: "child-a" });
  });

  it("pages, returns to the first page on refresh, and does not link unstarted children", async () => {
    const { read } = mount();
    await screen.findByText("inspect-a");
    fireEvent.click(screen.getByRole("button", { name: "Next children" }));
    await screen.findByText("next-child");
    expect(screen.queryByRole("button", { name: /Open child run/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next children" })).toBeDisabled();
    expect(read).toHaveBeenLastCalledWith(runId, "next", expect.anything());
    fireEvent.click(screen.getByRole("button", { name: "Refresh child history" }));
    await screen.findByText("inspect-a");
    await waitFor(() => expect(read).toHaveBeenLastCalledWith(runId, "", expect.anything()));
    expect(screen.getByRole("button", { name: "Previous children" })).toBeDisabled();
  });

  it("distinguishes an unavailable source from an empty history", async () => {
    mount({ ...page(), status: "unavailable", items: [], nextCursor: "" });
    await screen.findByText("Child history is unavailable from this daemon.");
    expect(screen.queryByText("No retained child acceptances.")).not.toBeInTheDocument();
  });

  it("rejects history for a different parent", async () => {
    mount({ ...page(), runId: "foreign" });
    expect(await screen.findByRole("alert")).toHaveTextContent("mismatched child history");
    expect(screen.queryByText("inspect-a")).not.toBeInTheDocument();
  });
});
