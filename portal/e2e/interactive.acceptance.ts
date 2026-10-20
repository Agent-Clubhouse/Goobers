import { humanBrowserOptions } from "./interactive-auth";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { mkdir, writeFile, readFile, realpath } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import { createServer } from "node:http";

const api = "/api/v1/interactive";

test("another loopback port cannot frame the authenticated portal", async ({ browser, baseURL }) => {
  const preview = createServer((_request, response) => {
    response.setHeader("Content-Type", "text/html");
    response.end(`<!doctype html><title>Controlled framing fixture</title><iframe src="${baseURL}/#/interactive"></iframe>`);
  });
  await new Promise<void>(resolve => preview.listen(0, "127.0.0.1", resolve));
  const address = preview.address();
  if (!address || typeof address === "string") throw new Error("Preview fixture did not bind");
  const context = await browser.newContext({ storageState: humanBrowserOptions().storageState });
  try {
    const page = await context.newPage();
    const blocked = page.waitForEvent("console", {
      predicate: message => message.type() === "error" && message.text().includes("frame-ancestors"),
    });
    await page.goto(`http://127.0.0.1:${address.port}`);
    expect((await blocked).text()).toContain("frame-ancestors");
    expect(page.frames().some(frame => frame.url().startsWith(`${baseURL}/`))).toBe(false);
    // Positive control: the same authenticated browser can load the portal at
    // top level; only embedding is refused.
    await page.goto(`${baseURL}/#/interactive`);
    await expect(page.getByRole("main")).toBeVisible();
    await expect(page).toHaveURL(`${baseURL}/#/interactive`);
  } finally {
    await context.close();
    await new Promise<void>(resolve => preview.close(() => resolve()));
  }
});

test("another loopback port cannot reuse captured cookies as human authority", async ({ browser, baseURL }) => {
  let captured = "";
  const preview = createServer((request, response) => {
    captured = request.headers.cookie ?? "";
    response.end("<!doctype html><title>Controlled preview fixture</title>");
  });
  await new Promise<void>(resolve => preview.listen(0, "127.0.0.1", resolve));
  const address = preview.address();
  if (!address || typeof address === "string") throw new Error("Preview fixture did not bind");
  // No global extraHTTPHeaders: real browser navigations must never attach the
  // portal's origin credential to a different server.
  const context = await browser.newContext({ storageState: humanBrowserOptions().storageState });
  try {
    const page = await context.newPage();
    await page.goto(`http://127.0.0.1:${address.port}${api}/preview`);
    expect(captured).toContain("goobers-human-session=");
    expect(await page.evaluate(() => localStorage.getItem("goobers.interactive.human-binding"))).toBeNull();
    for (const [path, method] of [["/status", "GET"], ["/sessions/fixture/events", "GET"], ["/sessions/fixture/commands", "POST"], ["/operations/actions/fixture/apply", "POST"]]) {
      const response = await fetch(`${baseURL}${api}${path}`, {
        method, headers: { Cookie: captured, "Content-Type": "application/json" },
        ...(method === "POST" ? { body: "{}" } : {}),
      });
      expect(response.status).toBe(403);
      await response.arrayBuffer();
    }
  } finally {
    await context.close();
    await new Promise<void>(resolve => preview.close(() => resolve()));
  }
});
const evidenceDirectory = resolve(
  process.env.INTERACTIVE_EVIDENCE_DIR ?? `${tmpdir()}/goobers-interactive-evidence`,
);

interface Surface { id: string; name: string; mode: string; enabled: boolean; maxChildren: number; configRevision: string }
interface Event {
  sequence: number; type: string; time: string; turnId?: string; commandId?: string;
  data: Record<string, unknown>;
}
interface Session {
  id: string;
  mode: string;
  executionMode: string;
  configDigest: string;
  configRevision: string;
  status: string;
  sequence: number;
  fence: number;
  custodyPending?: boolean;
  lease?: { status: string };
  activeTurnId?: string;
  turns: Array<{ id: string; text: string; status: string; attempt?: number }>;
  children: Array<{ id: string; status: string; result?: string }>;
  changes: Array<{ path: string; status: string }>;
  diff?: string;
  owner?: string;
  workspacePath?: string;
}

async function surfaces(request: APIRequestContext): Promise<Surface[]> {
  const response = await request.get(`${api}/surfaces`);
  expect(response.ok(), "the real interactive backend must be running").toBeTruthy();
  return (await response.json()).surfaces;
}

async function snapshot(request: APIRequestContext, id: string): Promise<Session> {
  const response = await request.get(`${api}/sessions/${id}`);
  expect(response.ok()).toBeTruthy();
  return response.json();
}

