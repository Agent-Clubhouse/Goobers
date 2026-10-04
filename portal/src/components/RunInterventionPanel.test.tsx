import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "../api/httpClient";
import type { InteractiveRunView } from "../api/types";
import { RunInterventionPanel } from "./RunInterventionPanel";

function view(): InteractiveRunView {
  return { runId: "run-1", gaggle: "web", phase: "escalated", actions: [{ kind: "guidance", stage: "review", subjectSequence: 8, decisions: [], available: true, reason: "" }], guidance: [], restartReason: "Fresh stage restart is unavailable." };
}
function json(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } }); }

describe("shared human operations", () => {
  it("saves guidance through the production HTTP client and shows attributed shared history", async () => {
    const state = view();
    const requests: RequestInit[] = [];
    const fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method !== "POST") return json(state);
      requests.push(init);
      const input = JSON.parse(init.body as string);
      state.guidance.push({ request: { schema: "goobers.dev/operator-message/request/v1", requestId: "note-1", idempotencyKey: "server-key", targetAddress: "stage:review@8", principalRef: "https://id.example:alice", requestedAt: "2026-10-04T10:00:00Z", purpose: "stage-restart-guidance", content: { text: input.guidance }, deliveryMode: "shared-guidance" }, state: "accepted" });
      return json({ status: "saved", accepted: true, runId: "run-1", phase: "escalated", journalSequence: 0, guidance: state.guidance[0] });
    });
    render(<RunInterventionPanel client={new HttpDaemonClient({ fetch })} runId="run-1" />);
    fireEvent.change(await screen.findByLabelText("Guidance"), { target: { value: "Check the failing integration test." } });
    fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
    expect(await screen.findByText("Guidance saved. It has not been delivered to an agent.")).toBeInTheDocument();
    expect(await screen.findByText("Check the failing integration test.")).toBeInTheDocument();
    expect(screen.getByText(/Saved by https:\/\/id.example:alice/)).toBeInTheDocument();
    expect(JSON.parse(requests[0].body as string)).toEqual({ kind: "guidance", stage: "review", expectedSubjectSequence: 8, guidance: "Check the failing integration test." });
    expect((requests[0].headers as Record<string, string>)["Idempotency-Key"]).toBeTruthy();
    expect(requests).toHaveLength(1);
  });
  it("retains exact payload and key across an uncertain response without automatic mutation retries", async () => {
    const requests: RequestInit[] = [];
    const fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method !== "POST") return json(view());
      requests.push(init);
      if (requests.length === 1) throw new TypeError("connection lost");
      return json({ status: "saved", accepted: true, runId: "run-1", phase: "escalated", journalSequence: 0 });
    });
    render(<RunInterventionPanel client={new HttpDaemonClient({ fetch })} runId="run-1" />);
    fireEvent.change(await screen.findByLabelText("Guidance"), { target: { value: "Preserve this draft." } });
    fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
    const retry = await screen.findByRole("button", { name: "Check same request" });
    expect(requests).toHaveLength(1);
    expect(screen.getByLabelText("Guidance")).toBeDisabled();
    fireEvent.click(retry);
    await screen.findByText("Guidance saved. It has not been delivered to an agent.");
    expect(requests[0].body).toEqual(requests[1].body);
    expect(requests[0].headers).toEqual(requests[1].headers);
  });
  it("preserves the draft on stale refusal and presents read-only access when permission is absent", async () => {
    let deny = false;
    const fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      if (deny) return json({ error: { code: "interactive_access_denied", message: "denied" } }, 403);
      if (init?.method === "POST") return json({ error: { code: "interactive_subject_changed", message: "refresh" } }, 409);
      return json(view());
    });
    const client = new HttpDaemonClient({ fetch });
    const rendered = render(<RunInterventionPanel client={client} runId="run-1" revision={1} />);
    fireEvent.change(await screen.findByLabelText("Guidance"), { target: { value: "Keep my draft." } });
    fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
    await screen.findByText(/The stage changed/);
    expect(screen.getByLabelText("Guidance")).toHaveValue("Keep my draft.");
    deny = true;
    rendered.rerender(<RunInterventionPanel client={client} runId="run-1" revision={2} />);
    await waitFor(() => expect(screen.queryByRole("button", { name: "Save guidance" })).not.toBeInTheDocument());
    expect(screen.getByText(/Human operations require sign-in/)).toBeInTheDocument();
  });
});
