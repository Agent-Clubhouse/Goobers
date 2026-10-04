import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { HttpDaemonClient } from "../api/httpClient";
import type { InteractiveRunView, InteractiveRunCommandResult, DaemonClient } from "../api/types";
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

it("restarts with explicitly selected saved guidance and links the distinct execution", async () => {
  const state = view();
  state.actions = [{ kind: "restart", stage: "implement", subjectSequence: 8, decisions: [], available: true, reason: "" }];
  state.guidance = [{ request: { schema: "goobers.dev/operator-message/request/v1", requestId: "chosen-note", idempotencyKey: "note-key", targetAddress: "stage:implement@8", principalRef: "https://id.test:alice", requestedAt: "2026-10-04T10:00:00Z", purpose: "stage-restart-guidance", content: { text: "Keep the established boundary." }, deliveryMode: "shared-guidance" }, state: "accepted" }];
  const requests: RequestInit[] = [];
  const fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method !== "POST") return json(state);
    requests.push(init);
    return json({ status: "started", accepted: true, runId: "run-1", continuationRunId: "human-restart-1", phase: "escalated", journalSequence: 8 });
  });
  render(<RunInterventionPanel client={new HttpDaemonClient({ fetch })} runId="run-1" />);
  expect(await screen.findByRole("button", { name: "Restart stage" })).toBeDisabled();
  fireEvent.click(screen.getByRole("checkbox", { name: /Keep the established boundary/ }));
  fireEvent.change(screen.getByLabelText("Rationale"), { target: { value: "Retry after review." } });
  fireEvent.click(screen.getByRole("button", { name: "Restart stage" }));
  expect(await screen.findByRole("link", { name: "Open restarted execution" })).toHaveAttribute("href", "#/run/human-restart-1");
  expect(JSON.parse(requests[0].body as string)).toEqual({ kind: "restart", stage: "implement", expectedSubjectSequence: 8, guidanceIds: ["chosen-note"], rationale: "Retry after review." });
  expect(requests).toHaveLength(1);
});

it("clears a previous client/run synchronously and ignores its late command completion", async () => {
  const first: DaemonClient = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(first, "getInteractiveRun").mockResolvedValue(view());
  let resolveOld!: (value: InteractiveRunCommandResult) => void;
  vi.spyOn(first, "commandInteractiveRun").mockReturnValue(new Promise((resolve) => { resolveOld = resolve; }));
  const { rerender } = render(<RunInterventionPanel client={first} runId="run-1" />);
  fireEvent.change(await screen.findByLabelText("Guidance"), { target: { value: "Private old draft" } });
  fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
  const second: DaemonClient = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(second, "getInteractiveRun").mockResolvedValue({ ...view(), runId: "run-2" });
  vi.spyOn(second, "commandInteractiveRun");
  rerender(<RunInterventionPanel client={second} runId="run-2" />);
  expect(screen.queryByLabelText("Guidance")).not.toBeInTheDocument();
  expect(vi.mocked(first.commandInteractiveRun).mock.calls[0][3]?.signal?.aborted).toBe(true);
  expect(await screen.findByLabelText("Guidance")).toHaveValue("");
  await act(async () => { resolveOld({ status: "saved", accepted: true, runId: "run-1", phase: "escalated", journalSequence: 8 }); });
  expect(screen.queryByText(/Guidance saved/)).not.toBeInTheDocument();
  expect(second.commandInteractiveRun).not.toHaveBeenCalled();
  expect(second.getInteractiveRun).toHaveBeenCalledTimes(1);
});

it("does not carry an uncertain command key into a new stage occurrence", async () => {
  const client: DaemonClient = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getInteractiveRun").mockResolvedValue(view());
  const command = vi.spyOn(client, "commandInteractiveRun").mockRejectedValue(new Error("lost"));
  render(<RunInterventionPanel client={client} runId="run-1" />);
  fireEvent.change(await screen.findByLabelText("Guidance"), { target: { value: "Old occurrence draft" } });
  fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
  await screen.findByRole("button", { name: "Check same request" });
  const next = view(); next.actions[0].subjectSequence = 9;
  vi.mocked(client.getInteractiveRun).mockResolvedValue(next);
  fireEvent.click(screen.getByRole("button", { name: "Refresh human operations" }));
  await screen.findByText("Observed occurrence 9");
  expect(screen.getByLabelText("Guidance")).toHaveValue("");
  expect(screen.queryByRole("button", { name: "Check same request" })).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Guidance"), { target: { value: "New occurrence draft" } });
  fireEvent.click(screen.getByRole("button", { name: "Save guidance" }));
  await waitFor(() => expect(command).toHaveBeenCalledTimes(2));
  expect(command.mock.calls[1][1]).not.toBe(command.mock.calls[0][1]);
  expect(command.mock.calls[1][2]).toMatchObject({ expectedSubjectSequence: 9, guidance: "New occurrence draft" });
});

it("refuses a read response for another run", async () => {
  const client: DaemonClient = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getInteractiveRun").mockResolvedValue({ ...view(), runId: "foreign" });
  render(<RunInterventionPanel client={client} runId="run-1" />);
  await screen.findByText(/Human operations are unavailable/);
  expect(screen.queryByLabelText("Guidance")).not.toBeInTheDocument();
});

it("retains queued restart identity without implying its execution already exists", async () => {
  const state = view();
  state.actions = [{ kind: "restart", stage: "review", subjectSequence: 8, decisions: [], available: true, reason: "" }];
  state.guidance = [{ request: { schema: "goobers.dev/operator-message/request/v1", requestId: "note", idempotencyKey: "note-key", targetAddress: "stage:review@8", principalRef: "issuer:human", requestedAt: "2026-10-04T10:00:00Z", purpose: "stage-restart-guidance", content: { text: "Use the corrected requirements." }, deliveryMode: "shared-guidance" }, state: "accepted" }];
  const client: DaemonClient = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getInteractiveRun").mockResolvedValue(state);
  const command = vi.spyOn(client, "commandInteractiveRun").mockResolvedValueOnce({ status: "pending", accepted: true, runId: "run-1", continuationRunId: "reserved-run", phase: "escalated", journalSequence: 8 }).mockResolvedValueOnce({ status: "started", accepted: true, runId: "run-1", continuationRunId: "reserved-run", phase: "escalated", journalSequence: 8 });
  render(<RunInterventionPanel client={client} runId="run-1" />);
  fireEvent.click(await screen.findByRole("checkbox", { name: /corrected requirements/ }));
  fireEvent.change(screen.getByLabelText("Rationale"), { target: { value: "Ready to resume" } });
  fireEvent.click(screen.getByRole("button", { name: "Restart stage" }));
  await screen.findByText(/Restart queued/);
  expect(screen.getByText("reserved-run")).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Open restarted execution" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Check same request" }));
  expect(await screen.findByRole("link", { name: "Open restarted execution" })).toHaveAttribute("href", "#/run/reserved-run");
  expect(command.mock.calls[1].slice(0, 3)).toEqual(command.mock.calls[0].slice(0, 3));
});
