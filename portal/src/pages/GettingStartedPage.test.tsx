import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  GuidedClient,
  type GuidedRepositoryInspection,
  type GuidedState,
} from "../guided/client";
import { GettingStartedPage } from "./GettingStartedPage";

function guidedState(overrides: Partial<GuidedState> = {}): GuidedState {
  return {
    version: 2,
    platform: "windows",
    workdir: "C:\\work",
    instancePath: "C:\\work\\tutorial-instance",
    configPath: "C:\\work\\tutorial-instance-config",
    instanceExists: false,
    env: {
      tokenEnv: "GOOBERS_GITHUB_REPO_TOKEN",
      goobersGithubToken: false,
      goobersGithubIssuesToken: false,
    },
    job: null,
    apiReady: false,
    connected: { repo: null },
    ...overrides,
  };
}

const inspection: GuidedRepositoryInspection = {
  provider: "github",
  owner: "acme",
  name: "widgets",
  displayName: "acme/widgets",
  gaggleName: "widgets",
  localPath: "C:\\src\\widgets",
  defaultBranch: "trunk",
  stack: "Node.js",
  ciCommand: ["npm", "run", "ci"],
  requiredCapabilities: ["node@20"],
  discovery: "deterministic",
  evidence: ["package.json: scripts.ci"],
  needsClone: false,
  peerConfigPath: "C:\\src\\widgets-goobers",
  inRepoConfigPath: "C:\\src\\widgets\\goobers",
  auth: {
    kind: "github-cli",
    ready: true,
    identity: "octocat",
  },
};

type RouteHandler = (init?: RequestInit) => { status?: number; body: unknown };

function clientWith(routes: Record<string, RouteHandler>): GuidedClient {
  const fetchFn = vi.fn(async (input: string, init?: RequestInit) => {
    const handler = routes[input];
    if (!handler) {
      return new Response(JSON.stringify({ code: "not_found", message: input }), {
        status: 404,
        headers: { "Content-Type": "application/json" },
      });
    }
    const { status = 200, body } = handler(init);
    return new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  });
  return new GuidedClient(fetchFn);
}

function parseBody(init?: RequestInit): unknown {
  return JSON.parse(String(init?.body ?? "null"));
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/**
 * A client whose `/guided/state` reads settle only when the test says so, and
 * which answers even an aborted read — the shape of the polling defect (#3660),
 * where a response that had already settled arrives after a newer one.
 */
function gatedStateClient(routes: Record<string, RouteHandler> = {}) {
  const pending: Array<(state: GuidedState) => void> = [];
  const signals: Array<AbortSignal | null | undefined> = [];
  const fetchFn = vi.fn(async (input: string, init?: RequestInit) => {
    if (input === "/guided/state") {
      signals.push(init?.signal);
      return jsonResponse(await new Promise<GuidedState>((resolve) => pending.push(resolve)));
    }
    const handler = routes[input];
    if (!handler) {
      return jsonResponse({ code: "not_found", message: input }, 404);
    }
    const { status = 200, body } = handler(init);
    return jsonResponse(body, status);
  });
  return { client: new GuidedClient(fetchFn), fetchFn, pending, signals };
}

async function continueWizard(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "Continue" }));
}

async function openRepositoryPage(user: ReturnType<typeof userEvent.setup>) {
  await screen.findByRole("heading", { name: "Welcome to Goobers" });
  await continueWizard(user);
}

