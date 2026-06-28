import { MsalProvider } from "@azure/msal-react";
import { render, screen } from "@testing-library/react";
import { PublicClientApplication } from "@azure/msal-browser";
import { describe, expect, it } from "vitest";
import { App } from "./App";
import { createMockGoobersApi, emptyInstanceSnapshot } from "./api/mockClient";
import { msalConfig } from "./auth/msal";

function renderWithAuth(ui: React.ReactElement) {
  const instance = new PublicClientApplication(msalConfig);
  return render(<MsalProvider instance={instance}>{ui}</MsalProvider>);
}

describe("App first boot", () => {
  it("shows the alive empty state when no gaggles exist", async () => {
    renderWithAuth(<App api={createMockGoobersApi({ snapshot: emptyInstanceSnapshot, runs: [], traces: [], gateApprovals: [] })} />);

    expect(await screen.findByText("I'm alive — your goober gaggle is ready")).toBeInTheDocument();
    expect(screen.getByText(/No gaggles are configured yet/i)).toBeInTheDocument();
    expect(screen.getByText("No runs have been recorded yet.")).toBeInTheDocument();
    expect(screen.getByText("No human approvals are waiting.")).toBeInTheDocument();
  });
});
