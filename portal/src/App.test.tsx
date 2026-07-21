import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { FixtureDaemonClient } from "./api/fixtureClient";
import { emptyDaemonFixtures, populatedDaemonFixtures } from "./test/daemonFixtures";

const storedValues = new Map<string, string>();

beforeEach(() => {
  storedValues.clear();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      clear: () => storedValues.clear(),
      getItem: (key: string) => storedValues.get(key) ?? null,
      key: (index: number) => [...storedValues.keys()][index] ?? null,
      get length() {
        return storedValues.size;
      },
      removeItem: (key: string) => storedValues.delete(key),
      setItem: (key: string, value: string) => storedValues.set(key, value),
    } satisfies Storage,
  });
  delete document.documentElement.dataset.theme;
  document.querySelector('meta[name="goobers-dashboard-mode"]')?.remove();
});

describe("portal foundation", () => {
  beforeEach(() => {
    window.location.hash = "#/overview";
  });

  it("shows the operational overview", async () => {
    renderLiveApp();

    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
    expect(screen.getByText("Daemon ready")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Needs attention" })).toBeInTheDocument();
  });

  it("labels standalone read-only mode in the portal chrome", async () => {
    const mode = document.createElement("meta");
    mode.name = "goobers-dashboard-mode";
    mode.content = "standalone";
    document.head.append(mode);

    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(await screen.findByText("Standalone read-only")).toBeInTheDocument();
    expect(screen.getByText("Daemon not running; reading this instance locally")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Instance is ready." })).toBeInTheDocument();
    expect(screen.getByText("Local instance loaded")).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent("Live updates connected"),
    );
    expect(screen.queryByText("Daemon ready")).not.toBeInTheDocument();
    expect(screen.queryByText(/The daemon is ready/)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Workflows" }));
    expect(
      await screen.findByText(
        "The instance is ready. Provision a gaggle to make its workflows and goobers visible here.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/The daemon is ready/)).not.toBeInTheDocument();
  });

  it("keeps run loading copy local-read aware in standalone mode", async () => {
    const mode = document.createElement("meta");
    mode.name = "goobers-dashboard-mode";
    mode.content = "standalone";
    document.head.append(mode);
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(client, "getRun").mockImplementation(() => new Promise(() => {}));
    vi.spyOn(client, "listRunEvents").mockImplementation(() => new Promise(() => {}));
    const user = userEvent.setup();
    render(<App client={client} />);

    await user.click(await screen.findByRole("link", { name: "Open run 01JZ402DASHBOARD" }));

    expect(await screen.findByRole("heading", { name: "Loading run" })).toBeInTheDocument();
    expect(
      screen.getByText("Reading pinned identity, graph, and durable events from local instance files."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/durable events from the daemon/)).not.toBeInTheDocument();
  });

  it("opens a run from daemon data without later-slice controls", async () => {
    const user = userEvent.setup();
    renderLiveApp();

    await user.click(
      await screen.findByRole("link", { name: "Open run 01JZ402DASHBOARD" }),
    );
    expect(
      await screen.findByRole("heading", { name: "Run 01JZ402DASHBOARD" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Execution graph" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Event ledger" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /play|replay/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /attempt|escalation/i })).not.toBeInTheDocument();
  });

  it("uses the run's pinned workflow and derives graph state at the selected event", async () => {
    const user = userEvent.setup();
    renderLiveApp();

    await user.click(
      await screen.findByRole("link", { name: "Open run 01JZ402DASHBOARD" }),
    );
    expect(
      await screen.findByText("sha256:core", { selector: ".run-graph-pin .mono" }),
    ).toBeInTheDocument();

    await user.click(
      screen.getByRole("button", { name: /^Select sequence 4:/ }),
    );

    expect(
      screen.getByRole("button", {
        name: "implement, agentic, Running at sequence 4",
      }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.getByRole("button", {
        name: "query, deterministic, Completed at sequence 4",
      }),
    ).toBeInTheDocument();
  });

  it.each([
    { hash: "#/overview", heading: "2 runs need attention." },
    { hash: "#/workflows", heading: "Workflows" },
    { hash: "#/runs", heading: "Runs" },
  ])("renders the $hash shell route from daemon fixtures", async ({ hash, heading }) => {
    window.location.hash = hash;
    renderLiveApp();

    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Primary" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Skip to main content" })).toHaveAttribute(
      "href",
      "#main-content",
    );
  });

  it("persists independently selected themes", async () => {
    window.localStorage.setItem("goobers-theme", "dark");
    const user = userEvent.setup();
    renderLiveApp();

    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    await user.click(screen.getByRole("button", { name: "Use light theme" }));

    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(window.localStorage.getItem("goobers-theme")).toBe("light");
  });

  it("operates primary navigation from the keyboard and moves focus to the route content", async () => {
    const user = userEvent.setup();
    renderLiveApp();
    const workflowsButton = screen.getByRole("button", { name: "Workflows" });

    workflowsButton.focus();
    await user.keyboard("{Enter}");

    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("main")).toHaveFocus());
    expect(screen.getByRole("button", { name: "Workflows" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("skips to main content without changing the active hash route", async () => {
    window.location.hash = "#/workflows";
    const user = userEvent.setup();
    renderLiveApp();

    await user.click(screen.getByRole("link", { name: "Skip to main content" }));

    expect(window.location.hash).toBe("#/workflows");
    expect(screen.getByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(screen.getByRole("main")).toHaveFocus();
  });

  it("supports directional graph selection and exposes the scroll-safe responsive contract", async () => {
    window.location.hash = "#/workflow/core/implementation";
    renderLiveApp();
    const firstStage = await screen.findByRole("button", { name: /^query,/ });
    const secondStage = screen.getByRole("button", { name: /^implement,/ });

    firstStage.focus();
    fireEvent.keyDown(firstStage, { key: "ArrowRight" });

    expect(secondStage).toHaveFocus();
    expect(secondStage).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("group", { name: /implementation execution graph/i })).toHaveAttribute(
      "data-responsive-layout",
      "scroll-under-820",
    );
  });

  it("filters live run history through server-side phase requests", async () => {
    window.location.hash = "#/runs";
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(
      await screen.findByRole("link", { name: "Open run 01JZ441DAEMONAPI" }),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "attention" }));

    expect(
      await screen.findByRole("link", { name: "Open run 01JZ402DASHBOARD" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open run 01JZ400FAILED" })).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Open run 01JZ441DAEMONAPI" }),
    ).not.toBeInTheDocument();

    // The attention chip fans out to server-side failed + escalated phase
    // filters rather than fetching the whole journal and filtering in the client.
    expect(listRuns).toHaveBeenCalledWith(
      expect.objectContaining({ phase: "escalated" }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(listRuns).toHaveBeenCalledWith(
      expect.objectContaining({ phase: "failed" }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  function renderLiveApp() {
    return render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);
  }
});
