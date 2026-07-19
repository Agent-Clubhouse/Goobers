import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

const DEFAULT_DAEMON_URL = "http://127.0.0.1:8080";

interface PortalEnvironment {
  GOOBERS_DAEMON_URL?: string;
}

export function createViteConfig(environment: PortalEnvironment = process.env) {
  return {
    plugins: [react()],
    server: {
      proxy: {
        "/api": {
          target: environment.GOOBERS_DAEMON_URL ?? DEFAULT_DAEMON_URL,
          changeOrigin: true,
        },
      },
    },
    test: {
      css: true,
      environment: "jsdom",
      globals: true,
      setupFiles: "./src/test/setup.ts",
    },
  };
}

export default defineConfig(createViteConfig());
