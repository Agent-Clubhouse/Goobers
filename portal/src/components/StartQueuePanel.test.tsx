import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DaemonAuthError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { InteractiveCapabilities, StartQueueItem, StartQueuePage } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { StartQueuePanel } from "./StartQueuePanel";
const item: StartQueueItem = { acceptanceId: "trigger-0123456789abcdef0123456789abcdef", gaggle: "web", workflow: "repair", source: "human-restart", generation: "retained-generation", acceptedAt: "2026-10-04T00:00:00Z", deadline: "2026-10-11T00:00:00Z", state: "accepted", waitingReason: "Waiting for workflow capacity or budget." };
const access: InteractiveCapabilities = { gaggle: "web", policyConfigured: true, viewer: true, operator: true, sourceWriteMode: "pull-request", actions: [{ action: "queue.cancel", authorized: true, credentialConfigured: true, available: true, reasonCode: "" }] };
function setup() { const client = new FixtureDaemonClient(populatedDaemonFixtures()); vi.spyOn(client, "getStartQueue").mockResolvedValue({ gaggle: "web", items: [item], nextCursor: "next" }); vi.spyOn(client, "getInteractiveCapabilities").mockResolvedValue(access); vi.spyOn(client, "cancelQueuedStart"); return { client, ...render(<StartQueuePanel client={client} gaggle="web" />) }; }
describe("workflow start queue", () => {
  it("shows bounded receipt state, deadline and waiting reason without an unconfirmed run link", async () => {
    const { client } = setup(); await screen.findByText(item.waitingReason!); expect(screen.getByText("retained-generation")).toBeInTheDocument(); expect(screen.getByText(/Pending deadline/)).toBeInTheDocument(); expect(screen.queryByRole("link", { name: "Open execution" })).not.toBeInTheDocument();
    vi.mocked(client.getStartQueue).mockResolvedValueOnce({ gaggle: "web", items: [] }); fireEvent.click(screen.getByRole("button", { name: "Next starts" })); await screen.findByText("No starts in this queue window."); expect(screen.queryByText(item.waitingReason!)).not.toBeInTheDocument(); expect(client.getStartQueue).toHaveBeenLastCalledWith("web", { cursor: "next", limit: 25 }, expect.anything());
  });
  it("retries an unknown cancellation with its original key and preserves truthful result", async () => {
    const { client } = setup(); await screen.findByText(item.waitingReason!); vi.mocked(client.cancelQueuedStart).mockRejectedValueOnce(new Error("lost reply"));
    fireEvent.change(screen.getByLabelText("Cancellation reason"), { target: { value: "No longer needed" } }); fireEvent.click(screen.getByRole("button", { name: "Cancel start" })); await screen.findByText(/response is unknown/);
    const command = vi.mocked(client.cancelQueuedStart).mock.calls[0][2]; expect(screen.getByRole("button", { name: "Refresh start queue" })).toBeDisabled(); expect(screen.getByLabelText("Cancellation reason")).toBeDisabled();
    vi.mocked(client.cancelQueuedStart).mockResolvedValueOnce({ ...item, state: "rejected", disposition: "cancelled", cancellation: { ...command, actor: "alice", requestedAt: item.acceptedAt, state: "cancelled-before-dispatch" } });
    fireEvent.click(screen.getByRole("button", { name: "Retry same cancellation" })); await screen.findByText("Cancelled before execution."); expect(vi.mocked(client.cancelQueuedStart).mock.calls[1][2]).toEqual(command); expect(screen.queryByRole("link", { name: "Open execution" })).not.toBeInTheDocument(); expect(screen.queryByRole("button", { name: "Cancel start" })).not.toBeInTheDocument();
  });
  it("clears prior data on access failure and ignores late replies after gaggle or client changes", async () => {
    const { client, rerender } = setup(); await screen.findByText(item.waitingReason!); vi.mocked(client.getStartQueue).mockRejectedValueOnce(new DaemonAuthError(403)); fireEvent.click(screen.getByRole("button", { name: "Refresh start queue" })); await screen.findByText(/Start queue is unavailable/); expect(screen.queryByText(item.waitingReason!)).not.toBeInTheDocument();
    let resolve!: (value: StartQueuePage) => void; vi.mocked(client.getStartQueue).mockReturnValueOnce(new Promise((done) => { resolve = done; })); fireEvent.click(screen.getByRole("button", { name: "Refresh start queue" })); await waitFor(() => expect(client.getStartQueue).toHaveBeenCalledTimes(3));
    const next = new FixtureDaemonClient(populatedDaemonFixtures()); vi.spyOn(next, "getInteractiveCapabilities").mockResolvedValue({ ...access, gaggle: "other" }); vi.spyOn(next, "getStartQueue").mockResolvedValue({ gaggle: "other", items: [] }); rerender(<StartQueuePanel client={next} gaggle="other" />); await screen.findByText("No starts in this queue window."); await act(async () => resolve({ gaggle: "web", items: [item] })); expect(screen.queryByText(item.waitingReason!)).not.toBeInTheDocument();
  });
  it("does not offer cancellation to a viewer or show mismatched server scope", async () => {
    const { client } = setup(); await screen.findByText(item.waitingReason!); vi.mocked(client.getInteractiveCapabilities).mockResolvedValueOnce({ ...access, operator: false, actions: [] }); fireEvent.click(screen.getByRole("button", { name: "Refresh start queue" })); await screen.findByText(/Cancellation is unavailable/); expect(screen.getByRole("button", { name: "Cancel start" })).toBeDisabled();
    vi.mocked(client.getStartQueue).mockResolvedValueOnce({ gaggle: "foreign", items: [item] }); fireEvent.click(screen.getByRole("button", { name: "Refresh start queue" })); await screen.findByText(/Start queue is unavailable/); expect(screen.queryByText(item.waitingReason!)).not.toBeInTheDocument();
  });
});
