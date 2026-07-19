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
});
