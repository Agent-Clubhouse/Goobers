import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { SourceView, WorkbenchGraph as Graph } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { WorkbenchGraph } from "./WorkbenchGraph";

const sources: SourceView[] = [{ bindingId: "strategy", kind: "documents", provider: "github", owner: "acme", repository: "strategy", paths: ["goal.md"] }, { bindingId: "issues", kind: "backlog", provider: "github", owner: "acme", repository: "code" }];
const goal = { gaggleId: "team", sourceBindingId: "strategy", kind: "objective-document", sourceId: "goal" };
const item = { gaggleId: "team", sourceBindingId: "issues", kind: "work-item", sourceId: "42" };
const graph: Graph = {
  generation: "one", gaggleId: "team", partial: true, documents: [], aliases: [], conflicts: [],
  sources: [{ sourceBindingId: "strategy", kind: "documents", status: "complete", consistency: "commit-pinned" }, { sourceBindingId: "issues", kind: "backlog", status: "partial", consistency: "native-non-snapshot", reasons: ["window-only"] }],
  nodes: [
    { key: "goal", conflict: false, observations: [{ ref: goal, contentDigest: "a", title: "Human operations", revision: "commit", locator: { id: "goal.md" }, objective: true, path: "goal.md" }] },
    { key: "item", conflict: false, observations: [{ ref: item, contentDigest: "b", title: "Unblock a run", revision: "2", locator: { id: "42" }, objective: false }] },
  ],
  edges: [{ key: "link", kind: "contributes-to", from: { ref: item, resolved: true }, to: { ref: goal, resolved: true }, owner: { kind: "manifest", sourceBindingId: "strategy", path: "links.yaml" }, origin: "authored", conflict: false }],
};
function setup(value = graph) {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getWorkbenchGraph").mockResolvedValue(value);
  vi.spyOn(client, "getWorkbenchItem");
  vi.spyOn(client, "getWorkbenchDocuments");
  return { client, ...render(<WorkbenchGraph client={client} gaggle="team" generation="one" sources={sources} />) };
}
async function load() { fireEvent.click(screen.getByRole("button", { name: "Load relationships" })); await screen.findByText(/2 source items/); }

describe("source relationship map", () => {
  it("loads explicitly and follows incoming contribution links with keyboard navigation without fetching targets", async () => {
    const { client } = setup(); expect(client.getWorkbenchGraph).not.toHaveBeenCalled();
    await load(); fireEvent.click(screen.getByRole("button", { name: "Human operations" }));
    expect(screen.getByText(/Partial coverage/)).toBeInTheDocument();
    const diagram = screen.getByRole("group", { name: "Declared relationship directions" });
    const line = diagram.querySelector("line")!;
    expect(Number(line.getAttribute("x1"))).toBeLessThan(Number(line.getAttribute("x2")));
    expect(line.getAttribute("marker-end")).toMatch(/^url/);
    fireEvent.keyDown(within(diagram).getByRole("button", { name: "Explore Unblock a run" }), { key: "Enter" });
    expect(within(screen.getByRole("region", { name: "Selected objective relationships" })).getByRole("heading", { name: "Unblock a run" })).toBeInTheDocument();
    expect(client.getWorkbenchGraph).toHaveBeenCalledTimes(1);
    expect(client.getWorkbenchItem).not.toHaveBeenCalled(); expect(client.getWorkbenchDocuments).not.toHaveBeenCalled();
  });
  it("retains unresolved and ownership-conflicting edges as text without drawing verified connections", async () => {
    const value = structuredClone(graph);
    value.edges.push({ ...value.edges[0], key: "unread", to: { ref: { ...goal, sourceId: "not-loaded" }, resolved: false } });
    value.edges[0].conflict = true;
    setup(value); await load(); fireEvent.click(screen.getByRole("checkbox", { name: "Show objectives only" }));
    fireEvent.click(screen.getByRole("button", { name: "Unblock a run" }));
    const region = screen.getByRole("region", { name: "Selected objective relationships" });
    expect(within(region).getByText(/source ownership conflict/)).toBeInTheDocument();
    expect(within(region).getByText(/linked content not loaded/)).toBeInTheDocument();
    expect(region.querySelectorAll("line")).toHaveLength(0);
  });
  it("shows every conflicting observation without choosing a source winner or executing labels", async () => {
    const value = structuredClone(graph); value.nodes[0].conflict = true;
    value.nodes[0].observations.push({ ...value.nodes[0].observations[0], title: '<img src="https://tracking.invalid">', revision: "other", path: "other.md" });
    setup(value); await load(); fireEvent.click(screen.getByRole("button", { name: /Conflicting source observations/ }));
    const region = screen.getByRole("region", { name: "Selected objective relationships" });
    expect(within(region).getByText(/Human operations · strategy · goal.md/)).toBeInTheDocument();
    expect(within(region).getByText(/<img src=/)).toBeInTheDocument();
    expect(region.querySelector("svg, img, iframe")).toBeNull();
  });
  it("clears content before refresh and discards denied or mismatched generation responses", async () => {
    const { client } = setup(); await load();
    vi.mocked(client.getWorkbenchGraph).mockRejectedValueOnce(new Error("denied"));
    fireEvent.click(screen.getByRole("button", { name: "Refresh relationships" }));
    expect(screen.queryByText(/2 source items/)).not.toBeInTheDocument();
    await screen.findByText(/Relationships are unavailable/);
    vi.mocked(client.getWorkbenchGraph).mockResolvedValueOnce({ ...graph, generation: "changed" });
    fireEvent.click(screen.getByRole("button", { name: "Refresh relationships" }));
    await screen.findByText(/Planning sources changed/);
    expect(screen.queryByText(/2 source items/)).not.toBeInTheDocument();
  });
  it("ignores a late response from the previous gaggle", async () => {
    const { client, rerender } = setup(); let resolve!: (value: Graph) => void;
    vi.mocked(client.getWorkbenchGraph).mockReturnValueOnce(new Promise((done) => { resolve = done; }));
    fireEvent.click(screen.getByRole("button", { name: "Load relationships" }));
    vi.mocked(client.getWorkbenchGraph).mockReturnValue(new Promise(() => {}));
    rerender(<WorkbenchGraph client={client} gaggle="other" generation="one" sources={sources} />);
    await act(async () => { resolve(graph); });
    expect(screen.queryByText(/2 source items/)).not.toBeInTheDocument();
  });
});
