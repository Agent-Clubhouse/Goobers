// @vitest-environment node

import { describe, expect, it } from "vitest";
import { createViteConfig } from "./vite.config";

describe("portal development proxy", () => {
  it("builds assets into the Go embed directory", () => {
    expect(createViteConfig({}).build).toEqual({
      outDir: "../cmd/goobers/portal-dist",
      emptyOutDir: true,
    });
  });

  it("routes same-origin API requests to the default daemon address", () => {
    expect(createViteConfig({}).server.proxy["/api"]).toEqual({
      target: "http://127.0.0.1:8080",
      changeOrigin: true,
    });
  });

  it("routes API requests to a configured daemon address", () => {
    expect(
      createViteConfig({
        GOOBERS_DAEMON_URL: "http://127.0.0.1:9090",
      }).server.proxy["/api"],
    ).toEqual({
      target: "http://127.0.0.1:9090",
      changeOrigin: true,
    });
  });

  it("routes guided requests to the Getting Started backend", () => {
    expect(createViteConfig({}).server.proxy["/guided"]).toMatchObject({
      target: "http://127.0.0.1:8081",
      changeOrigin: true,
    });
    expect(
      createViteConfig({
        GOOBERS_GUIDED_URL: "http://127.0.0.1:9091",
      }).server.proxy["/guided"],
    ).toMatchObject({
      target: "http://127.0.0.1:9091",
      changeOrigin: true,
    });
    expect(
      createViteConfig({}).server.proxy["/guided"].configure,
    ).toBeTypeOf("function");
  });

  it("stamps Getting Started mode for the dedicated dev command", () => {
    const plugin = createViteConfig({}, "getting-started").plugins[1];
    const html =
      '<meta name="goobers-dashboard-mode" content="daemon"><title>Goobers · local operations</title>';
    expect(plugin.transformIndexHtml(html)).toContain(
      'name="goobers-dashboard-mode" content="getting-started"',
    );
    expect(plugin.transformIndexHtml(html)).toContain(
      "<title>Getting Started | Goobers</title>",
    );
  });
});