async function createSession(request: APIRequestContext, mode = "workbench") {
  const surface = (await surfaces(request)).find((value) => value.mode === mode && value.enabled);
  expect(surface, `an enabled ${mode} surface is required for this acceptance`).toBeDefined();
  const data = { commandId: crypto.randomUUID(), surfaceId: surface!.id, executionMode: "local-tools", title: `Acceptance ${mode}` };
  const response = await request.post(`${api}/sessions`, { data });
  expect(response.ok()).toBeTruthy();
  const session = await response.json() as Session;
  expect(session.executionMode).toBe("local-tools");
  return { session, data };
}

async function capture(page: Page, name: string) {
  await mkdir(evidenceDirectory, { recursive: true });
  await page.screenshot({ path: `${evidenceDirectory}/${name}.png`, fullPage: true });
}

async function events(request: APIRequestContext, id: string, after = 0): Promise<Event[]> {
  const response = await request.get(`${api}/sessions/${id}/events?after=${after}`);
  expect(response.ok()).toBeTruthy();
  return (await response.json()).events;
}

async function sendMessage(page: Page, text: string) {
  await page.getByRole("textbox", { name: "Message", exact: true }).fill(text);
  await page.getByRole("button", { name: "Send message", exact: true }).click();
}

async function awaitTurn(request: APIRequestContext, id: string, count: number) {
  await expect.poll(async () => {
    const current = await snapshot(request, id);
    return current.turns.length === count && ["idle", "error", "waiting_auth"].includes(current.status);
  }, { timeout: 60_000 }).toBeTruthy();
  return snapshot(request, id);
}

test.beforeAll(async ({ request }) => {
  const response = await request.get(`${api}/status`);
  expect(response.ok()).toBeTruthy();
  const status = await response.json();
  expect(status.defaultMode).toBe("codex-local");
  expect(status.capabilities).toMatchObject({ modelEnabled: false, liveModel: false, liveDelegatedAuth: false });
  expect(status.executionModes).toEqual(expect.arrayContaining([
    expect.objectContaining({ id: "codex-local", available: false, liveQualified: false }),
    expect.objectContaining({ id: "local-tools", available: true, liveQualified: false }),
  ]));
});

test.afterEach(async ({ page, request }, info) => {
  if (info.status === info.expectedStatus) return;
  const id = page.url().split("/interactive/")[1];
  if (!id?.startsWith("ses_")) return;
  // A failed UI assertion must not leave a real worker consuming the budget of
  // the next test. This cleanup never changes whether that assertion passed.
  for (let attempt = 0; attempt < 4; attempt++) {
    const response = await request.get(`${api}/sessions/${id}`);
    if (!response.ok()) return;
    const current = await response.json() as Session;
    if (current.status === "stopped") return;
    const stopped = await request.post(`${api}/sessions/${id}/commands`, { data: {
      commandId: crypto.randomUUID(), type: "stop", expectedSequence: current.sequence, expectedFence: current.fence,
    } });
    const result = await stopped.json();
    await info.attach(`failed-test-cleanup-${attempt}`, { body: JSON.stringify(result), contentType: "application/json" });
    if (stopped.ok() || result.error?.code !== "stale_sequence") return;
  }
});

test("session creation is idempotent and changed payloads cannot reuse an identity", async ({ request }) => {
  const surface = (await surfaces(request)).find(value => value.mode === "workbench" && value.enabled)!;
  expect(surface.id).toBe("interactive--workbench");
  for (const executionMode of [undefined, "codex-local"]) {
    const unavailable = await request.post(`${api}/sessions`, { data: {
      commandId: crypto.randomUUID(), surfaceId: surface.id, ...(executionMode ? { executionMode } : {}),
    } });
    expect(unavailable.status()).toBe(409);
    expect((await unavailable.json()).error.code).toBe("execution_mode_unavailable");
  }
  const { session, data } = await createSession(request);
  const replay = await request.post(`${api}/sessions`, { data });
  expect(replay.ok()).toBeTruthy();
  expect((await replay.json()).id).toBe(session.id);
  const conflict = await request.post(`${api}/sessions`, { data: { ...data, title: "Changed intent" } });
  expect(conflict.status()).toBe(409);
  expect((await conflict.json()).error.code).toBe("idempotency_conflict");
  const current = await snapshot(request, session.id);
  expect(current.turns ?? []).toHaveLength(0);
  expect(current.status).toBe("idle");
});

