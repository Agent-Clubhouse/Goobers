import { afterEach, describe, expect, it, vi } from "vitest";
import { HttpInteractiveClient } from "./client";
import { humanControlHeaders } from "./humanAuth";

afterEach(() => { document.cookie = "goobers-human-csrf=; Max-Age=0; Path=/"; });

describe("human control CSRF binding", () => {
  it("does not invent authority before bootstrap", () => {
    expect(humanControlHeaders()).toEqual({});
  });
  it("adds the browser's CSRF binding to control mutations without storing it in command bodies", async () => {
    document.cookie = "goobers-human-csrf=fixture-binding; Path=/";
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ status: "accepted" })));
    const client = new HttpInteractiveClient("/api/v1/interactive", fetcher);
    await client.selectIdentity("alice");
    expect(fetcher.mock.calls[0][1]).toMatchObject({
      credentials: "same-origin",
      headers: { "X-Goobers-CSRF": "fixture-binding" },
      body: JSON.stringify({ name: "alice" }),
    });
  });
});
