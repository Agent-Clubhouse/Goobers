import { configDefaults, defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import type { ProxyOptions } from "vite";

const DEFAULT_DAEMON_URL = "http://127.0.0.1:8080";
const DEFAULT_GUIDED_URL = "http://127.0.0.1:8081";

interface PortalEnvironment {
  GOOBERS_DAEMON_URL?: string;
  GOOBERS_GUIDED_URL?: string;
}

export function createViteConfig(
  environment: PortalEnvironment = process.env,
  mode = "development",
) {
  const gettingStarted = mode === "getting-started";
  const guidedProxy: ProxyOptions = {
    target: environment.GOOBERS_GUIDED_URL ?? DEFAULT_GUIDED_URL,
    changeOrigin: true,
    configure(proxy) {
      proxy.on("proxyReq", (proxyRequest) => {
        proxyRequest.removeHeader("origin");
      });
    },
  };
  return {
    plugins: [
      react(),
      {
        name: "goobers-dashboard-mode",
        transformIndexHtml(html: string) {
          if (!gettingStarted) {
            return html;
          }
          return html
            .replace(
              'name="goobers-dashboard-mode" content="daemon"',
              'name="goobers-dashboard-mode" content="getting-started"',
            )
            .replace(
              "<title>Goobers · local operations</title>",
              "<title>Getting Started | Goobers</title>",
            );
        },
      },
    ],
    build: {
      outDir: "../cmd/goobers/portal-dist",
      emptyOutDir: true,
    },
    server: {
      proxy: {
        "/api": {
          target: environment.GOOBERS_DAEMON_URL ?? DEFAULT_DAEMON_URL,
          changeOrigin: true,
        },
        "/guided": guidedProxy,
      },
    },
    test: {
      css: true,
      environment: "jsdom",
      exclude: [...configDefaults.exclude, "e2e/**"],
      globals: true,
      setupFiles: "./src/test/setup.ts",
    },
  };
}

export default defineConfig(({ mode }) => createViteConfig(process.env, mode));
