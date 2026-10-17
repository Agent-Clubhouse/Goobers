import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { mkdir, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";

const api = "/api/v1/interactive";
const evidenceDirectory = resolve(
  process.env.INTERACTIVE_EVIDENCE_DIR ?? `${tmpdir()}/goobers-interactive-evidence`,
);

interface Surface { id: string; name: string; mode: string; enabled: boolean }
interface Event {
  sequence: number; type: string; time: string; turnId?: string; commandId?: string;
  data: Record<string, unknown>;
}
interface Session {
  id: string;
  mode: string;
  status: string;
  sequence: number;
  fence: number;
  activeTurnId?: string;
  turns: Array<{ id: string; text: string; status: string }>;
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
  const data = { commandId: crypto.randomUUID(), surfaceId: surface!.id, title: `Acceptance ${mode}` };
  const response = await request.post(`${api}/sessions`, { data });
  expect(response.ok()).toBeTruthy();
  return { session: await response.json() as Session, data };
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

test("session creation is idempotent and changed payloads cannot reuse an identity", async ({ request }) => {
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
    expect((await snapshot(request, session.id)).turns ?? []).toHaveLength(0);
    await page.setViewportSize({ width: 1280, height: 800 });
  }
});

test("controlled local identities cannot read each other's sessions and execute in separate workspaces", async ({ browser, baseURL }) => {
  const contexts = await Promise.all([browser.newContext({ baseURL }), browser.newContext({ baseURL })]);
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
  await page.getByRole("radio", { name: new RegExp(workbench.name) }).check();
  await page.getByRole("textbox", { name: /Session name/ }).fill("Invoice investigation acceptance");
  await page.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page).toHaveURL(/#\/interactive\/ses_/);
  const id = page.url().split("/interactive/")[1];
  const startedAt = Date.now();
  await sendMessage(page, "investigate");
  await expect.poll(async () => (await events(request, id)).some(event => event.type === "tool.completed"), { timeout: 60_000 }).toBeTruthy();
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
  await expect(page.getByText("Live stream", { exact: true })).toBeVisible();
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
    const response = await request.get(stateURL);
    expect(response.ok()).toBeTruthy();
    return response.json();
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
  expect(final.actions.find((action: {kind: string}) => action.kind === "submit_run").status).toBe("succeeded");
  await capture(page, "interactive-helper-native-run-completed");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
  await capture(page, "interactive-helper-actions-mobile");
  await writeFile(`${evidenceDirectory}/helper-native-workflow.json`, JSON.stringify({
    execution: "actual Goobers configuration validator, transaction, CLI and native journal", provider: "controlled local fixture",
    state: final,
  }, null, 2));
});

test("controlled provider expiry blocks work, explicit reauthentication resumes and revocation stays fenced", async ({ page, request }) => {
  const { session } = await createSession(request);
  await page.goto(`/#/interactive/${session.id}`);
  await page.getByRole("tab", { name: "Access", exact: true }).click();
  const ado = page.locator(".ix-connection").filter({ hasText: "Controlled local ADO" });
  await expect(ado).toBeVisible();
  await expect(page.getByText("No live Azure sign-in or delegation has been demonstrated.", { exact: false })).toBeVisible();
  await ado.getByRole("button", { name: "Expire demo connection", exact: true }).click();
  await expect(ado).toContainText("Interaction required");
  await sendMessage(page, "investigate");
  await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("waiting_auth");
  await expect(page.getByText("Waiting for authorization", { exact: true })).toBeVisible();
  expect((await events(request, session.id)).some(event => event.type === "turn.completed")).toBeFalsy();
  await capture(page, "interactive-auth-required");
  await ado.getByRole("button", { name: "Reauthenticate demo connection", exact: true }).click();
  await expect(ado.locator(".ix-status")).toHaveText("Ready");
  await page.getByRole("button", { name: "Resume session", exact: true }).click();
  expect((await awaitTurn(request, session.id, 1)).status).toBe("idle");
  await capture(page, "interactive-auth-resumed");
  await ado.getByRole("button", { name: "Revoke demo connection", exact: true }).click();
  await expect(ado.locator(".ix-status")).toHaveText("Revoked");
  await expect(ado.getByRole("button", { name: "Reauthenticate demo connection", exact: true })).toBeDisabled();
  await sendMessage(page, "investigate");
  await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("waiting_auth");
  const revoked = await snapshot(request, session.id);
  const revokedTurn = revoked.turns[1].id;
  expect((await events(request, session.id)).filter(event => event.turnId === revokedTurn && event.type === "tool.completed" && event.data.name === "repository.read")).toHaveLength(0);
  await capture(page, "interactive-auth-revoked");
  await page.getByRole("button", { name: "Stop session", exact: true }).click();
  await expect.poll(async () => (await snapshot(request, session.id)).status).toBe("stopped");
});
