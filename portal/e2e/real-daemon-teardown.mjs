const controlPort = process.env.PORTAL_E2E_CONTROL_PORT ?? "4175";

export default async function teardownRealDaemon() {
  const response = await fetch(`http://127.0.0.1:${controlPort}/shutdown`, {
    method: "POST",
    signal: AbortSignal.timeout(15_000),
  });
  if (!response.ok) {
    throw new Error(`real dashboard cleanup failed with HTTP ${response.status}`);
  }
}
