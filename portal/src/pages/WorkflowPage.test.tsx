import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { fixtureKey, FixtureDaemonClient } from "../api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  RequestOptions,
} from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

const storedValues = new Map<string, string>();

class PushableClient extends FixtureDaemonClient {
  private readers: ((result: IteratorResult<DaemonUpdateEvent>) => void)[] = [];
  private queued: DaemonUpdateEvent[] = [];

  connectEvents(
    _request?: EventStreamRequest,
    _options?: RequestOptions,
  ): Promise<DaemonEventStream> {
    const self = this;
    return Promise.resolve({
      close: () => {},
      [Symbol.asyncIterator]() {
        return {
          next(): Promise<IteratorResult<DaemonUpdateEvent>> {
            const queued = self.queued.shift();
            if (queued) {
              return Promise.resolve({ done: false, value: queued });
            }
            return new Promise((resolve) => self.readers.push(resolve));
          },
        };
      },
    });
  }

  push(event: DaemonUpdateEvent): void {
    const reader = this.readers.shift();
    if (reader) {
      reader({ done: false, value: event });
    } else {
      this.queued.push(event);
    }
  }
}

function workflowEvent(
  id: string,
  gaggle: string,
  workflow: string,
  models: ("run" | "workflow")[] = ["run", "workflow"],
): DaemonUpdateEvent {
  return {
    id,
    type: "invalidate",
    data: {
      cursor: id,
      models,
      runIds: [],
      workflows: [{ gaggle, name: workflow }],
    },
  };
}

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
    const getTelemetryStats = vi.spyOn(client, "getTelemetryStats");
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
    await waitFor(() =>
      expect(getTelemetryStats).toHaveBeenCalledWith(
        expect.objectContaining({ gaggle: "core", workflow: "implementation" }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
  });

  it("refreshes workflow metadata and recent runs after a scoped live invalidation", async () => {
    const fixtures = populatedDaemonFixtures();
    const client = new PushableClient(fixtures);
    render(<App client={client} />);

    expect(await screen.findByText("v7 · sha256:core")).toBeInTheDocument();
    const workflow = fixtures.workflowDetails?.[fixtureKey("core", "implementation")];
    if (!workflow) {
      throw new Error("Expected the core workflow fixture.");
    }
    workflow.definition = { version: 8, digest: "sha256:refreshed" };
    workflow.graph.version = 8;
    workflow.graph.digest = "sha256:refreshed";
    fixtures.runs.runs.push({
      ...fixtures.runs.runs[0],
      id: "01JZLIVEWORKFLOW",
      startedAt: "2026-07-18T08:00:00Z",
      finishedAt: "2026-07-18T08:01:00Z",
      lastActivityAt: "2026-07-18T08:01:00Z",
    });

    act(() => client.push(workflowEvent("session:workflow-1", "core", "implementation")));

    expect(await screen.findByText("v8 · sha256:refreshed")).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "Open run 01JZLIVEWORKFLOW" }),
    ).toBeInTheDocument();
  });

  it("retains stale detail after a failed refresh and replaces it on retry", async () => {
    const fixtures = populatedDaemonFixtures();
    const client = new PushableClient(fixtures);
    const getWorkflow = vi.spyOn(client, "getWorkflow");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(await screen.findByText("v7 · sha256:core")).toBeInTheDocument();
    const workflow = fixtures.workflowDetails?.[fixtureKey("core", "implementation")];
    if (!workflow) {
      throw new Error("Expected the core workflow fixture.");
    }
    workflow.definition = { version: 8, digest: "sha256:recovered" };
    workflow.graph.version = 8;
    workflow.graph.digest = "sha256:recovered";
    getWorkflow.mockRejectedValueOnce(new Error("Refresh failed."));

    act(() => client.push(workflowEvent("session:workflow-2", "core", "implementation")));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Workflow detail may be stale");
    expect(alert).toHaveTextContent("Refresh failed.");
    expect(screen.getByText("v7 · sha256:core")).toBeInTheDocument();

    await user.click(within(alert).getByRole("button", { name: "Try again" }));

    expect(await screen.findByText("v8 · sha256:recovered")).toBeInTheDocument();
    expect(screen.queryByText("Workflow detail may be stale")).not.toBeInTheDocument();
  });

  it("scopes live refreshes to the workflow and aborts an in-flight refresh on workflow change", async () => {
    const client = new PushableClient(populatedDaemonFixtures());
    const getWorkflow = vi.spyOn(client, "getWorkflow");
    render(<App client={client} />);

    expect(await screen.findByText("v7 · sha256:core")).toBeInTheDocument();
    getWorkflow.mockClear();

    await act(async () => {
      client.push(workflowEvent("session:unrelated", "tools", "implementation"));
      await new Promise((resolve) => setTimeout(resolve, 75));
    });
    expect(getWorkflow).not.toHaveBeenCalled();

    let refreshSignal: AbortSignal | undefined;
    getWorkflow.mockImplementationOnce((_gaggle, _workflow, options) => {
      refreshSignal = options?.signal;
      return new Promise((_resolve, reject) => {
        options?.signal?.addEventListener("abort", () => reject(new Error("aborted")), {
          once: true,
        });
      });
    });
    act(() => client.push(workflowEvent("session:matching", "core", "implementation")));
    await waitFor(() => expect(refreshSignal).toBeInstanceOf(AbortSignal));
    expect(refreshSignal?.aborted).toBe(false);

    act(() => {
      window.location.hash = "#/workflow/tools/implementation";
    });

    await waitFor(() => expect(refreshSignal?.aborted).toBe(true));
    expect(await screen.findByText("v7 · sha256:tools")).toBeInTheDocument();
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
