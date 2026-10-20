import { expect, test } from "@playwright/test";
import { mkdir } from "node:fs/promises";
import type { Command, InteractiveEvent, Session } from "../src/interactive/client";

// Controlled native-driver events qualify browser behavior without a model
// call. The separate interactive.acceptance suite qualifies the real backend.
test("approval dock preserves accepted requests and applies confirmed driver settings", async ({page}, testInfo) => {
  const session: Session = {id: "controls", owner: "reviewer", surfaceId: "workbench", mode: "workbench", executionMode: "codex-local", title: "Review the requested action", status: "running", sequence: 0, fence: 7, activeTurnId: "turn-1", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z", turns: [{id: "turn-1", text: "Run the focused checks", status: "running", attempt: 1, fence: 7, startedAt: "2026-01-01T00:00:00Z"}], children: [], changes: []};
  const caps = {steer: true, queue: "none", interrupt: false, questions: true, permissionExtras: {denyMessage: true}, liveLevels: ["plan", "ask"], modelSwitch: "per_turn", effortSwitch: "live"};
  const events: InteractiveEvent[] = [];
  const commands: Command[] = [];
  function emit(type: string, data: Record<string, unknown>, commandId?: string) {
    const sequence = ++session.sequence;
    // Match ProjectChatEvent's durable host wrapper, including the preserved
    // v2 payload. Human command receipts originate in the host itself.
    const driverEvent = !type.startsWith("command.");
    events.push({sequence, sessionId: session.id, turnId: session.activeTurnId, commandId, type: driverEvent ? "execution.note" : type, time: "2026-01-01T00:00:00Z",
      data: driverEvent ? {chat: {v: 2, agentId: "controlled", sessionId: session.id, turnId: session.activeTurnId, seq: sequence, ts: Date.parse("2026-01-01T00:00:00Z"), fence: session.fence, type, data}} : data});
  }
  emit("session_info", {capabilities: caps, currentModel: "model-a", currentEffort: "high", level: "ask", models: [{id: "model-a", label: "Model A", efforts: ["low", "high"]}, {id: "model-b", label: "Model B", efforts: ["medium"]}]});
  emit("permission_request", {requestId: "permission-1", title: "Run focused tests", content: [{type: "text", text: "npm test -- invoice"}], options: [{optionId: "allow_once", kind: "allow_once", label: "Allow once"}, {optionId: "reject_once", kind: "reject_once", label: "Deny"}]});
  await page.route("**/api/v1/interactive/**", async route => {
    const url = new URL(route.request().url());
    let json: unknown;
    if (url.pathname.endsWith("/status")) json = {providerMode: "controlled-local", executionMode: "codex-local", identity: {name: "Reviewer", subject: "reviewer", issuer: "controlled"}, connections: [], limits: {maxTurnSeconds: 120, maxChildren: 0}, capabilities: {liveModel: false, liveDelegatedAuth: false}, executionModes: [{id: "codex-local", label: "Controlled agent", available: true}]};
    else if (url.pathname.endsWith("/surfaces")) json = {surfaces: [{id: "workbench", name: "Workbench", description: "Controlled driver events", mode: "workbench", enabled: true, maxChildren: 0}]};
    else if (url.pathname.endsWith("/sessions")) json = {sessions: [session]};
    else if (url.pathname.endsWith("/events")) {
      const replay = events.filter(event => event.sequence > Number(url.searchParams.get("after") ?? 0));
      if (route.request().headers().accept?.includes("text/event-stream")) {
        await route.fulfill({contentType: "text/event-stream", body: replay.map(event => `event: interactive\ndata: ${JSON.stringify(event)}\n\n`).join("")}); return;
      }
      json = {events: replay};
    } else if (url.pathname.endsWith("/commands")) {
      const command = route.request().postDataJSON() as Command;
      expect(command.turnId).toBe("turn-1"); expect(command.expectedFence).toBe(7);
      commands.push(command);
      emit("command.accepted", {commandType: command.type, requestId: command.requestId}, command.commandId);
      json = {commandId: command.commandId, sessionId: session.id, turnId: "turn-1", type: command.type, status: "accepted", sequence: session.sequence};
    } else json = session;
    await route.fulfill({json});
  });
  await page.goto("/#/interactive/controls");
  const card = page.getByRole("region", {name: "Permission request"});
  await expect(card).toContainText("npm test -- invoice");
  await page.setViewportSize({width: 390, height: 844});
  await expect(card.getByRole("button", {name: "Deny", exact: true})).toBeInViewport({ratio: 1});
  await expect(page.getByRole("combobox", {name: "Permission level"})).toBeVisible();
  const evidence = process.env.INTERACTIVE_EVIDENCE_DIR;
  if (evidence) { await mkdir(evidence, {recursive: true}); await page.screenshot({path: `${evidence}/approval-dock-mobile.png`, fullPage: true}); }
  await card.getByRole("textbox", {name: "Denial message"}).fill("Run only the focused checks");
  await card.getByRole("button", {name: "Deny", exact: true}).click();
  await expect.poll(() => commands.length).toBe(1);
  expect(commands[0]).toMatchObject({type: "permission.respond", requestId: "permission-1", optionId: "reject_once", message: "Run only the focused checks"});
  await page.reload();
  await expect(card).toContainText("Response accepted");
  await expect(card.getByRole("button", {name: "Allow once"})).toBeDisabled();
  emit("permission_resolved", {requestId: "permission-1", by: "human"});
  emit("question", {requestId: "question-1", questions: [{questionId: "checks", prompt: "Which checks should run?", options: [{id: "unit", label: "Unit"}, {id: "browser", label: "Browser"}], allowMultiple: true, allowFreeText: false}]});
  const question = page.getByRole("region", {name: "Question", exact: true});
  await expect(question).toBeVisible();
  await question.getByRole("button", {name: "Unit", exact: true}).click();
  await question.getByRole("button", {name: "Browser", exact: true}).click();
  if (evidence) await page.screenshot({path: `${evidence}/question-dock-mobile.png`, fullPage: true});
  await question.getByRole("button", {name: "Send answer"}).click();
  await expect.poll(() => commands.length).toBe(2);
  expect(commands[1]).toMatchObject({type: "question.answer", answers: {checks: ["unit", "browser"]}});
  emit("question_resolved", {requestId: "question-1", by: "human"});
  await expect(question).not.toBeVisible();
  await page.getByRole("combobox", {name: "Model", exact: true}).selectOption("model-b");
  await expect.poll(() => commands.length).toBe(3);
  await expect(page.getByRole("combobox", {name: "Model", exact: true})).toHaveValue("model-a");
  emit("command.delivered", {commandType: "session.set_model", actual: "model-b"}, commands[2].commandId);
  await expect(page.getByRole("combobox", {name: "Model", exact: true})).toHaveValue("model-b");
  await expect(page.getByRole("combobox", {name: "Effort", exact: true})).toHaveValue("unreported");
  await page.getByRole("combobox", {name: "Effort", exact: true}).selectOption("");
  await expect.poll(() => commands.length).toBe(4);
  expect(commands[3]).toMatchObject({type: "session.set_effort", effort: ""});
  emit("command.delivered", {commandType: "session.set_effort", actual: ""}, commands[3].commandId);
  await expect(page.getByRole("combobox", {name: "Effort", exact: true})).toHaveValue("");
  emit("permission_request", {requestId: "opaque", title: "Opaque tool request", content: [], options: [{optionId: "allow_once", label: "Allow once"}, {optionId: "reject_once", label: "Deny"}]});
  await expect(card.getByRole("button", {name: "Allow once"})).toBeDisabled();
  emit("permission_resolved", {requestId: "opaque", by: "timeout"});
  await expect(card).not.toBeVisible();
  // Codex escalations are notices, never a human permission request.
  emit("notice", {message: "Approval declined by host policy"});
  await expect(page.getByText("Approval declined by host policy")).toBeVisible();
  await expect(page.getByRole("button", {name: "Allow once"})).toHaveCount(0);
  await testInfo.attach("control-commands", {body: JSON.stringify(commands, null, 2), contentType: "application/json"});
});
