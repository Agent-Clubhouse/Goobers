import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

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
  window.location.hash = "#/workflow/core/implementation";
  delete document.documentElement.dataset.theme;
});

describe("workflow detail page", () => {
  it("renders live definition metadata, the canonical graph, stage context, and filtered runs", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(await screen.findByRole("heading", { name: "Implementation" })).toBeInTheDocument();
    expect(screen.getByText("v7 · sha256:core")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Workflow configuration summary" })).toHaveTextContent(
      "core/implementer",
    );
    expect(screen.getByRole("group", { name: "implementation execution graph" })).toBeInTheDocument();
    expect(screen.getByRole("complementary", { name: "query definition" })).toHaveTextContent(
      "Claim the next approved backlog item.",
    );

    await user.click(screen.getByRole("button", { name: /^implement,/ }));
    expect(screen.getByRole("complementary", { name: "implement definition" })).toHaveTextContent(
      "repo:push",
    );

    const history = screen.getByRole("region", { name: "Implementation recent runs" });
    expect(within(history).getAllByRole("link")).toHaveLength(4);
    expect(
      within(history).queryByRole("link", { name: "Open run 01JZ300ABORTED" }),
    ).not.toBeInTheDocument();
    expect(listRuns).toHaveBeenCalledWith(
      { gaggle: "core", workflow: "implementation", limit: 20 },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  it("surfaces stage timeout/retry in fields and the full config in raw YAML (#2185)", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await user.click(await screen.findByRole("button", { name: /^implement,/ }));
    const panel = screen.getByRole("complementary", { name: "implement definition" });

    expect(panel).toHaveTextContent("3600s");
    expect(panel).toHaveTextContent("2 attempts, 30s backoff");
    expect(panel).toHaveTextContent("pr:open");
    expect(within(panel).queryByText(/goober: implementer/)).not.toBeInTheDocument();

    await user.click(within(panel).getByRole("tab", { name: "Raw YAML" }));
    expect(within(panel).getByText(/goober: implementer/)).toBeInTheDocument();
    expect(panel).toHaveTextContent("timeoutSeconds: 3600");
  });

  it("navigates from recent history to run detail", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await user.click(await screen.findByRole("link", { name: "Open run 01JZ402DASHBOARD" }));

    await waitFor(() => expect(window.location.hash).toBe("#/run/01JZ402DASHBOARD"));
    expect(
      await screen.findByRole("heading", { name: "Run 01JZ402DASHBOARD" }),
    ).toBeInTheDocument();
  });

  it("navigates back to the owning gaggle topology", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const breadcrumbs = await screen.findByRole("navigation", { name: "Breadcrumb" });
    await user.click(within(breadcrumbs).getByRole("button", { name: "core" }));

    await waitFor(() => expect(window.location.hash).toBe("#/gaggle/core"));
    expect(
      await screen.findByRole("heading", { name: "Workflow topology" }),
    ).toBeInTheDocument();
  });

  it("pivots the workflow and its owning gaggle into pre-scoped Runs/Insight views (#2529)", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await screen.findByRole("heading", { name: "Implementation" });
    const breadcrumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(
      within(breadcrumbs).getByRole("link", { name: "View core in Runs" }),
    ).toHaveAttribute("href", "#/runs?gaggle=core");
    // The gaggle breadcrumb button's own name ("core") stays unique even with
    // the adjacent pivot links present.
    expect(within(breadcrumbs).getByRole("button", { name: "core" })).toBeInTheDocument();

    await user.click(
      screen.getByRole("link", { name: "View core / Implementation in Insight" }),
    );

    expect(await screen.findByRole("heading", { name: "Insight" })).toBeInTheDocument();
    expect(screen.getByLabelText("Insight scope")).toHaveTextContent("core / implementation");
  });

  it("keeps the graph available across dark and light themes", async () => {
    window.localStorage.setItem("goobers-theme", "dark");
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const graph = await screen.findByRole("group", {
      name: "implementation execution graph",
    });
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    expect(graph).toHaveAttribute("data-responsive-layout", "scroll-under-820");

    await user.click(screen.getByRole("button", { name: "Use light theme" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(graph).toBeVisible();
  });

  it("shows an explicit live-data error instead of substituting prototype content", async () => {
    window.location.hash = "#/workflow/missing/implementation";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(
      await screen.findByRole("heading", { name: "Workflow unavailable" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Fixture workflow not found.")).toBeInTheDocument();
    expect(screen.queryByText("Gather context")).not.toBeInTheDocument();
  });
});
