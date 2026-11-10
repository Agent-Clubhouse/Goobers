import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { FluidUIRequest } from "../api/types";
import { routeHash } from "../routing";
import { emptyDaemonFixtures } from "../test/daemonFixtures";

const at = "2026-09-08T00:00:00Z";

function request(overrides: Partial<FluidUIRequest> & Pick<FluidUIRequest, "id" | "status">): FluidUIRequest {
  return { prompt: `prompt for ${overrides.id}`, attempt: 1, createdAt: at, updatedAt: at, ...overrides };
}

const frame = () => document.querySelector(".portal-frame");

async function openFeedback(requests: FluidUIRequest[]) {
  const client = new FixtureDaemonClient({ ...emptyDaemonFixtures(), fluidUIRequests: requests });
  const user = userEvent.setup();
  render(<App client={client} />);
  await user.click(await screen.findByRole("tab", { name: "Feedback" }));
  await screen.findByRole("heading", { name: "Your requests" });
  return { client, user };
}

function item(prompt: string) {
  const entry = screen.getByText(prompt).closest("li");
  if (!entry) throw new Error(`no request item for ${prompt}`);
  return within(entry);
}

beforeEach(() => {
  window.location.hash = "#/labs/fluid-ui";
});

describe("Labs Fluid UI feedback", () => {
  it("says the UX is ready only for a ready_to_preview candidate, and links each run", async () => {
    await openFeedback([
      request({ id: "req-queued", status: "queued" }),
      request({ id: "req-running", status: "implementing", runId: "run-1" }),
      request({
        id: "req-ready",
        status: "ready_to_preview",
        runId: "run-2",
        candidate: { version: "portal.customization/v1", density: "compact" },
      }),
    ]);

    expect(screen.getAllByText("Your UX is ready.")).toHaveLength(1);
    expect(item("prompt for req-ready").getByText("Your UX is ready.")).toBeInTheDocument();
    expect(item("prompt for req-queued").getByText("Queued")).toBeInTheDocument();
    expect(item("prompt for req-running").getByText("Implementing")).toBeInTheDocument();
    expect(item("prompt for req-running").getByRole("link", { name: "View run" })).toHaveAttribute(
      "href",
      routeHash({ page: "run", id: "run-1" }),
    );
  });

  it("previews without saving, then applies through the request and reverts", async () => {
    const { client, user } = await openFeedback([
      request({
        id: "req-ready",
        status: "ready_to_preview",
        candidate: { version: "portal.customization/v1", density: "compact" },
      }),
    ]);
    const put = vi.spyOn(client, "putLabsFluidUICustomization");
    const apply = vi.spyOn(client, "applyLabsFluidUIRequest");
    const ready = item("prompt for req-ready");

    await user.click(ready.getByRole("button", { name: "Preview" }));
    expect(frame()).toHaveAttribute("data-density", "compact");
    expect(apply).not.toHaveBeenCalled();

    await user.click(ready.getByRole("button", { name: "Apply" }));
    expect(apply).toHaveBeenCalledWith("req-ready");
    expect(put).not.toHaveBeenCalled();
    expect(await ready.findByText("Applied")).toBeInTheDocument();
    expect(ready.queryByText("Your UX is ready.")).not.toBeInTheDocument();
    expect((await client.getLabsFluidUICustomization()).customization).toEqual({
      version: "portal.customization/v1",
      density: "compact",
    });

    await user.click(ready.getByRole("button", { name: "Revert" }));
    expect(await ready.findByText("Reverted")).toBeInTheDocument();
    expect((await client.getLabsFluidUICustomization()).customization).toBeNull();
  });

  it("shows failures with their errors and retries them", async () => {
    const { client, user } = await openFeedback([
      request({
        id: "req-failed",
        status: "failed",
        attempt: 1,
        detail: "candidate failed validation after one retry",
        errors: ["density: must be comfortable or compact"],
      }),
      request({ id: "req-unsupported", status: "unsupported", unmetNeed: "cost-charts" }),
    ]);
    const retry = vi.spyOn(client, "retryLabsFluidUIRequest");
    const failed = item("prompt for req-failed");

    expect(failed.getByRole("alert")).toHaveTextContent("candidate failed validation after one retry");
    expect(failed.getByText("density: must be comfortable or compact")).toBeInTheDocument();
    expect(item("prompt for req-unsupported").getByText("cost-charts")).toBeInTheDocument();

    await user.click(failed.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledWith("req-failed");
    expect(await failed.findByText("Queued")).toBeInTheDocument();
    expect(failed.getByText(/attempt 2/)).toBeInTheDocument();
  });

  it("submits a request that starts queued", async () => {
    const { client, user } = await openFeedback([]);
    const submit = vi.spyOn(client, "submitLabsFluidUIRequest");

    expect(screen.getByText("You have not made any requests yet.")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Request"), "  show failing runs first ");
    await user.click(screen.getByRole("button", { name: "Submit request" }));

    expect(submit).toHaveBeenCalledWith("show failing runs first");
    expect(await item("show failing runs first").findByText("Queued")).toBeInTheDocument();
    expect(screen.queryByText("Your UX is ready.")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Request")).toHaveValue("");
  });
});
