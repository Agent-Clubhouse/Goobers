import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { act, useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { QueryState } from "../api/queryState";
import type { ValidationWarning } from "../api/types";
import { configurationWarningKey } from "../configurationWarnings";
import { instanceWarnings, workflowWarnings } from "../prototypeData";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { ConfigurationWarnings } from "./ConfigurationWarnings";

// The warning surface reads through the same daemon client the rest of the app
// uses, so stub only the two warning reads over a complete fixture client.
// The operational pages read the daemon; the warning surface reads its own
// (defaulted) client. Tests drive them separately.
function daemonClient() {
  return new FixtureDaemonClient(populatedDaemonFixtures());
}

function prototypeWarningClient() {
  return {
    getInstance: vi.fn().mockResolvedValue({ warnings: instanceWarnings }),
    getWorkflow: vi.fn().mockResolvedValue({ warnings: workflowWarnings.implementation }),
  };
}

const modelWarning: ValidationWarning = {
  code: "MODEL002",
  severity: "warning",
  scope: "Goober/coder",
  explanation: "requested model is unavailable; using the harness default",
};

const previewWarning: ValidationWarning = {
  code: "VER002",
  severity: "warning",
  scope: "workflows/deploy.yaml Workflow/deploy",
  explanation: "preview feature in use",
};

const workflowWarning: ValidationWarning = {
  code: "VER003",
  severity: "warning",
  scope: "gaggles/alpha/workflows/deploy.yaml Gaggle/alpha Workflow/deploy",
  explanation:
    "expectedOutputs is declared but the stage has no inputs.resultFile to emit it through",
};

function renderWarnings(
  state: QueryState<readonly ValidationWarning[]>,
  options: {
    context?: "instance" | "workflow";
    dismissed?: ReadonlySet<string>;
  } = {},
) {
  const onDismiss = vi.fn();
  const onRefresh = vi.fn();
  render(
    <ConfigurationWarnings
      context={options.context ?? "instance"}
      dismissedWarningKeys={options.dismissed ?? new Set()}
      onDismiss={onDismiss}
      onRefresh={onRefresh}
      state={state}
    />,
  );
  return { onDismiss, onRefresh };
}

describe("ConfigurationWarnings", () => {
  beforeEach(() => {
    window.location.hash = "#/overview";
    delete document.documentElement.dataset.theme;
  });

  it("renders the coded API fields unchanged with read-only remediation guidance", () => {
    renderWarnings({ status: "ready", data: [modelWarning] });

    expect(screen.getByText("MODEL002")).toBeInTheDocument();
    expect(screen.getByText("warning")).toBeInTheDocument();
    expect(screen.getByText("Goober/coder")).toBeInTheDocument();
    expect(screen.getByText(modelWarning.explanation)).toBeInTheDocument();
    expect(screen.getByText("1 active warning")).toBeInTheDocument();
    expect(screen.getByText("goobers validate")).toBeInTheDocument();
    expect(screen.getByText(/The portal is read-only/)).toBeInTheDocument();
  });

  it("groups and orders multiple warnings by scope, code, then explanation", () => {
    const earlierCode = {
      ...workflowWarning,
      code: "VER001" as const,
      explanation: "deprecated feature remains supported",
    };
    const laterExplanation = {
      ...workflowWarning,
      explanation: "run.image is not honored by the local runner",
    };
    renderWarnings({
      status: "ready",
      data: [previewWarning, laterExplanation, workflowWarning, earlierCode, modelWarning],
    });

    expect(
      screen.getAllByTestId("configuration-warning").map((warning) => warning.textContent),
    ).toEqual([
      expect.stringContaining("MODEL002"),
      expect.stringContaining(earlierCode.explanation),
      expect.stringContaining(workflowWarning.explanation),
      expect.stringContaining(laterExplanation.explanation),
      expect.stringContaining("VER002"),
    ]);
    expect(
      screen.getByRole("region", {
        name: `${workflowWarning.scope} configuration warnings`,
      }),
    ).toContainElement(screen.getByText(laterExplanation.explanation));
  });

  it("distinguishes a warning-free read from a warning read failure", () => {
    const { unmount } = render(
      <ConfigurationWarnings
        context="instance"
        dismissedWarningKeys={new Set()}
        onDismiss={vi.fn()}
        onRefresh={vi.fn()}
        state={{ status: "empty" }}
      />,
    );

    expect(screen.getByText("No active configuration warnings.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    unmount();
    renderWarnings({ status: "error", error: new Error("Daemon warning read failed.") });
    expect(screen.getByRole("alert")).toHaveTextContent("Configuration warnings unavailable");
    expect(screen.getByRole("alert")).toHaveTextContent("Daemon warning read failed.");
    expect(screen.queryByRole("button", { name: /Dismiss/ })).not.toBeInTheDocument();
  });

  it("uses the shared loading state while warnings are being read", () => {
    renderWarnings({ status: "loading" });

    expect(screen.getByRole("status")).toHaveTextContent("Loading configuration warnings");
    expect(screen.queryByText("No active configuration warnings.")).not.toBeInTheDocument();
  });

  it("keeps a stale API error visible when its warning has been dismissed", () => {
    renderWarnings(
      {
        status: "stale",
        data: [modelWarning],
        error: new Error("Refresh failed."),
      },
      { dismissed: new Set([configurationWarningKey(modelWarning)]) },
    );

    expect(screen.getByRole("alert")).toHaveTextContent("Refresh failed.");
    expect(screen.getByText("Warnings dismissed for this portal session.")).toBeInTheDocument();
  });

  it("keeps dismissal session-local across routes and restores active warnings on refresh", async () => {
    const user = userEvent.setup();
    render(<App client={daemonClient()} warningClient={prototypeWarningClient()} />);

    await user.click(
      await screen.findByRole("button", {
        name: /Dismiss VER003 warning for .*Workflow\/implementation/,
      }),
    );
    // Route directly: this test is about dismissal surviving a route change,
    // not about how the daemon-backed workflows index renders its links.
    act(() => {
      window.location.hash = "#/workflow/implementation";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });

    expect(
      await screen.findByText("Warnings dismissed for this portal session."),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Refresh warnings" }));
    expect(
      await screen.findByRole("button", {
        name: /Dismiss VER003 warning for .*Workflow\/implementation/,
      }),
    ).toBeInTheDocument();
  });

  it("supports keyboard dismissal without mutating the warning", async () => {
    const user = userEvent.setup();

    function DismissalHarness() {
      const [dismissed, setDismissed] = useState<ReadonlySet<string>>(() => new Set());
      return (
        <ConfigurationWarnings
          context="instance"
          dismissedWarningKeys={dismissed}
          onDismiss={(warning) => setDismissed(new Set([configurationWarningKey(warning)]))}
          onRefresh={() => setDismissed(new Set())}
          state={{ status: "ready", data: [modelWarning] }}
        />
      );
    }

    render(<DismissalHarness />);
    const dismiss = screen.getByRole("button", {
      name: "Dismiss MODEL002 warning for Goober/coder",
    });
    dismiss.focus();
    await user.keyboard("{Enter}");

    expect(screen.getByText("Warnings dismissed for this portal session.")).toBeInTheDocument();
    expect(modelWarning).toEqual({
      code: "MODEL002",
      severity: "warning",
      scope: "Goober/coder",
      explanation: "requested model is unavailable; using the harness default",
    });
  });

  it("dismisses same-code warnings independently", async () => {
    const user = userEvent.setup();
    const otherFinding = {
      ...modelWarning,
      explanation: "A second model fallback is active.",
    };

    function DismissalHarness() {
      const [dismissed, setDismissed] = useState<ReadonlySet<string>>(() => new Set());
      return (
        <ConfigurationWarnings
          context="instance"
          dismissedWarningKeys={dismissed}
          onDismiss={(warning) =>
            setDismissed((current) => new Set(current).add(configurationWarningKey(warning)))
          }
          onRefresh={() => setDismissed(new Set())}
          state={{ status: "ready", data: [modelWarning, otherFinding] }}
        />
      );
    }

    render(<DismissalHarness />);
    const warning = screen.getByText(modelWarning.explanation).closest("article");
    if (!warning) {
      throw new Error("Expected model warning article.");
    }
    await user.click(
      within(warning).getByRole("button", {
        name: "Dismiss MODEL002 warning for Goober/coder",
      }),
    );

    expect(screen.queryByText(modelWarning.explanation)).not.toBeInTheDocument();
    expect(screen.getByText(otherFinding.explanation)).toBeInTheDocument();
  });

  it("loads exact workflow-scoped warnings from the existing workflow endpoint", async () => {
    window.location.hash = "#/workflow/implementation";
    const warningClient = {
      getInstance: vi.fn(),
      getWorkflow: vi.fn().mockResolvedValue({ warnings: [workflowWarning] }),
    };
    render(<App client={daemonClient()} warningClient={warningClient} />);

    expect(await screen.findByText(workflowWarning.scope)).toBeInTheDocument();
    expect(screen.getByText(workflowWarning.explanation)).toBeInTheDocument();
    expect(warningClient.getWorkflow).toHaveBeenCalledWith(
      "goobers",
      "implementation",
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    expect(warningClient.getInstance).not.toHaveBeenCalled();
  });

  it("keeps API failures retryable and separate from warning dismissal", async () => {
    const user = userEvent.setup();
    const getInstance = vi
      .fn()
      .mockRejectedValueOnce(new Error("Instance API unavailable."))
      .mockResolvedValueOnce({ warnings: [modelWarning] });
    render(<App client={daemonClient()} warningClient={{ getInstance, getWorkflow: vi.fn() }} />);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Instance API unavailable.");
    await user.click(within(alert).getByRole("button", { name: "Try again" }));

    expect(await screen.findByText(modelWarning.explanation)).toBeInTheDocument();
    expect(getInstance).toHaveBeenCalledTimes(2);
  });

  it("retains warning semantics in light and dark themes below run attention", async () => {
    const user = userEvent.setup();
    render(<App client={daemonClient()} warningClient={prototypeWarningClient()} />);

    const warning = (await screen.findAllByTestId("configuration-warning"))[0];
    const attention = screen.getByRole("heading", { name: "Needs attention" });
    const warningHeading = screen.getByRole("heading", { name: "Configuration warnings" });
    expect(attention.compareDocumentPosition(warningHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
    expect(document.documentElement.dataset.theme).toBe("light");
    expect(warning).toHaveClass("configuration-warning");

    await user.click(screen.getByRole("button", { name: "Use dark theme" }));
    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(warning).toHaveClass("configuration-warning");
  });
});
