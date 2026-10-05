import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { goWireFixtures } from "../api/wire.generated";
import type { PRRepairCommand } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { PRRepairReceiptLookup } from "./PRRepairReceiptLookup";

const verified: PRRepairCommand = goWireFixtures.prRepairCommand;
const unknown: PRRepairCommand = { ...verified, state: "unknown", observations: undefined };
function clientFixture() {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getPRRepairCommand").mockResolvedValue(unknown);
  vi.spyOn(client, "checkPRRepairCommand").mockResolvedValue(verified);
  return client;
}
function load() {
  fireEvent.click(screen.getByText("Check a retained PR repair"));
  fireEvent.change(screen.getByLabelText("Repair command ID"), { target: { value: verified.id } });
  fireEvent.click(screen.getByRole("button", { name: "Load repair receipt" }));
}
describe("retained PR repair lookup", () => {
  it("requires separate explicit reads and preserves the original acknowledgement after positive proof", async () => {
    const client = clientFixture(); render(<PRRepairReceiptLookup client={client} gaggle="team" />);
    expect(client.getPRRepairCommand).not.toHaveBeenCalled(); expect(client.checkPRRepairCommand).not.toHaveBeenCalled();
    load(); await screen.findByText("Repair unknown");
    expect(client.getPRRepairCommand).toHaveBeenCalledExactlyOnceWith("team", verified.id, expect.anything());
    expect(client.checkPRRepairCommand).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Check repair provider state" }));
    await screen.findByText("Exact repair observed");
    expect(client.checkPRRepairCommand).toHaveBeenCalledExactlyOnceWith("team", verified.id, expect.anything());
    expect(screen.getByText(/Original provider acknowledgement: unknown. Original outcome: unknown./)).toBeInTheDocument();
    expect(screen.getByText(/checked by checker/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Check repair provider state" })).not.toBeInTheDocument();
  });
  it("explains unjoined attempts without offering settlement", async () => {
    const client = clientFixture(); vi.mocked(client.getPRRepairCommand).mockResolvedValue({ ...unknown, state: "attempting", receipt: undefined });
    render(<PRRepairReceiptLookup client={client} gaggle="team" />); load();
    await screen.findByText(/The original provider attempt has no joined receipt/);
    expect(screen.queryByRole("button", { name: "Check repair provider state" })).not.toBeInTheDocument();
    expect(client.checkPRRepairCommand).not.toHaveBeenCalled();
  });
  it("clears inaccessible evidence and never retries a failed check automatically", async () => {
    const client = clientFixture(); vi.mocked(client.checkPRRepairCommand).mockRejectedValue(new Error("secret provider text"));
    render(<PRRepairReceiptLookup client={client} gaggle="team" />); load();
    fireEvent.click(await screen.findByRole("button", { name: "Check repair provider state" }));
    await screen.findByRole("alert");
    expect(screen.queryByRole("region", { name: "Retained PR repair receipt" })).not.toBeInTheDocument();
    expect(screen.queryByText("secret provider text")).not.toBeInTheDocument();
    expect(client.checkPRRepairCommand).toHaveBeenCalledTimes(1);
  });
  it("discards old-client late responses and foreign command identity", async () => {
    const client = clientFixture(); let resolve!: (value: PRRepairCommand) => void;
    vi.mocked(client.getPRRepairCommand).mockReturnValue(new Promise((done) => { resolve = done; }));
    const { rerender } = render(<PRRepairReceiptLookup client={client} gaggle="team" />); load();
    const next = clientFixture(); vi.mocked(next.getPRRepairCommand).mockResolvedValue({ ...verified, id: `repair-${"f".repeat(32)}` });
    rerender(<PRRepairReceiptLookup client={next} gaggle="team" />);
    await act(async () => { resolve(verified); });
    expect(screen.queryByText("Exact repair observed")).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Repair command ID"), { target: { value: verified.id } });
    fireEvent.click(screen.getByRole("button", { name: "Load repair receipt" }));
    await screen.findByRole("alert");
    expect(screen.queryByText("Exact repair observed")).not.toBeInTheDocument();
  });
});
