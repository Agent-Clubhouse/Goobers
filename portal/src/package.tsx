import { App } from "./App";
import type { DaemonClient } from "./api/types";
import type { PortalDiagnostics } from "./portalDiagnostics";

export interface PortalWorkbenchProps {
  client: DaemonClient;
  /** Include the principal, instance and route/filter identity; never include credentials. */
  scope: string;
  diagnostics?: PortalDiagnostics;
  mode?: "daemon" | "standalone";
}

export function PortalWorkbench({
  client,
  scope,
  diagnostics,
  mode = "daemon",
}: PortalWorkbenchProps) {
  if (!client || !scope.trim()) {
    throw new Error("PortalWorkbench requires an explicit client and nonempty cursor scope.");
  }
  return (
    <App
      key={scope}
      client={client}
      diagnostics={diagnostics}
      mode={mode}
      cursorScope={scope}
      liveDataConfig={{ pollingEnabled: false }}
    />
  );
}

export { HttpDaemonClient, type HttpDaemonClientConfig } from "./api/httpClient";
export { createPortalDiagnostics, type PortalDiagnostics } from "./portalDiagnostics";
export { publishReadState } from "./liveData";
export type * from "./api/types";
