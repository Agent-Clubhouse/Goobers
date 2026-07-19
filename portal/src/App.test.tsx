import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { FixtureDaemonClient } from "./api/fixtureClient";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

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
    expect(screen.getByText("Daemon connected")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Needs attention" })).toBeInTheDocument();
  });

  it("labels standalone read-only mode in the portal chrome", async () => {
    const mode = document.createElement("meta");
    mode.name = "goobers-dashboard-mode";
    mode.content = "standalone";
    document.head.append(mode);

    renderLiveApp();

    expect(await screen.findByText("Standalone read-only")).toBeInTheDocument();
    expect(screen.getByText("Daemon not running; reading this instance locally")).toBeInTheDocument();
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

  it("supports directional graph selection and exposes the compact responsive contract", () => {
    window.location.hash = "#/workflow/implementation";
    render(<App />);
    const firstStage = screen.getByRole("button", { name: "Gather context, pending" });
    const secondStage = screen.getByRole("button", { name: "Implement, pending" });

    firstStage.focus();
    fireEvent.keyDown(firstStage, { key: "ArrowRight" });

    expect(secondStage).toHaveFocus();
    expect(secondStage).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("group", { name: /Implementation execution graph/ })).toHaveAttribute(
      "data-responsive-layout",
      "compact-under-820",
    );
  });

  it("gives run filters observable behavior", async () => {
    window.location.hash = "#/runs";
    const user = userEvent.setup();
    render(<App />);

    await user.click(screen.getByRole("button", { name: "attention" }));

    expect(screen.getByRole("button", { name: /Open run Live visual dashboard/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Open run Daemon read API/i })).not.toBeInTheDocument();
  });

  function renderLiveApp() {
    return render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);
  }
});
