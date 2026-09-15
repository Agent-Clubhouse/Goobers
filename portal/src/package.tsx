import { App, type DashboardMode } from "./App";
import type { DaemonClient } from "./api/types";
import type { PortalDiagnostics } from "./portalDiagnostics";
import type { PortalHeaderHost } from "./shell/PortalShell";

export interface PortalWorkbenchProps {
  client: DaemonClient;
  /** Include the principal, instance and route/filter identity; never include credentials. */
  scope: string;
  diagnostics?: PortalDiagnostics;
  mode?: Exclude<DashboardMode, "getting-started">;
  pollingEnabled?: boolean;
  headerHost?: PortalHeaderHost;
}

export function PortalWorkbench({
  client,
  scope,
  diagnostics,
  mode = "daemon",
  pollingEnabled = false,
  headerHost,
}: PortalWorkbenchProps) {
  if (!client || !scope.trim()) {
    throw new Error("PortalWorkbench requires an explicit client and nonempty cursor scope.");
  }
  if (headerHost && headerHost.target.ownerDocument !== document) {
    throw new Error("PortalWorkbench headerHost requires a same-document container.");
  }
  return (
    <App
      key={scope}
      client={client}
      diagnostics={diagnostics}
      mode={mode}
      cursorScope={scope}
      liveDataConfig={{ pollingEnabled }}
      headerHost={headerHost}
    />
  );
}

export { HttpDaemonClient, type HttpDaemonClientConfig } from "./api/httpClient";
export { createPortalDiagnostics, type PortalDiagnostics } from "./portalDiagnostics";
export { publishReadState } from "./liveData";
export type * from "./api/types";
export type { PortalHeaderHost } from "./shell/PortalShell";
