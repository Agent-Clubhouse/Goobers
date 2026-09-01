import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
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

const authRequiredInspection: GuidedRepositoryInspection = {
  ...inspection,
  auth: {
    kind: "github-cli",
    ready: false,
    message: "GitHub CLI authentication is required before setup can continue.",
    remediationCommand: "gh auth login --hostname github.com --git-protocol https --web",
    needsLogin: true,
  },
};

const remoteAuthRequiredInspection: GuidedRepositoryInspection = {
  ...authRequiredInspection,
  localPath: undefined,
  needsClone: true,
};

const remoteReadyInspection: GuidedRepositoryInspection = {
  ...inspection,
  localPath: undefined,
  needsClone: true,
};

const githubAccessRequiredInspection: GuidedRepositoryInspection = {
  ...inspection,
  auth: {
    kind: "github-cli",
    ready: false,
    identity: "octocat",
    message:
      "GitHub CLI is authenticated as octocat, but that account cannot access acme/widgets.",
    remediationCommand: "gh repo view acme/widgets",
    needsLogin: false,
  },
};

const adoAuthRequiredInspection: GuidedRepositoryInspection = {
  ...inspection,
  provider: "ado",
  project: "platform",
  auth: {
    kind: "azure-cli",
    ready: false,
    message: "Azure CLI authentication is required before setup can continue.",
    remediationCommand: "az login",
    needsLogin: true,
  },
};

