import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { DaemonApiError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { WorkItemsPage } from "./WorkItemsPage";

function client() {
  return new FixtureDaemonClient({
    ...populatedDaemonFixtures(),
    workItems: {
      hasMore: false,
      items: [{
        provider: "github",
        repository: "acme/app",
        kind: "pr",
        externalId: "42",
        url: "https://github.com/acme/app/pull/42",
        outcome: "done",
        actionCount: 2,
        lastOperation: "comment",
        lastActionAt: "2026-09-01T12:00:00Z",
        lastRunId: "run-2",
        gaggle: "core",
        workflow: "merge-review",
        runStatus: "completed",
      }, {
        provider: "github",
        repository: "acme/service",
        kind: "issue",
        externalId: "77",
        url: "https://github.com/acme/service/issues/77",
        outcome: "in-progress",
        actionCount: 1,
        lastOperation: "comment",
        lastActionAt: "2026-09-01T11:00:00Z",
        lastRunId: "run-3",
        gaggle: "tools",
        workflow: "triage",
        runStatus: "completed",
      }],
    },
    workItemDetails: {
      "github/acme/app/pr/42": {
        provider: "github",
        repository: "acme/app",
        kind: "pr",
        externalId: "42",
        url: "https://github.com/acme/app/pull/42",
        outcome: "done",
        cost: {
          nanoAIU: 1_250_000_000,
          totalRuns: 1,
          measuredRuns: 1,
          totalAttempts: 1,
          measuredAttempts: 1,
          lowerBound: false,
        },
        relatedPullRequests: [{
          provider: "github",
          repository: "acme/app",
          kind: "pr",
          externalId: "43",
          url: "https://github.com/acme/app/pull/43",
        }],
        truncated: false,
        actions: [{
          runId: "run-2",
          sequence: 9,
          operation: "merge",
          occurredAt: "2026-09-01T12:00:00Z",
          gaggle: "core",
          workflow: "merge-review",
          runStatus: "completed",
        }, {
          runId: "run-1",
          sequence: 4,
          operation: "comment",
          occurredAt: "2026-09-01T11:00:00Z",
          gaggle: "core",
          workflow: "implementation",
          runStatus: "completed",
        }],
      },
    },
  });
}

describe("WorkItemsPage", () => {
  it("lists grouped work items and navigates to their action history", async () => {
    const navigate = vi.fn();
    render(
      <WorkItemsPage
        client={client()}
        navigate={navigate}
        route={{ page: "work-items" }}
        standalone={false}
      />,
    );

    await screen.findByText("acme/app#42");
    expect(screen.getAllByText("Done", { selector: ".status-badge" })).toHaveLength(2);
    const row = screen.getByRole("button", { name: /Open PR #42 in acme\/app/i });
    expect(row.querySelector(".data-table-primary")).toHaveAttribute("title", "acme/app#42");
    expect(row).toHaveTextContent(/Last action:\s*Comment/);
    expect(row).toHaveClass("work-item-row-done");
    expect(row.querySelector(".work-item-status")).toHaveTextContent("Done");
    expect(row.querySelector(".work-item-mobile-context")).toHaveTextContent(
      "core / merge-review",
    );
    await userEvent.click(row);
    expect(navigate).toHaveBeenCalledWith({
      page: "work-items",
      provider: "github",
      repository: "acme/app",
      kind: "pr",
      id: "42",
    });
  });

  it("filters instantly by gaggle and work-item identity", async () => {
    render(
      <WorkItemsPage
        client={client()}
        navigate={vi.fn()}
        route={{ page: "work-items", gaggle: "core", query: "app#42" }}
        standalone={false}
      />,
    );

    await screen.findByRole("button", { name: /Open PR #42 in acme\/app/i });
    expect(screen.queryByRole("button", { name: /Open issue #77 in acme\/service/i }))
      .not.toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Filter work items by gaggle" }))
      .toHaveValue("core");
    expect(screen.getByRole("searchbox", { name: "Search work items" }))
      .toHaveValue("app#42");
  });

  it("filters work items by outcome", async () => {
    render(
      <WorkItemsPage
        client={client()}
        navigate={vi.fn()}
        route={{ page: "work-items" }}
        standalone={false}
      />,
    );

    await screen.findByRole("button", { name: /Open PR #42 in acme\/app/i });
    await userEvent.selectOptions(
      screen.getByRole("combobox", { name: "Filter work items by status" }),
      "in-progress",
    );

    expect(screen.queryByRole("button", { name: /Open PR #42 in acme\/app/i }))
      .not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Open issue #77 in acme\/service/i }))
      .toBeInTheDocument();
  });

  it("derives draft gaggle choices and validation from the draft work-item type", async () => {
    const navigate = vi.fn();
    const user = userEvent.setup();
    render(
      <WorkItemsPage
        client={client()}
        navigate={navigate}
        route={{ page: "work-items", kind: "pr", gaggle: "core" }}
        standalone={false}
      />,
    );

    await user.click(await screen.findByRole("button", { name: "Filters" }));
    const dialog = screen.getByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "issues" }));

    const gaggle = within(dialog).getByRole("combobox", {
      name: "Draft work item gaggle filter",
    });
    expect(gaggle).toHaveValue("");
    expect(within(gaggle).queryByRole("option", { name: "core" })).not.toBeInTheDocument();
    expect(within(gaggle).getByRole("option", { name: "tools" })).toBeInTheDocument();

    await user.click(within(dialog).getByRole("button", { name: "Apply filters" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent(
      'Invalid gaggle filter "core"',
    );
    expect(navigate).not.toHaveBeenCalled();

    await user.selectOptions(gaggle, "tools");
    await user.click(within(dialog).getByRole("button", { name: "Apply filters" }));
    expect(navigate).toHaveBeenCalledWith({
      page: "work-items",
      kind: "issue",
      gaggle: "tools",
      query: undefined,
    });
  });

  it("keeps search focus while word-wheel filtering and replaces the current URL", async () => {
    const navigate = vi.fn();
    window.history.replaceState(null, "", "#/work-items");
    render(
      <WorkItemsPage
        client={client()}
        navigate={navigate}
        route={{ page: "work-items" }}
        standalone={false}
      />,
    );

    const search = await screen.findByRole("searchbox", { name: "Search work items" });
    await userEvent.type(search, "app#42");

    expect(search).toHaveFocus();
    expect(search).toHaveValue("app#42");
    expect(window.location.hash).toBe("#/work-items?q=app%2342");
    expect(navigate).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: /Open PR #42 in acme\/app/i }))
      .toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Open issue #77 in acme\/service/i }))
      .not.toBeInTheDocument();
  });

  it("shows the confirmed action table and provider link", async () => {
    const navigate = vi.fn();
    render(
      <WorkItemsPage
        client={client()}
        navigate={navigate}
        route={{ page: "work-items", provider: "github", repository: "acme/app", kind: "pr", id: "42" }}
        standalone={false}
      />,
    );

    await waitFor(() => expect(screen.getByRole("heading", { name: "acme/app#42" })).toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: "Work Items" }));
    expect(navigate).toHaveBeenCalledWith({ page: "work-items", kind: "pr" });
    expect(screen.getByText("1.25 AIC")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open pull request" })).toHaveAttribute(
      "href",
      "https://github.com/acme/app/pull/42",
    );
    const relatedPullRequest = screen.getByRole("link", {
      name: "Open related PR acme/app#43",
    });
    expect(relatedPullRequest).toHaveAttribute("href", "https://github.com/acme/app/pull/43");
    expect(relatedPullRequest.closest(".page-heading-actions")).not.toBeNull();
    expect(screen.queryByRole("heading", { name: "Related pull requests" })).not.toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Action history for acme/app#42" })).toBeInTheDocument();
    expect(screen.getAllByRole("columnheader").map((header) => header.textContent)).toEqual([
      "Action",
      "Gaggle / workflow",
      "Status",
      "Time",
      "Run",
    ]);
    expect(screen.getAllByRole("link", { name: "View run" })[0]).toHaveAttribute("href", "#/run/run-2");

    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Filter actions by type" }), "comment");
    expect(screen.queryByRole("cell", { name: /^Merge/ })).not.toBeInTheDocument();
    expect(screen.getByRole("cell", { name: /^Comment/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View run" })).toHaveAttribute("href", "#/run/run-1");
  });

  it("keeps equal numeric ids separate across providers, repositories, and projects", async () => {
    const summary = {
      outcome: "in-progress" as const,
      actionCount: 1,
      lastOperation: "create",
      lastActionAt: "2026-09-01T12:00:00Z",
      lastRunId: "run-1",
    };
    const navigate = vi.fn();
    render(
      <WorkItemsPage
        client={new FixtureDaemonClient({
          ...populatedDaemonFixtures(),
          workItems: {
            hasMore: false,
            items: [
              { ...summary, provider: "github", repository: "acme/app", kind: "issue", externalId: "7" },
              { ...summary, provider: "github", repository: "acme/web", kind: "issue", externalId: "7" },
              { ...summary, provider: "ado", repository: "contoso/alpha", kind: "issue", externalId: "7" },
              { ...summary, provider: "ado", repository: "contoso/beta", kind: "issue", externalId: "7" },
              { ...summary, provider: "ado", repository: "contoso/alpha/web", kind: "pr", externalId: "7" },
              { ...summary, provider: "ado", kind: "issue", externalId: "7", url: "https://ado.example/a" },
              { ...summary, provider: "ado", kind: "issue", externalId: "7", url: "https://ado.example/b" },
            ],
          },
        })}
        navigate={navigate}
        route={{ page: "work-items" }}
        standalone={false}
      />,
    );

    await screen.findByRole("button", { name: "Open issue #7 in acme/app" });
    expect(screen.getByRole("button", { name: "Open issue #7 in acme/web" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open work item #7 in contoso/alpha" }))
      .toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open work item #7 in contoso/beta" }))
      .toBeInTheDocument();
    expect(screen.getAllByText("ado · work item")).toHaveLength(2);

    const unknown = screen.getAllByRole("button", { name: "work item #7 has no recorded repository" });
    expect(unknown).toHaveLength(2);
    unknown.forEach((row) => expect(row).toBeDisabled());
    expect(screen.getAllByText("#7")).toHaveLength(2);

    await userEvent.click(screen.getByRole("button", { name: "Open PR #7 in contoso/alpha/web" }));
    expect(navigate).toHaveBeenCalledWith({
      page: "work-items",
      provider: "ado",
      repository: "contoso/alpha/web",
      kind: "pr",
      id: "7",
    });
  });

  it("explains that unrecorded external work may still exist when the page is empty", async () => {
    render(
      <WorkItemsPage
        client={new FixtureDaemonClient({
          ...populatedDaemonFixtures(),
          workItems: { hasMore: false, items: [] },
        })}
        navigate={vi.fn()}
        route={{ page: "work-items" }}
        standalone={false}
      />,
    );

    expect(await screen.findByText(/No confirmed provider actions match this filter/))
      .toHaveTextContent("may still exist in the provider");
  });

  it("gives upgrade guidance when an older daemon lacks work item history", async () => {
    const oldClient = client();
    vi.spyOn(oldClient, "listWorkItems").mockRejectedValue(
      new DaemonApiError(404, "not_found", "route not found"),
    );
    render(
      <WorkItemsPage
        client={oldClient}
        navigate={vi.fn()}
        route={{ page: "work-items" }}
        standalone={false}
      />,
    );

    expect(await screen.findByRole("heading", { name: "Work Items unavailable" }))
      .toBeInTheDocument();
    expect(screen.getByText(/Upgrade Goobers/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("labels an Azure Boards detail page as work-item activity", async () => {
    render(
      <WorkItemsPage
        client={new FixtureDaemonClient({
          ...populatedDaemonFixtures(),
          workItemDetails: {
            "ado/contoso/alpha/issue/7": {
              provider: "ado",
              repository: "contoso/alpha",
              kind: "issue",
              externalId: "7",
              url: "https://dev.azure.com/contoso/alpha/_workitems/edit/7",
              outcome: "in-progress",
              relatedPullRequests: [],
              actions: [],
              truncated: false,
            },
          },
        })}
        navigate={vi.fn()}
        route={{ page: "work-items", provider: "ado", repository: "contoso/alpha", kind: "issue", id: "7" }}
        standalone={false}
      />,
    );

    expect(await screen.findByText("ado work item activity")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open work item" })).toHaveAttribute(
      "href",
      "https://dev.azure.com/contoso/alpha/_workitems/edit/7",
    );
  });
});
