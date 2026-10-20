import { afterEach, describe, expect, it, vi } from "vitest";
import { HttpInteractiveClient } from "./client";
import { humanControlHeaders } from "./humanAuth";

afterEach(() => { localStorage.clear(); });

describe("human control CSRF binding", () => {
  it("does not invent authority before bootstrap", () => {
    expect(humanControlHeaders()).toEqual({});
  });
  it("adds the browser's CSRF binding to control mutations without storing it in command bodies", async () => {
    localStorage.setItem("goobers.interactive.human-binding", "fixture-binding");
    const fetcher = vi.fn().mockImplementation(async () => new Response(JSON.stringify({ status: "accepted" })));
    const client = new HttpInteractiveClient("/api/v1/interactive", fetcher);
    await client.selectIdentity("alice");
    await client.status();
    expect(fetcher.mock.calls[1][1].headers).toMatchObject({ "X-Goobers-CSRF": "fixture-binding" });
    expect(fetcher.mock.calls[0][1]).toMatchObject({
      credentials: "same-origin",
      headers: { "X-Goobers-CSRF": "fixture-binding" },
      body: JSON.stringify({ name: "alice" }),
    });
  });
});