test("stale sequence and fence reject execution before any turn starts", async ({ request }) => {
  const { session } = await createSession(request);
  const data = {
    commandId: crypto.randomUUID(), type: "message", text: "investigate",
    expectedSequence: session.sequence, expectedFence: session.fence,
  };
  const staleSequence = await request.post(`${api}/sessions/${session.id}/commands`, {
    data: { ...data, expectedSequence: session.sequence + 100 },
  });
  expect(staleSequence.status()).toBe(409);
  expect((await staleSequence.json()).error.code).toBe("stale_sequence");
  const staleFence = await request.post(`${api}/sessions/${session.id}/commands`, {
    data: { ...data, commandId: crypto.randomUUID(), expectedFence: session.fence + 100 },
  });
  expect(staleFence.status()).toBe(409);
  expect((await staleFence.json()).error.code).toBe("stale_fence");
  const current = await snapshot(request, session.id);
  expect(current.turns ?? []).toHaveLength(0);
  expect(current.sequence).toBe(session.sequence);
});

test("the loopback host rejects foreign origins and rebinding hosts", async ({ request }) => {
  const { session } = await createSession(request);
  const foreignOrigin = await request.post(`${api}/sessions/${session.id}/commands`, {
    headers: { Origin: "https://attacker.invalid" },
    data: { commandId: crypto.randomUUID(), type: "message", text: "wait", expectedSequence: session.sequence, expectedFence: session.fence },
  });
  expect(foreignOrigin.status()).toBe(403);
  const rebindingHost = await request.get(`${api}/sessions/${session.id}`, { headers: { Host: "attacker.invalid" } });
  expect(rebindingHost.status()).toBe(403);
  expect((await snapshot(request, session.id)).turns ?? []).toHaveLength(0);
});

test("both surfaces render a new session and retain it after a narrow-screen reconnect", async ({ page, request }) => {
  for (const mode of ["helper", "workbench"]) {
    const { session } = await createSession(request, mode);
    await page.goto(`/#/interactive/${session.id}`);
    await expect(page.getByRole("textbox", { name: "Message", exact: true })).toBeVisible();
    await capture(page, `interactive-${mode}-desktop`);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload();
    await expect(page.getByRole("textbox", { name: "Message", exact: true })).toBeVisible();
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
    await capture(page, `interactive-${mode}-mobile`);
    await page.getByRole("textbox", { name: "Message", exact: true }).scrollIntoViewIfNeeded();
    await expect(page.getByRole("textbox", { name: "Message", exact: true })).toBeInViewport();
    await capture(page, `interactive-${mode}-mobile-composer`);
    expect((await snapshot(request, session.id)).turns ?? []).toHaveLength(0);
    await page.setViewportSize({ width: 1280, height: 800 });
  }
});

test("controlled local identities cannot read each other's sessions and execute in separate workspaces", async ({ browser, baseURL }) => {
  const contexts = await Promise.all([browser.newContext({ baseURL, ...humanBrowserOptions() }), browser.newContext({ baseURL, ...humanBrowserOptions() })]);
  try {
    const requests = contexts.map(context => context.request);
    for (const [index, name] of ["Mason", "Alex"].entries()) {
      const identity = await requests[index].post(`${api}/identity/select`, { data: { name } });
      expect(identity.ok()).toBeTruthy();
      expect((await identity.json()).mode).toBe("controlled-local");
    }
    const created = await Promise.all(requests.map(request => createSession(request)));
    for (let index = 0; index < 2; index++) {
      const other = created[1 - index].session;
      for (const suffix of ["", "/events", "/changes"]) {
        expect((await requests[index].get(`${api}/sessions/${other.id}${suffix}`)).status()).toBe(404);
      }
      const response = await requests[index].post(`${api}/sessions/${other.id}/commands`, {
        data: { commandId: crypto.randomUUID(), type: "stop", expectedSequence: other.sequence, expectedFence: other.fence },
      });
      expect(response.status()).toBe(404);
    }
    await Promise.all(created.map(async ({ session }, index) => {
      const response = await requests[index].post(`${api}/sessions/${session.id}/commands`, {
        data: { commandId: crypto.randomUUID(), type: "message", text: index === 0 ? "fix" : "investigate", expectedSequence: session.sequence, expectedFence: session.fence },
      });
      expect(response.status()).toBe(202);
    }));
    const completed = await Promise.all(created.map(({ session }, index) => awaitTurn(requests[index], session.id, 1)));
    expect(completed[0].owner).toBe("Mason");
    expect(completed[1].owner).toBe("Alex");
    expect(completed[0].workspacePath).toBeTruthy();
    expect(completed[0].workspacePath).not.toBe(completed[1].workspacePath);
    expect(completed[0].diff).toContain("Math.max");
    expect(completed[1].diff ?? "").toBe("");
    for (let index = 0; index < 2; index++) {
      const page = await contexts[index].newPage();
      await page.goto(`/#/interactive/${completed[index].id}`);
      await expect(page.getByRole("textbox", { name: "Message", exact: true })).toBeVisible();
      await capture(page, `interactive-local-owner-${index + 1}`);
    }
    await writeFile(`${evidenceDirectory}/controlled-identity-isolation.json`, JSON.stringify({
      identityMode: "controlled-local selectable personas; not remote authentication", sessions: completed,
    }, null, 2));
  } finally {
    await Promise.all(contexts.map(context => context.close()));
  }
});