describe("GettingStartedPage", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
  });

  it("explains how to open setup when guided endpoints are unavailable", async () => {
    render(<GettingStartedPage client={clientWith({})} />);

    expect(
      await screen.findByRole("heading", { name: "Setup is not available from this dashboard" }),
    ).toBeInTheDocument();
    expect(screen.getByText("goobers init --guided")).toBeInTheDocument();
  });

  it("uses fixed ten-step progress", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({ "/guided/state": () => ({ body: guidedState() }) })}
      />,
    );

    expect(await screen.findByRole("heading", { name: "Welcome to Goobers" })).toBeInTheDocument();
    const progress = screen.getByRole("progressbar");
    expect(progress).toHaveAttribute("aria-valuemax", "10");
    expect(progress).toHaveAttribute("aria-valuenow", "1");

    await continueWizard(user);
    expect(progress).toHaveAttribute("aria-valuenow", "2");
    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(progress).toHaveAttribute("aria-valuenow", "1");
  });

  it("discovers the repository and submits provider-aware guided options", async () => {
    const user = userEvent.setup();
    const initBodies: unknown[] = [];
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/inspect-repository": () => ({ body: inspection }),
          "/guided/actions/init-instance": (init) => {
            initBodies.push(parseBody(init));
            return {
              body: {
                exitCode: 0,
                stdout: "Created C:\\work\\tutorial-instance with 2 workflow module(s).",
                stderr: "",
              },
            };
          },
        })}
      />,
    );

    await openRepositoryPage(user);
    await user.type(screen.getByRole("textbox", { name: "Local clone" }), "C:\\src\\widgets");
    await user.click(screen.getByRole("button", { name: "Inspect clone" }));
    expect(await screen.findByText("GitHub CLI authentication is ready as octocat.")).toBeInTheDocument();
    expect(screen.getByText("trunk")).toBeInTheDocument();
    expect(screen.getByText("npm run ci")).toBeInTheDocument();
    expect(screen.getByText("node@20")).toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: "Local clone" }), "{enter}");
    expect(screen.getByRole("heading", { name: "Choose where Instance Configuration lives" })).toBeInTheDocument();
    await continueWizard(user);
    expect(screen.getByRole("heading", { name: "Set up your first gaggle" })).toBeInTheDocument();
    expect(screen.getByText("gaggles/widgets/")).toBeInTheDocument();
    expect(
      screen.getByText(/production-oriented canonical modules/i),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: /Work nomination/ }));
    await continueWizard(user);
    expect(
      screen.getByRole("heading", {
        name: "Choose which ready issues Goobers may implement",
      }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: /Only issues assigned to me/ }));
    await continueWizard(user);
    await continueWizard(user);

    expect(screen.getByRole("heading", { name: "Review and create the instance" })).toBeInTheDocument();
    expect(screen.getByText("C:\\src\\widgets-goobers")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Create Goobers instance" }));

    expect(initBodies).toEqual([
      {
        template: "guided",
        guided: {
          provider: "github",
          owner: "acme",
          name: "widgets",
          localPath: "C:\\src\\widgets",
          configPath: "C:\\src\\widgets-goobers",
          branch: "trunk",
          workflows: ["backlog-curation", "implementation"],
          issueScope: "assigned",
          assignedTo: "octocat",
          ciCommand: ["npm", "run", "ci"],
          requiredCapabilities: ["node@20"],
          harness: "copilot",
          repoTokenEnv: "GOOBERS_GITHUB_REPO_TOKEN",
          workTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
          githubCLIUser: "octocat",
          pullRequestTokenEnv: "GOOBERS_GITHUB_PR_TOKEN",
          repoPushTokenEnv: "GOOBERS_GITHUB_PUSH_TOKEN",
        },
      },
    ]);
  });

  it("switches repository entry modes and browses for a local clone", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/choose-repository-folder": () => ({
            body: { path: "C:\\src\\widgets", canceled: false },
          }),
          "/guided/actions/inspect-repository": () => ({ body: inspection }),
        })}
      />,
    );

    await openRepositoryPage(user);
    expect(screen.getByRole("textbox", { name: "Local clone" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Browse…" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Repository URL" }));
    expect(screen.getByRole("textbox", { name: "Repository URL" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Browse…" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Local clone" }));
    await user.click(screen.getByRole("button", { name: "Browse…" }));
    expect(await screen.findByDisplayValue("C:\\src\\widgets")).toBeInTheDocument();
    expect(screen.getByText("GitHub CLI authentication is ready as octocat.")).toBeInTheDocument();
  });

  it("ends after checks with commands for operating the instance", async () => {
    const user = userEvent.setup();
    window.sessionStorage.setItem("goobers-wizard-path", JSON.stringify("own-repo"));
    window.sessionStorage.setItem("goobers-wizard-page", JSON.stringify(8));
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({ instanceExists: true, connected: { repo: "acme/widgets" } }),
          }),
          "/guided/actions/validate": () => ({
            body: {
              exitCode: 0,
              envelope: { ok: true, counts: { errors: 0, warnings: 0 }, findings: [] },
              stderr: "",
            },
          }),
        })}
      />,
    );

    expect(await screen.findByRole("heading", { name: "Check the setup" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Run checks" }));
    await screen.findByText(/All configuration, harness, and repository checks passed/);
    await continueWizard(user);
    expect(screen.getByRole("heading", { name: "Goobers is ready" })).toBeInTheDocument();
    expect(screen.getByText(/goobers up/)).toBeInTheDocument();
    expect(screen.getByText(/goobers run implementation/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Start run" })).not.toBeInTheDocument();
  });

  describe("state polling", () => {
    afterEach(() => {
      vi.useRealTimers();
    });

    it("schedules the next state poll only after the previous one settles", async () => {
      vi.useFakeTimers();
      const { client, fetchFn, pending } = gatedStateClient();
      render(<GettingStartedPage client={client} />);

      await act(async () => {});
      expect(fetchFn).toHaveBeenCalledTimes(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(6_000);
      });
      expect(fetchFn).toHaveBeenCalledTimes(1);

      await act(async () => {
        pending[0](guidedState());
      });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_000);
      });
      expect(fetchFn).toHaveBeenCalledTimes(2);
    });

    it("stops polling and abandons the read in flight when the page unmounts", async () => {
      const { client, fetchFn, pending, signals } = gatedStateClient();
      const view = render(<GettingStartedPage client={client} />);
      await waitFor(() => expect(pending).toHaveLength(1));

      view.unmount();
      expect(signals[0]?.aborted).toBe(true);

      await act(async () => {
        pending[0](guidedState());
      });
      expect(fetchFn).toHaveBeenCalledTimes(1);
    });

    it("ignores a superseded state read that settles after a newer one", async () => {
      window.sessionStorage.setItem("goobers-wizard-page", JSON.stringify(8));
      const user = userEvent.setup();
      const { client, pending } = gatedStateClient({
        "/guided/actions/validate": () => ({
          body: {
            exitCode: 0,
            envelope: { ok: true, counts: { errors: 0, warnings: 0 }, findings: [] },
            stderr: "",
          },
        }),
      });
      render(<GettingStartedPage client={client} />);

      await waitFor(() => expect(pending).toHaveLength(1));
      await act(async () => {
        pending[0](guidedState({ instanceExists: true, instancePath: "C:\\first" }));
      });
      expect(await screen.findByRole("heading", { name: "Check the setup" })).toBeInTheDocument();

      await user.click(screen.getByRole("button", { name: "Run checks" }));
      await waitFor(() => expect(pending).toHaveLength(2));
      await user.click(screen.getByRole("button", { name: "Run checks" }));
      await waitFor(() => expect(pending).toHaveLength(3));

      await act(async () => {
        pending[2](guidedState({ instanceExists: true, instancePath: "C:\\newest" }));
      });
      await act(async () => {
        pending[1](guidedState({ instanceExists: true, instancePath: "C:\\stale" }));
      });

      expect(screen.getByText(/C:\\newest/)).toBeInTheDocument();
      expect(screen.queryByText(/C:\\stale/)).not.toBeInTheDocument();
    });
  });
});
