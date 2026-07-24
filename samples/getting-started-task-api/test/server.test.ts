import assert from "node:assert/strict";
import type { Server } from "node:http";
import { afterEach, beforeEach, describe, it } from "node:test";
import { createTaskServer } from "../src/server.js";

describe("task API baseline", () => {
  let server: Server;
  let baseURL: string;

  beforeEach(async () => {
    server = createTaskServer();
    await new Promise<void>((resolve) => {
      server.listen(0, "127.0.0.1", resolve);
    });
    const address = server.address();
    if (address === null || typeof address === "string") {
      throw new Error("test server did not bind to a TCP port");
    }
    baseURL = `http://127.0.0.1:${address.port}`;
  });

  afterEach(async () => {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => {
        if (error === undefined) {
          resolve();
          return;
        }
        reject(error);
      });
    });
  });

  it("reports health", async () => {
    const response = await fetch(`${baseURL}/health`);

    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { status: "ok" });
  });

  it("lists the starter tasks", async () => {
    const response = await fetch(`${baseURL}/tasks`);
    const body = await response.json();

    assert.equal(response.status, 200);
    assert.equal(body.tasks.length, 2);
    assert.equal(body.tasks[0].title, "Read the getting-started guide");
  });

  it("creates a valid task", async () => {
    const response = await fetch(`${baseURL}/tasks`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ title: "Watch the workflow" })
    });

    assert.equal(response.status, 201);
    assert.deepEqual(await response.json(), {
      id: 3,
      title: "Watch the workflow",
      completed: false
    });
  });

  it("rejects malformed JSON", async () => {
    const response = await fetch(`${baseURL}/tasks`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: "{"
    });

    assert.equal(response.status, 400);
    assert.deepEqual(await response.json(), {
      error: "request body must be valid JSON"
    });
  });

  it("returns not found for an unknown route", async () => {
    const response = await fetch(`${baseURL}/missing`);

    assert.equal(response.status, 404);
    assert.deepEqual(await response.json(), { error: "not found" });
  });
});