test("real workspace investigates, edits, tests, reviews a child and safely stops after steering", async ({ page, request }) => {
  const workbench = (await surfaces(request)).find(surface => surface.mode === "workbench" && surface.enabled)!;
  expect(workbench).toBeDefined();
  const pageErrors: string[] = [];
  page.on("pageerror", error => pageErrors.push(error.message));
  await page.goto("/#/interactive");
  await expect(page.getByRole("radio", { name: "Agent (Codex)", exact: true })).toBeChecked();
  await expect(page.getByRole("radio", { name: "Agent (Codex)", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Start session", exact: true })).toBeDisabled();
  await expect(page.getByText("Not live-qualified", { exact: true })).toBeVisible();
  await page.getByRole("radio", { name: "Agent (Codex)", exact: true }).scrollIntoViewIfNeeded();
  await capture(page, "interactive-agent-unavailable-no-fallback");
  await page.getByRole("radio", { name: new RegExp(workbench.name) }).check();
  await expect(page.getByRole("button", { name: "Start session", exact: true })).toBeDisabled();
  await page.getByRole("radio", { name: "Deterministic demo", exact: true }).check();
  await page.getByRole("textbox", { name: /Session name/ }).fill("Invoice investigation acceptance");
  await page.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page).toHaveURL(/#\/interactive\/ses_/);
  const id = page.url().split("/interactive/")[1];
  const startedAt = Date.now();
  await sendMessage(page, "investigate");
  await expect.poll(async () => (await events(request, id)).some(event => event.type === "tool.completed" && event.data.name === "repository.read"), { timeout: 60_000 }).toBeTruthy();
  await expect(page.getByText("Completed: repository.read", { exact: true })).toBeVisible();
  const firstUsefulOutputMs = Date.now() - startedAt;
  await awaitTurn(request, id, 1);
  await capture(page, "interactive-investigated");
  await sendMessage(page, "fix");
  const edited = await awaitTurn(request, id, 2);
  expect(edited.status).toBe("idle");
  await sendMessage(page, "test");
  const tested = await awaitTurn(request, id, 3);
  expect(tested.status).toBe("idle");
  await page.getByRole("tab", { name: /^Changes/ }).click();
  await expect(page.getByLabel("Cumulative diff")).toContainText("invoice.mjs");
  await expect(page.getByLabel("Cumulative diff")).toContainText("Math.max");
  const reviewedDiff = (await request.get(`${api}/sessions/${id}/changes`));
  expect(reviewedDiff.ok()).toBeTruthy();
  const changesBeforeChild = await reviewedDiff.json();
  expect(changesBeforeChild.changes).toEqual(expect.arrayContaining([expect.objectContaining({ path: "invoice.mjs" })]));
  await capture(page, "interactive-cumulative-changes");

  await page.getByRole("tab", { name: /^Children/ }).click();
  await page.getByRole("textbox", { name: "Child task", exact: true }).fill("Review the invoice changes and tests, without modifying files");
  await page.getByRole("button", { name: "Propose child", exact: true }).click();
  await expect(page.getByRole("button", { name: "Run child", exact: true })).toBeVisible();
  await capture(page, "interactive-child-proposal");
  await page.getByRole("button", { name: "Run child", exact: true }).click();
  await expect(page.getByRole("button", { name: "Mark reviewed", exact: true })).toBeVisible({ timeout: 60_000 });
  const nativeChildEvents = (await events(request, id)).filter(event => event.data.name === "child.native-workflow");
  expect(nativeChildEvents.some(event => {
    const definition = String(event.data.workflowYaml);
    return event.data.executionMode === "native-goobers" && /dslVersion:\s*["']?3\.0/.test(definition) &&
      definition.includes("runsOn:") && /network:\s*none/.test(definition);
  }), "the child must carry a DSL3 definition with an enforced network:none runner restriction").toBeTruthy();
  expect(nativeChildEvents.some(event => {
    const result = event.data.result as { cleanupConfirmed?: boolean; runId?: string; status?: string; request?: { workflow?: string } } | undefined;
    return result?.cleanupConfirmed && result.runId && result.status === "succeeded" && result.request?.workflow === "review-child";
  }), "the child must have a real native scheduler receipt and confirmed process cleanup").toBeTruthy();
  await capture(page, "interactive-child-result");
  await page.getByRole("button", { name: "Mark reviewed", exact: true }).click();
  await expect.poll(async () => (await snapshot(request, id)).children[0]?.status).toBe("reviewed");
  const afterChild = await (await request.get(`${api}/sessions/${id}/changes`)).json();
  expect(afterChild.diff).toBe(changesBeforeChild.diff);

  await sendMessage(page, "wait");
  await expect(page.getByRole("textbox", { name: "Steer this turn", exact: true })).toBeVisible();
  await page.getByRole("textbox", { name: "Steer this turn", exact: true }).fill("Keep the public invoice API unchanged and preserve the current diff");
  await page.getByRole("button", { name: "Send steering", exact: true }).click();
  await expect.poll(async () => (await events(request, id)).some(event => event.type === "steering.delivered")).toBeTruthy();
  await capture(page, "interactive-steering-delivered");
  await page.getByRole("button", { name: "Disconnect stream", exact: true }).click();
  await expect(page.getByText("View disconnected. Accepted work may continue on the server.", { exact: false })).toBeVisible();
  const disconnected = await snapshot(request, id);
  expect(disconnected.status).toBe("running");
  expect(disconnected.turns).toHaveLength(4);
  await capture(page, "interactive-disconnected");
  await page.getByRole("button", { name: "Reconnect", exact: true }).click();
  await expect(page.locator("#main-content").getByText("Live stream", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Stop session", exact: true }).click();
  await expect.poll(async () => (await snapshot(request, id)).status).toBe("stopped");
  await expect(page.getByText("Session stopped. History and cumulative changes remain available for review.", { exact: true })).toBeVisible();
  await capture(page, "interactive-stopped");

  const persisted = await events(request, id);
  expect(new Set(persisted.map(event => event.sequence)).size).toBe(persisted.length);
  expect(persisted.map(event => event.sequence)).toEqual([...persisted.map(event => event.sequence)].sort((a, b) => a - b));
  const cursor = persisted[Math.floor(persisted.length / 2)].sequence;
  expect(await events(request, id, cursor)).toEqual(persisted.filter(event => event.sequence > cursor));
  const final = await snapshot(request, id);
  expect(final.turns).toHaveLength(4);
  expect(final.children).toHaveLength(1);
  expect(final.children[0].status).toBe("reviewed");
  expect(pageErrors).toEqual([]);
  await writeFile(`${evidenceDirectory}/workspace-journey.json`, JSON.stringify({
    measuredAt: new Date().toISOString(), firstUsefulOutputMs,
    executor: "controlled local tools with real repository subprocesses", provider: "controlled local ADO/Azure",
    session: final, events: persisted,
  }, null, 2));
});

test("a lost command response survives reload and retries the original accepted intent", async ({ page, request }) => {
  const { session } = await createSession(request);
  const submittedIDs: string[] = [];
  let loseNextResponse = true;
  // Transport fault injection: the real backend accepts the real request. Only
  // its first response is lost; no canned API response is substituted.
  await page.route(`**${api}/sessions/${session.id}/commands`, async route => {
    const body = route.request().postDataJSON();
    if (body.type === "message") submittedIDs.push(body.commandId);
    if (loseNextResponse && body.type === "message") {
      loseNextResponse = false;
      const response = await route.fetch();
      expect(response.status()).toBe(202);
      await route.abort("failed");
    } else await route.continue();
  });
  await page.goto(`/#/interactive/${session.id}`);
  await sendMessage(page, "wait");
  await expect(page.getByText("Command outcome unknown", { exact: true })).toBeVisible();
  await expect.poll(async () => (await snapshot(request, session.id)).turns.length).toBe(1);
  await capture(page, "interactive-command-response-lost");
  await page.reload();
  await expect(page.getByRole("button", { name: "Retry same command", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Retry same command", exact: true }).click();
  await expect.poll(() => submittedIDs.length).toBe(2);
  expect(submittedIDs[1]).toBe(submittedIDs[0]);
  expect((await snapshot(request, session.id)).turns).toHaveLength(1);
  await expect(page.getByRole("button", { name: "Retry same command", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Stop session", exact: true }).click();
  await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("stopped");
  await capture(page, "interactive-command-recovered");
});

test("helper authors and applies a native workflow, tweaks its gaggle and observes a real run", async ({ page, request }) => {
  const { session } = await createSession(request, "helper");
  const stateURL = `${api}/operations/state?sessionId=${session.id}`;
  const readState = async () => {
    let state: any;
    await expect.poll(async () => {
      const response = await request.get(stateURL);
      const body = await response.json();
      if (response.status() === 409 && body.error?.startsWith("helper busy:")) return false;
      expect(response.ok(), JSON.stringify(body)).toBeTruthy();
      state = body;
      return true;
    }).toBeTruthy();
    return state;
  };
  await page.goto(`/#/interactive/${session.id}`);
  await sendMessage(page, "fix");
  expect((await awaitTurn(request, session.id, 1)).status).toBe("idle");
  await sendMessage(page, "design workflow");
  expect((await awaitTurn(request, session.id, 2)).status).toBe("idle");
  await page.getByRole("tab", { name: "Actions", exact: true }).click();
  await expect(page.getByRole("button", { name: "Apply action", exact: true })).toBeVisible();
  await capture(page, "interactive-helper-workflow-preview");
  await page.getByRole("button", { name: "Apply action", exact: true }).click();
  await expect.poll(async () => (await readState()).workflows.some((workflow: { metadata: { name: string } }) => workflow.metadata.name === "invoice-check-custom")).toBeTruthy();

  await page.getByRole("combobox", { name: "Action", exact: true }).selectOption("tweak_gaggle");
  await page.getByRole("textbox", { name: "Gaggle display name", exact: true }).fill("Interactive acceptance lab");
  await page.getByRole("button", { name: "Preview action", exact: true }).click();
  await page.getByRole("button", { name: "Apply action", exact: true }).click();
  await expect.poll(async () => (await readState()).gaggles[0].spec.displayName).toBe("Interactive acceptance lab");

  await page.getByRole("combobox", { name: "Action", exact: true }).selectOption("submit_run");
  await page.getByRole("combobox", { name: "Workflow", exact: true }).selectOption("invoice-check-custom");
  await page.getByRole("button", { name: "Preview action", exact: true }).click();
  await capture(page, "interactive-helper-run-preview");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect.poll(async () => (await readState()).runs.some((run: {workflow: string; phase: string}) => run.workflow === "invoice-check-custom" && run.phase === "completed"), { timeout: 90_000 }).toBeTruthy();
  const final = await readState();
  const nativeAction = final.actions.find((action: {kind: string}) => action.kind === "submit_run");
  expect(nativeAction).toMatchObject({ status: "succeeded", operationStatus: "confirmed", cleanupConfirmed: true });
  expect(nativeAction.executionRepository).toBeTruthy();
  expect(nativeAction.executionRepository).not.toBe(final.repository);
  expect(nativeAction.executionRoot).toContain(`/executions/${nativeAction.id}/instance`);
  expect(nativeAction.executionBindingDigest).toBeTruthy();
  expect(nativeAction.executionPayloadDigest).toBeTruthy();
  const runReceipt = page.getByRole("region", { name: "Action proposals", exact: true }).locator("article").filter({ hasText: "Submit interactive/invoice-check-custom" });
  await expect(runReceipt.locator(".ix-status")).toHaveText("succeeded");
  await runReceipt.locator(".ix-status").scrollIntoViewIfNeeded();
  await capture(page, "interactive-helper-native-run-completed");
  const observedRun = page.getByRole("region", { name: "Observed runs", exact: true });
  await expect(observedRun).toContainText("completed");
  await observedRun.scrollIntoViewIfNeeded();
  await capture(page, "interactive-helper-native-journal-completed");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
  await capture(page, "interactive-helper-actions-mobile");
  await writeFile(`${evidenceDirectory}/helper-native-workflow.json`, JSON.stringify({
    execution: "actual Goobers configuration validator, transaction, CLI and native journal", provider: "controlled local fixture",
    state: final,
  }, null, 2));
});

test("DSL reload rejects invalid policy and fences existing work before new sessions use changed policy", async ({ page, request, browser, baseURL }) => {
  const startup = JSON.parse(await readFile(`${evidenceDirectory}/startup.json`, "utf8"));
  const fixtureConfig = await realpath(resolve(evidenceDirectory, "config"));
  expect(await realpath(startup.config), "reload qualification must only edit the launcher's copied config").toBe(fixtureConfig);
  const gagglePath = resolve(fixtureConfig, "gaggles/interactive/gaggle.yaml");
  const original = await readFile(gagglePath, "utf8");
  const updated = original.replace(/maxChildren:\s*3\b/g, "maxChildren: 2");
  expect(updated).not.toBe(original);
  const { session: helper } = await createSession(request, "helper");
  await page.goto(`/#/interactive/${helper.id}`);
  await sendMessage(page, "fix");
  expect((await awaitTurn(request, helper.id, 1)).status).toBe("idle");
  const proposed = await request.post(`${api}/operations/proposals?sessionId=${helper.id}`, { data: {
    kind: "tweak_gaggle", gaggle: "interactive", displayName: "Retained qualification receipt", idempotencyKey: crypto.randomUUID(),
  } });
  expect(proposed.ok(), await proposed.text()).toBeTruthy();
  const helperAction = await proposed.json();
  const receiptURL = `${api}/operations/actions/${helperAction.id}?sessionId=${helper.id}`;
  const stateURL = `${api}/operations/state?sessionId=${helper.id}`;
  const applied = await request.post(`${api}/operations/actions/${helperAction.id}/apply?sessionId=${helper.id}`, { data: {} });
  expect(applied.ok(), await applied.text()).toBeTruthy();
  expect((await applied.json()).status).toBe("succeeded");
  const { session } = await createSession(request);
  const surfaceBefore = (await surfaces(request)).find(surface => surface.id === "interactive--workbench")!;
  expect(surfaceBefore.maxChildren).toBe(3);
  expect(surfaceBefore.configRevision).toBeTruthy();
  await page.goto(`/#/interactive/${session.id}`);
  await sendMessage(page, "wait");
  await expect.poll(async () => (await events(request, session.id)).some(event => event.type === "tool.started" && event.data.name === "bounded.wait")).toBeTruthy();
  const before = await snapshot(request, session.id);
  let changedPublished = false;
  try {
    await writeFile(gagglePath, "invalid: [\n");
    const invalidReload = await request.post(`${api}/config/reload`, { data: {} });
    expect(invalidReload.status()).toBe(409);
    const afterInvalid = await snapshot(request, session.id);
    expect(afterInvalid.status).toBe("running");
    expect(afterInvalid.fence).toBe(before.fence);
    expect((await surfaces(request)).find(surface => surface.id === surfaceBefore.id)?.configRevision).toBe(surfaceBefore.configRevision);

    await writeFile(gagglePath, updated);
    const reload = await request.post(`${api}/config/reload`, { data: {} });
    expect(reload.ok(), await reload.text()).toBeTruthy();
    changedPublished = true;
    const reloadReceipt = await reload.json();
    expect(reloadReceipt.status).toBe("reloaded");
    expect(reloadReceipt.configRevision).not.toBe(surfaceBefore.configRevision);
    await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("recovery_required");
    await expect.poll(async () => (await snapshot(request, session.id)).lease?.status).toBe("released");
    const fenced = await snapshot(request, session.id);
    expect(fenced.custodyPending ?? false).toBe(false);
    expect(fenced.fence).toBeGreaterThan(before.fence);
    expect(fenced.configDigest).toBe(before.configDigest);
    expect(fenced.turns[0].status).toBe("interrupted");
    const rejected = await request.post(`${api}/sessions/${session.id}/commands`, { data: {
      commandId: crypto.randomUUID(), type: "message", text: "investigate", expectedSequence: fenced.sequence, expectedFence: fenced.fence,
    } });
    expect(rejected.status()).toBe(409);
    expect((await rejected.json()).error.code).toBe("surface_changed");
    await expect(page.getByRole("alert").getByText("Recovery required", { exact: true })).toBeVisible();
    await capture(page, "interactive-dsl-reload-fenced");

    const retainedReceipts: unknown[] = [];
    for (const phase of ["after policy reload", "after explicit stop"]) {
      if (phase === "after explicit stop") {
        const current = await snapshot(request, helper.id);
        const stopped = await request.post(`${api}/sessions/${helper.id}/commands`, { data: {
          commandId: crypto.randomUUID(), type: "stop", expectedSequence: current.sequence, expectedFence: current.fence,
        } });
        expect(stopped.ok(), await stopped.text()).toBeTruthy();
        await expect.poll(async () => (await snapshot(request, helper.id)).status).toBe("stopped");
      }
      const retainedAction = await request.get(receiptURL);
      expect(retainedAction.ok(), `${phase}: ${await retainedAction.text()}`).toBeTruthy();
      const receipt = await retainedAction.json();
      expect(receipt).toMatchObject({ id: helperAction.id, status: "succeeded", operationStatus: "confirmed" });
      const retainedState = await request.get(stateURL);
      expect(retainedState.ok(), `${phase}: ${await retainedState.text()}`).toBeTruthy();
      const state = await retainedState.json();
      expect(state.readOnly).toBe(true);
      expect(state.actions).toEqual(expect.arrayContaining([expect.objectContaining({ id: helperAction.id })]));
      retainedReceipts.push({ phase, receipt });
    }
    const foreignContext = await browser.newContext({ baseURL, ...humanBrowserOptions() });
    try {
      expect((await foreignContext.request.post(`${api}/identity/select`, { data: { name: "Alex" } })).ok()).toBeTruthy();
      for (const url of [receiptURL, stateURL]) {
        const hidden = await foreignContext.request.get(url);
        expect(hidden.status()).toBe(403);
        expect(await hidden.text()).not.toContain(helperAction.id);
      }
    } finally { await foreignContext.close(); }

    const surfaceAfter = (await surfaces(request)).find(surface => surface.id === surfaceBefore.id)!;
    expect(surfaceAfter.maxChildren).toBe(2);
    const { session: fresh } = await createSession(request);
    expect(fresh.configDigest).not.toBe(fenced.configDigest);
    expect(fresh.configRevision).toBe(reloadReceipt.configRevision);
    await page.goto(`/#/interactive/${fresh.id}`);
    // The policy change was issued by this API client, outside the page's
    // Refresh configuration control. Reload fetches the current surface catalog.
    await page.reload();
    await sendMessage(page, "investigate");
    const completed = await awaitTurn(request, fresh.id, 1);
    expect(completed.status).toBe("idle");
    await page.getByRole("tab", { name: /^Children/ }).click();
    await expect(page.getByText("Configured limit: 2 children", { exact: true })).toBeVisible();
    await capture(page, "interactive-dsl-reload-new-session");
    await writeFile(`${evidenceDirectory}/dsl-reload.json`, JSON.stringify({
      fixtureConfig, before, afterInvalid, reloadReceipt, fenced, surfaceAfter, fresh: completed, retainedReceipts,
      evidenceBoundary: "actual DSL validation, policy publication, running-turn fencing and new-session execution; controlled provider; no model",
    }, null, 2));
  } finally {
    await writeFile(gagglePath, original);
    if (changedPublished) {
      const restored = await request.post(`${api}/config/reload`, { data: {} });
      expect(restored.ok(), await restored.text()).toBeTruthy();
    }
  }
});

test("controlled provider expiry blocks work, explicit reauthentication resumes and revocation stays fenced", async ({ page, request }) => {
  const { session } = await createSession(request);
  await page.goto(`/#/interactive/${session.id}`);
  await page.getByRole("tab", { name: "Access", exact: true }).click();
  const ado = page.locator(".ix-connection").filter({ hasText: "Controlled local ADO" });
  const insights = page.locator(".ix-connection").filter({ hasText: "Controlled local App Insights" });
  await expect(ado).toBeVisible();
  await expect(page.getByText("No live Azure sign-in or delegation has been demonstrated.", { exact: false })).toBeVisible();
  // Expire the second provider so the first read already has a confirmed
  // receipt when resume rebinds authority. It must not become a new effect.
  await insights.getByRole("button", { name: "Expire demo connection", exact: true }).click();
  await expect(insights).toContainText("Interaction required");
  await sendMessage(page, "investigate");
  await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("waiting_auth");
  await expect(page.getByText("Waiting for authorization", { exact: true })).toBeVisible();
  expect((await events(request, session.id)).some(event => event.type === "turn.completed")).toBeFalsy();
  await capture(page, "interactive-auth-required");
  await insights.getByRole("button", { name: "Reauthenticate demo connection", exact: true }).click();
  await expect(insights.locator(".ix-status")).toHaveText("Ready");
  await page.getByRole("button", { name: "Resume session", exact: true }).click();
  await expect.poll(async () => (await snapshot(request, session.id)).turns[0].attempt).toBe(2);
  expect((await awaitTurn(request, session.id, 1)).status).toBe("idle");
  await capture(page, "interactive-auth-resumed");
  await sendMessage(page, "wait");
  await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("running");
  await expect.poll(async () => (await events(request, session.id)).some(event => event.type === "tool.started" && event.data.name === "bounded.wait")).toBeTruthy();
  await ado.getByRole("button", { name: "Revoke demo connection", exact: true }).click();
  await expect(ado.locator(".ix-status")).toHaveText("Revoked");
  await expect(ado.getByRole("button", { name: "Reauthenticate demo connection", exact: true })).toBeDisabled();
  await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("stopped");
  await expect(page.getByRole("textbox", { name: "Message", exact: true })).toBeDisabled();
  const stopped = await snapshot(request, session.id);
  expect(stopped.turns[1].status).toBe("cancelled");
  await capture(page, "interactive-auth-revoked");
  const { session: fresh } = await createSession(request);
  await page.goto(`/#/interactive/${fresh.id}`);
  await sendMessage(page, "investigate");
  await expect.poll(async () => (await snapshot(request, fresh.id)).status).toBe("waiting_auth");
  expect((await events(request, fresh.id)).filter(event => event.type === "tool.completed" && event.data.name === "repository.read")).toHaveLength(0);
  await capture(page, "interactive-revoked-new-session-blocked");
  await page.getByRole("button", { name: "Stop session", exact: true }).click();
  await expect.poll(async () => (await snapshot(request, fresh.id)).status).toBe("stopped");
});
