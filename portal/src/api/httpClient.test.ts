import type { AddressInfo } from "node:net";
import {
  createServer,
  type RequestListener,
  type Server,
  type ServerResponse,
} from "node:http";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  DaemonApiError,
  DaemonUnavailableError,
  MalformedResponseError,
  RequestCancelledError,
  RequestTimeoutError,
  UnsupportedSchemaVersionError,
} from "./errors";
import { HttpDaemonClient } from "./httpClient";
import { API_VERSION, SCHEMA_VERSION, type Health } from "./types";

const health: Health = {
  apiVersion: API_VERSION,
  schemaVersion: SCHEMA_VERSION,
  ready: true,
  instance: { name: "local", environment: "dev" },
  freshness: {
    observedAt: "2026-07-18T00:00:00Z",
    definitionsLoadedAt: "2026-07-18T00:00:00Z",
    journalUpdatedAt: null,
  },
};

const servers: Server[] = [];

afterEach(async () => {
  await Promise.all(servers.splice(0).map(closeServer));
});

describe("HttpDaemonClient", () => {
  it("uses the same origin by default", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json(health));
    const client = new HttpDaemonClient({ fetch: fetcher });

    await expect(client.getHealth()).resolves.toEqual(health);
    expect(fetcher).toHaveBeenCalledWith(
      "/api/v1/health",
      expect.objectContaining({ method: "GET" }),
    );
  });

  it("maps every available daemon read route and preserves empty lists", async () => {
    const requests: string[] = [];
    const { baseUrl } = await startServer((request, response) => {
      requests.push(request.url ?? "");
      if (request.url?.includes("/artifacts/")) {
        const body = Buffer.from("artifact");
        response.writeHead(200, {
          "Content-Type": "text/plain",
          "Content-Length": body.byteLength,
          "X-Goobers-Digest": "sha256:abc",
          ETag: '"sha256:abc"',
        });
        response.end(body);
        return;
      }
      if (request.url === "/api/v1/health") {
        json(response, health);
        return;
      }
      if (request.url === "/api/v1/instance") {
        json(response, {
          apiVersion: API_VERSION,
          schemaVersion: SCHEMA_VERSION,
        });
        return;
      }
      if (request.url?.startsWith("/api/v1/runs?")) {
        json(response, { runs: [] });
        return;
      }
      json(response, {});
    });
    const client = new HttpDaemonClient({ baseUrl });

    await client.getHealth();
    await client.getInstance();
    await client.listGaggles({ limit: 10, cursor: "next" });
    await client.listGoobers("core", { limit: 5 });
    await client.listWorkflows("core", { cursor: "workflow-page" });
    await client.getWorkflow("core", "implementation");
    await expect(
      client.listRuns({
        gaggle: "core",
        workflow: "implementation",
        phase: "running",
        trigger: "item",
        limit: 25,
        cursor: "run-page",
      }),
    ).resolves.toEqual({ runs: [] });
    await client.getRun("run-1");
    await client.listRunEvents("run-1");
    await client.listStageAttempts("run-1", "implement");
    await expect(client.getArtifact("run-1", "sha256:abc")).resolves.toMatchObject({
      digest: "sha256:abc",
      mediaType: "text/plain",
      size: 8,
    });
    await client.getTelemetryStats({
      workflow: "implementation",
      gaggle: "core",
      since: "2026-07-01T00:00:00Z",
      until: "2026-07-18T00:00:00Z",
    });
    await client.listTelemetryErrors({
      workflow: "implementation",
      gaggle: "core",
      errorClass: "timeout",
      since: "2026-07-01T00:00:00Z",
      until: "2026-07-18T00:00:00Z",
      limit: 20,
      cursor: "error-page",
    });

    expect(requests).toEqual([
      "/api/v1/health",
      "/api/v1/instance",
      "/api/v1/gaggles?limit=10&cursor=next",
      "/api/v1/gaggles/core/goobers?limit=5",
      "/api/v1/gaggles/core/workflows?cursor=workflow-page",
      "/api/v1/gaggles/core/workflows/implementation",
      "/api/v1/runs?gaggle=core&workflow=implementation&phase=running&trigger=item&limit=25&cursor=run-page",
      "/api/v1/runs/run-1",
      "/api/v1/runs/run-1/events",
      "/api/v1/runs/run-1/stages/implement/attempts",
      "/api/v1/runs/run-1/artifacts/sha256%3Aabc",
      "/api/v1/telemetry/stats?workflow=implementation&gaggle=core&since=2026-07-01T00%3A00%3A00Z&until=2026-07-18T00%3A00%3A00Z",
      "/api/v1/telemetry/errors?workflow=implementation&gaggle=core&class=timeout&since=2026-07-01T00%3A00%3A00Z&until=2026-07-18T00%3A00%3A00Z&limit=20&cursor=error-page",
    ]);
  });

  it("surfaces request cancellation distinctly", async () => {
    let requestSeen!: () => void;
    const seen = new Promise<void>((resolve) => {
      requestSeen = resolve;
    });
    const { baseUrl } = await startServer(() => requestSeen());
    const client = new HttpDaemonClient({ baseUrl });
    const controller = new AbortController();

    const request = client.getHealth({ signal: controller.signal });
    await seen;
    controller.abort();

    await expect(request).rejects.toBeInstanceOf(RequestCancelledError);
  });

  it("surfaces timeouts distinctly", async () => {
    const { baseUrl } = await startServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.write('{"ready":');
    });
    const client = new HttpDaemonClient({ baseUrl, timeoutMs: 10 });

    await expect(client.getHealth()).rejects.toBeInstanceOf(RequestTimeoutError);
  });

  it("rejects unsupported schema versions explicitly", async () => {
    const { baseUrl } = await startServer((_request, response) => {
      json(response, { ...health, schemaVersion: "v2" });
    });

    await expect(new HttpDaemonClient({ baseUrl }).getHealth()).rejects.toBeInstanceOf(
      UnsupportedSchemaVersionError,
    );
  });

  it("surfaces structured API errors without substituting fixtures", async () => {
    const { baseUrl } = await startServer((_request, response) => {
      json(response, { error: { code: "telemetry_unavailable", message: "telemetry is not enabled" } }, 503);
    });

    await expect(new HttpDaemonClient({ baseUrl }).getTelemetryStats()).rejects.toMatchObject({
      status: 503,
      code: "telemetry_unavailable",
      message: "telemetry is not enabled",
    } satisfies Partial<DaemonApiError>);
  });

  it("surfaces malformed JSON responses distinctly", async () => {
    const { baseUrl } = await startServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end("{");
    });

    await expect(new HttpDaemonClient({ baseUrl }).getHealth()).rejects.toBeInstanceOf(
      MalformedResponseError,
    );
  });

  it("surfaces an unavailable daemon distinctly", async () => {
    const started = await startServer(() => {});
    await closeServer(started.server);

    await expect(new HttpDaemonClient({ baseUrl: started.baseUrl }).getHealth()).rejects.toBeInstanceOf(
      DaemonUnavailableError,
    );
  });
});

async function startServer(
  handler: RequestListener,
): Promise<{ baseUrl: string; server: Server }> {
  const server = createServer(handler);
  servers.push(server);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return { baseUrl: `http://127.0.0.1:${port}`, server };
}

async function closeServer(server: Server): Promise<void> {
  server.closeAllConnections();
  if (!server.listening) {
    return;
  }
  await new Promise<void>((resolve, reject) => {
    server.close((error) => (error ? reject(error) : resolve()));
  });
}

function json(
  response: ServerResponse,
  value: unknown,
  status = 200,
): void {
  response.writeHead(status, { "Content-Type": "application/json" });
  response.end(JSON.stringify(value));
}
