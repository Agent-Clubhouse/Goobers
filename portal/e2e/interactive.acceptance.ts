import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";

const api = "/api/v1/interactive";
const evidenceDirectory = resolve(
  process.env.INTERACTIVE_EVIDENCE_DIR ?? `${tmpdir()}/goobers-interactive-evidence`,
);

interface Surface { id: string; mode: string; enabled: boolean }
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
