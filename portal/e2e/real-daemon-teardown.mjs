import { existsSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { setTimeout as delay } from "node:timers/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const portalRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const cacheRoot = join(portalRoot, "node_modules", ".cache", "real-daemon");
const port = process.env.PORTAL_E2E_REAL_PORT ?? "4174";
const shutdownRequest = join(cacheRoot, `shutdown-${port}.request`);
const shutdownAck = join(cacheRoot, `shutdown-${port}.ack`);

export default async function teardownRealDaemon() {
  mkdirSync(cacheRoot, { recursive: true });
  writeFileSync(shutdownRequest, "stop\n");
  const deadline = Date.now() + 15_000;
  while (!existsSync(shutdownAck)) {
    if (Date.now() >= deadline) {
      throw new Error("real dashboard did not acknowledge cleanup within 15 seconds");
    }
    await delay(50);
  }
  rmSync(shutdownRequest, { force: true });
  rmSync(shutdownAck, { force: true });
}
