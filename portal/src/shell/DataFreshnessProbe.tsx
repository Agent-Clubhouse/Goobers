import type { DataFreshness } from "../liveData";
import { DataFreshnessIndicator } from "./PortalShell";

/**
 * Renders the freshness indicator in isolation, for tests.
 *
 * The indicator lives inside PortalShell, which needs a provider, a router, and
 * a config. Testing it through the full shell would make a test about three
 * text labels depend on all of that — and would fail for reasons unrelated to
 * what it asserts.
 */
export function PortalShellDataFreshnessProbe({ state }: { state: DataFreshness }) {
  return <DataFreshnessIndicator state={state} />;
}
