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
        relatedPullRequests: [],
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

  it("shows the confirmed action timeline and provider link", async () => {
    render(
      <WorkItemsPage
        client={client()}
        navigate={vi.fn()}
        route={{ page: "work-items", provider: "github", repository: "acme/app", kind: "pr", id: "42" }}
        standalone={false}
      />,
    );

    await waitFor(() => expect(screen.getByRole("heading", { name: "acme/app#42" })).toBeInTheDocument());
    expect(screen.getByText("$1.25")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open pull request" })).toHaveAttribute(
      "href",
      "https://github.com/acme/app/pull/42",
    );
    expect(screen.getByRole("link", { name: "View run" })).toHaveAttribute("href", "#/run/run-2");
  });
});
