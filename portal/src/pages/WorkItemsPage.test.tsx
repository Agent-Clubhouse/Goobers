import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
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
        actionCount: 2,
        lastOperation: "merge",
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
        cost: {
          costUSD: 1.25,
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
    expect(screen.getByText("Merge")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /Open PR #42 in acme\/app/i }));
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

  it("shows the confirmed action timeline and provider link", async () => {
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
    expect(screen.getByText("$1.25")).toBeInTheDocument();
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
    expect(screen.getByRole("link", { name: "View run" })).toHaveAttribute("href", "#/run/run-2");
  });
});