const adoAccessRequiredInspection: GuidedRepositoryInspection = {
  ...adoAuthRequiredInspection,
  auth: {
    kind: "azure-cli",
    ready: false,
    identity: "azure-user@example.com",
    message:
      "Azure CLI is authenticated as azure-user@example.com, but Azure DevOps access to acme/platform/widgets could not be verified.",
    remediationCommand:
      'az repos show --organization https://dev.azure.com/acme --project "platform" --repository "widgets"',
    needsLogin: false,
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

  it("starts GitHub device authorization from the repository step", async () => {
    const user = userEvent.setup();
    let authorizationRequests = 0;
    let inspectionRequests = 0;
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/inspect-repository": () => {
            inspectionRequests += 1;
            return { body: inspectionRequests === 1 ? authRequiredInspection : inspection };
          },
          "/guided/actions/authorize-github": (init) => {
            authorizationRequests += 1;
            expect(parseBody(init)).toEqual({ repository: "acme/widgets" });
            return {
              body: {
                auth: inspection.auth,
                message: "GitHub device/web authorization completed as octocat.",
              },
            };
          },
        })}
      />,
    );

    await openRepositoryPage(user);
    await user.type(screen.getByRole("textbox", { name: "Local clone" }), "C:\\src\\widgets");
    await user.click(screen.getByRole("button", { name: "Inspect clone" }));
    expect(
      await screen.findByText("GitHub CLI authentication is required before setup can continue."),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Sign in with GitHub" }));
    expect(authorizationRequests).toBe(1);
    expect(inspectionRequests).toBe(2);
    expect(
      await screen.findByText("GitHub CLI authentication is ready as octocat."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with GitHub" })).not.toBeInTheDocument();
  });

  it("shows GitHub authorization before cloning a repository URL", async () => {
    const user = userEvent.setup();
    let inspectionRequests = 0;
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/inspect-repository": () => {
            inspectionRequests += 1;
            return {
              body: inspectionRequests === 1 ? remoteAuthRequiredInspection : remoteReadyInspection,
            };
          },
          "/guided/actions/authorize-github": (init) => {
            expect(parseBody(init)).toEqual({ repository: "acme/widgets" });
            return {
              body: {
                auth: remoteReadyInspection.auth,
                message: "GitHub device/web authorization completed as octocat.",
              },
            };
          },
        })}
      />,
    );

    await openRepositoryPage(user);
    await user.click(screen.getByRole("button", { name: "Repository URL" }));
    await user.type(
      screen.getByRole("textbox", { name: "Repository URL" }),
      "https://github.com/acme/widgets",
    );
    await user.click(screen.getByRole("button", { name: "Inspect URL" }));
    expect(
      await screen.findByText("GitHub CLI authentication is required before setup can continue."),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in with GitHub" })).toBeInTheDocument();
    expect(screen.getByText("Authenticate, then clone the repository")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Sign in with GitHub" }));
    expect(inspectionRequests).toBe(2);
    expect(screen.getByText("Clone the repository, then inspect it again")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with GitHub" })).not.toBeInTheDocument();
  });

  it("shows GitHub PAT recovery guidance for authenticated access failures", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/inspect-repository": () => ({ body: githubAccessRequiredInspection }),
        })}
      />,
    );

    await openRepositoryPage(user);
    await user.type(screen.getByRole("textbox", { name: "Local clone" }), "C:\\src\\widgets");
    await user.click(screen.getByRole("button", { name: "Inspect clone" }));
    expect(
      await screen.findByText(
        "GitHub CLI is authenticated as octocat, but that account cannot access acme/widgets.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "Open GitHub fine-grained PAT settings" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/Resource owner/)).toBeInTheDocument();
    expect(screen.getByText(/Only select repositories/)).toBeInTheDocument();
    expect(screen.getByText("gh repo view acme/widgets")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with GitHub" })).not.toBeInTheDocument();
  });

  it("keeps Azure DevOps authentication manual", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/inspect-repository": () => ({ body: adoAuthRequiredInspection }),
        })}
      />,
    );

    await openRepositoryPage(user);
    await user.type(screen.getByRole("textbox", { name: "Local clone" }), "C:\\src\\widgets");
    await user.click(screen.getByRole("button", { name: "Inspect clone" }));
    expect(
      await screen.findByText("Azure CLI authentication is required before setup can continue."),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Run the Azure CLI login command, then inspect the repository again. Azure DevOps authentication remains a manual step.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with GitHub" })).not.toBeInTheDocument();
  });

  it("directs Azure DevOps access failures to account access checks", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/inspect-repository": () => ({ body: adoAccessRequiredInspection }),
        })}
      />,
    );

    await openRepositoryPage(user);
    await user.type(screen.getByRole("textbox", { name: "Local clone" }), "C:\\src\\widgets");
    await user.click(screen.getByRole("button", { name: "Inspect clone" }));
    expect(
      await screen.findByText(
        "Azure CLI is authenticated as azure-user@example.com, but Azure DevOps access to acme/platform/widgets could not be verified.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Confirm that the authenticated Azure account can access this repository before continuing.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        'az repos show --organization https://dev.azure.com/acme --project "platform" --repository "widgets"',
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(
        "Run the Azure CLI login command, then inspect the repository again. Azure DevOps authentication remains a manual step.",
      ),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in with GitHub" })).not.toBeInTheDocument();
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

  it("gives GitHub PAT owner and repository guidance when authentication is unavailable", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/inspect-repository": () => ({
            body: {
              ...inspection,
              auth: {
                kind: "github-cli",
                ready: false,
                remediationCommand: "gh auth login",
                needsLogin: true,
              },
            },
          }),
        })}
      />,
    );

    await openRepositoryPage(user);
    await user.type(screen.getByRole("textbox", { name: "Local clone" }), "C:\\src\\widgets");
    await user.click(screen.getByRole("button", { name: "Inspect clone" }));

    expect(
      await screen.findByText("GitHub authentication is required before setup can continue."),
    ).toBeInTheDocument();
    const patLink = screen.getByRole("link", {
      name: "Open GitHub fine-grained PAT settings",
    });
    expect(patLink).toHaveAttribute(
      "href",
      "https://github.com/settings/personal-access-tokens/new",
    );
    expect(screen.getByText(/Resource owner/)).toBeInTheDocument();
    expect(screen.getByText("acme")).toBeInTheDocument();
    expect(screen.getByText(/Only select repositories/)).toBeInTheDocument();
    expect(screen.getByText("gh auth login")).toBeInTheDocument();
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
});
