import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { GuidedClient, type GuidedState } from "../guided/client";
import { GettingStartedPage } from "./GettingStartedPage";

function guidedState(overrides: Partial<GuidedState> = {}): GuidedState {
  return {
    version: 1,
    workdir: "/work",
    samplePath: "/work/getting-started-task-api",
    instancePath: "/work/tutorial-instance",
    sampleExists: false,
    instanceExists: false,
    env: { tokenEnv: "GOOBERS_GITHUB_TOKEN", goobersGithubToken: false, goobersGithubIssuesToken: false },
    job: null,
    apiReady: false,
    connected: { repo: null },
    ...overrides,
  };
}

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

async function chooseSample(user: ReturnType<typeof userEvent.setup>) {
  await user.click(
    await screen.findByRole("button", { name: /Try the disposable sample/ }),
  );
}

async function chooseOwnRepo(user: ReturnType<typeof userEvent.setup>) {
  await user.click(
    await screen.findByRole("button", { name: /Connect your repository/ }),
  );
}

describe("GettingStartedPage", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
  });

  it("renders the instructional state when the guided endpoints are absent", async () => {
    render(<GettingStartedPage client={clientWith({})} />);

    expect(
      await screen.findByRole("heading", { name: "The guided experience is not running here" }),
    ).toBeInTheDocument();
    expect(screen.getByText("goobers getting-started")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Materialize the sample" })).toBeNull();
  });

  it("renders live env token badges from guided state", async () => {
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({
              env: {
                tokenEnv: "GOOBERS_GITHUB_TOKEN",
                goobersGithubToken: true,
                goobersGithubIssuesToken: false,
              },
            }),
          }),
        })}
      />,
    );

    const badges = await screen.findByLabelText("Token environment status");
    expect(badges).toHaveTextContent("GOOBERS_GITHUB_TOKEN set");
    expect(badges).toHaveTextContent("GOOBERS_GITHUB_ISSUES_TOKEN not set");
  });

  it("renders the path chooser with the own-repo card first and recommended", async () => {
    render(
      <GettingStartedPage
        client={clientWith({ "/guided/state": () => ({ body: guidedState() }) })}
      />,
    );

    const chooser = await screen.findByRole("group", { name: "Path chooser" });
    const cards = within(chooser).getAllByRole("button");
    expect(cards).toHaveLength(2);
    expect(cards[0]).toHaveTextContent("Connect your repository");
    expect(cards[0]).toHaveTextContent("Recommended");
    expect(cards[0]).toHaveTextContent("Your repo, your issues, a real first PR.");
    expect(cards[1]).toHaveTextContent("Try the disposable sample");
    expect(cards[1]).toHaveTextContent("A zero-stakes tutorial against a throwaway repo.");
    // No path chosen yet: neither branch's steps render.
    expect(screen.queryByRole("button", { name: "Materialize the sample" })).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Initialize the starter instance" }),
    ).toBeNull();
  });

  it("marks steps done from server truth and activates the first open step", async () => {
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({ sampleExists: true, instanceExists: true }),
          }),
        })}
      />,
    );

    // sampleExists infers the sample path on reload with nothing stored.
    const materialize = (
      await screen.findByRole("heading", { name: "Materialize the sample" })
    ).closest("li");
    const init = screen
      .getByRole("heading", { name: "Initialize the tutorial instance" })
      .closest("li");
    const welcome = screen
      .getByRole("heading", { name: "Welcome & prerequisites" })
      .closest("li");
    expect(materialize).toHaveAttribute("data-state", "done");
    expect(init).toHaveAttribute("data-state", "done");
    expect(welcome).toHaveAttribute("data-state", "active");
  });

  it("advances the welcome step and persists it for the session", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({ "/guided/state": () => ({ body: guidedState() }) })}
      />,
    );

    await user.click(await screen.findByRole("button", { name: "Start the walkthrough" }));

    expect(
      screen.getByRole("heading", { name: "Welcome & prerequisites" }).closest("li"),
    ).toHaveAttribute("data-state", "done");
    expect(window.sessionStorage.getItem("goobers-guided-welcome-done")).toBe("true");
  });

  it("keeps Node.js/npm out of the universal welcome checklist and delegates own-repo prerequisites", async () => {
    render(
      <GettingStartedPage
        client={clientWith({ "/guided/state": () => ({ body: guidedState() }) })}
      />,
    );

    const welcome = (
      await screen.findByRole("heading", { name: "Welcome & prerequisites" })
    ).closest("li");
    if (!welcome) {
      throw new Error("Welcome step did not render as a list item.");
    }
    expect(within(welcome).queryByText(/Node\.js/)).toBeNull();
    expect(within(welcome).queryByText(/npm/)).toBeNull();
    expect(
      within(welcome).getByText(/Its own workflow determines any further tooling it needs/),
    ).toBeInTheDocument();
  });

  it("surfaces the sample's Node.js/npm prerequisite only on the sample branch, softened", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({ "/guided/state": () => ({ body: guidedState() }) })}
      />,
    );

    await chooseSample(user);

    const materialize = (
      await screen.findByRole("heading", { name: "Materialize the sample" })
    ).closest("li");
    if (!materialize) {
      throw new Error("Materialize step did not render as a list item.");
    }
    expect(within(materialize).getByText(/Node\.js >= 20 and npm on/)).toBeInTheDocument();
    expect(
      within(materialize).getByText(/Nothing here enforces that for you/),
    ).toBeInTheDocument();
  });

  it("renders pending seed entries as pending with an explanation, not as errors", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/stub-sample": () => ({
            body: {
              exitCode: 0,
              envelope: {
                action: "stub-sample",
                version: 2,
                created: ["src/server.js"],
                skipped: [
                  "package.json",
                  "issue:TASK-9 (pending: GOOBERS_GITHUB_ISSUES_TOKEN unset)",
                ],
              },
              stderr: "",
            },
          }),
        })}
      />,
    );

    await chooseSample(user);
    await user.click(await screen.findByRole("button", { name: "Materialize the sample" }));

    expect(await screen.findByText("issue:TASK-9")).toBeInTheDocument();
    expect(
      screen.getByText("pending: GOOBERS_GITHUB_ISSUES_TOKEN unset"),
    ).toBeInTheDocument();
    expect(screen.getByText(/Pending is not an error/)).toBeInTheDocument();
    expect(screen.getByText("src/server.js")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("surfaces a stub-sample conflict and only then offers an explicit --force re-run", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/stub-sample": () => ({
            body: {
              exitCode: 1,
              envelope: null,
              stderr: "error: conflicting file package.json",
            },
          }),
        })}
      />,
    );

    await chooseSample(user);
    expect(await screen.findByRole("button", { name: "Materialize the sample" })).toBeVisible();
    expect(screen.queryByText(/Re-run with/)).toBeNull();

    await user.click(screen.getByRole("button", { name: "Materialize the sample" }));

    expect(await screen.findByText(/conflicting file package\.json/)).toBeInTheDocument();
    const force = screen.getByRole("checkbox", {
      name: /Re-run with --force/,
    });
    expect(force).not.toBeChecked();
  });

  it("presents the manual repo-creation, push, and placeholder steps explicitly", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({ "/guided/state": () => ({ body: guidedState() }) })}
      />,
    );

    await chooseSample(user);
    expect(
      await screen.findByRole("heading", {
        name: "Create the disposable GitHub repo & push",
      }),
    ).toBeInTheDocument();
    expect(screen.getByText(/gh repo create/)).toBeInTheDocument();
    expect(screen.getByText(/push -u origin main/)).toBeInTheDocument();
    expect(
      screen.getByText(/Goobers never creates remotes, never pushes/),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Point it at your repo" })).toBeInTheDocument();
    expect(screen.getByText("tutorial-instance/instance.yaml")).toBeInTheDocument();
    expect(
      screen.getByText("tutorial-instance/config/gaggles/example/gaggle.yaml"),
    ).toBeInTheDocument();
  });

  it("renders validation findings in a table and an all-clear on exit 0", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState({ instanceExists: true }) }),
          "/guided/actions/validate": () => ({
            body: {
              exitCode: 0,
              envelope: {
                ok: true,
                counts: { errors: 0, warnings: 1 },
                findings: [
                  {
                    file: "config/gaggles/example/gaggle.yaml",
                    code: "GBO002",
                    severity: "warning",
                    message: "placeholder owner",
                  },
                ],
              },
              stderr: "",
            },
          }),
        })}
      />,
    );

    await chooseSample(user);
    await user.click(await screen.findByRole("button", { name: "Run the checks" }));

    expect(await screen.findByText(/All systems go/)).toBeInTheDocument();
    const table = screen.getByRole("table", { name: "Validation findings" });
    expect(table).toHaveTextContent("GBO002");
    expect(table).toHaveTextContent("placeholder owner");
  });

  it("shows the run link and success readout once the run job finishes", async () => {
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({
              sampleExists: true,
              instanceExists: true,
              job: {
                id: "job-1",
                kind: "run",
                done: true,
                exitCode: 0,
                runId: "01JZGUIDEDRUN",
              },
            }),
          }),
          "/guided/jobs/job-1": () => ({
            body: {
              id: "job-1",
              kind: "run",
              done: true,
              exitCode: 0,
              runId: "01JZGUIDEDRUN",
              output: [
                "created run 01JZGUIDEDRUN (workflow=quickstart gaggle=example)",
                "stage implement started (run=01JZGUIDEDRUN, attempt=1, elapsed=1s)",
                "stage implement finished (run=01JZGUIDEDRUN, attempt=1, status=success, elapsed=9s)",
                "stage open-pr started (run=01JZGUIDEDRUN, attempt=1, elapsed=10s)",
                "stage open-pr finished (run=01JZGUIDEDRUN, attempt=1, status=success, elapsed=11s)",
              ],
            },
          }),
          "/guided/status": () => ({
            body: {
              exitCode: 0,
              envelope: {
                timeToFirstPR: { anchor: "initCompletedAt", milliseconds: 480_000 },
              },
              stderr: "",
            },
          }),
        })}
      />,
    );

    expect(
      await screen.findByRole("link", { name: "Watch run 01JZGUIDEDRUN live →" }),
    ).toHaveAttribute("href", "#/run/01JZGUIDEDRUN");
    expect(
      await screen.findByText(/Your first autonomous PR opened in 8 minutes\./),
    ).toBeInTheDocument();
    const implement = await screen.findByText("implement", { selector: ".guided-stage" });
    expect(implement).toHaveAttribute("data-state", "done");
    const openPr = await screen.findByText("open-pr", { selector: ".guided-stage" });
    expect(openPr).toHaveAttribute("data-state", "done");
  });

  // #2638: a run that exits 0 but never reaches the open-pr stage found no
  // eligible backlog item — the wizard must say so, not claim success.
  it("renders the no-eligible-issues state instead of Success when the run never reaches open-pr", async () => {
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({
              sampleExists: true,
              instanceExists: true,
              job: {
                id: "job-nowork",
                kind: "run",
                done: true,
                exitCode: 0,
                runId: "01JZNOWORKRUN",
              },
            }),
          }),
          "/guided/jobs/job-nowork": () => ({
            body: {
              id: "job-nowork",
              kind: "run",
              done: true,
              exitCode: 0,
              runId: "01JZNOWORKRUN",
              output: [
                "created run 01JZNOWORKRUN (workflow=quickstart gaggle=example)",
                "stage query-backlog started (run=01JZNOWORKRUN, attempt=1, elapsed=1s)",
                "no work: no eligible items",
                "stage query-backlog finished (run=01JZNOWORKRUN, attempt=1, status=success, elapsed=1s)",
              ],
            },
          }),
          "/guided/actions/probe-backlog": () => ({
            body: { exitCode: 0, eligibleCount: 0, stderr: "" },
          }),
        })}
      />,
    );

    expect(await screen.findByText(/No eligible issues found\./)).toBeInTheDocument();
    expect(
      screen.queryByText(/Your first autonomous (PR opened|run finished)/),
    ).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Re-run" })).toBeInTheDocument();
  });

  it("warns before the run when the pre-run probe finds no eligible issues", async () => {
    const user = userEvent.setup();
    let probeCalls = 0;
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({ sampleExists: true, instanceExists: true }),
          }),
          "/guided/actions/validate": () => ({
            body: { exitCode: 0, envelope: { ok: true, counts: { errors: 0, warnings: 0 } }, stderr: "" },
          }),
          "/guided/actions/probe-backlog": () => {
            probeCalls += 1;
            return { body: { exitCode: 0, eligibleCount: 0, stderr: "" } };
          },
        })}
      />,
    );

    await chooseSample(user);
    await user.click(screen.getByRole("button", { name: "Run the checks" }));

    expect(await screen.findByText(/0 eligible issues found\./)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Check again" })).toBeInTheDocument();
    expect(probeCalls).toBeGreaterThan(0);
  });

  it("reports eligible issues found by the pre-run probe without a warning", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({ sampleExists: true, instanceExists: true }),
          }),
          "/guided/actions/validate": () => ({
            body: { exitCode: 0, envelope: { ok: true, counts: { errors: 0, warnings: 0 } }, stderr: "" },
          }),
          "/guided/actions/probe-backlog": () => ({
            body: { exitCode: 0, eligibleCount: 2, stderr: "" },
          }),
        })}
      />,
    );

    await chooseSample(user);
    await user.click(screen.getByRole("button", { name: "Run the checks" }));

    expect(await screen.findByText(/2 eligible issues found — ready to run\./)).toBeInTheDocument();
    expect(screen.queryByText(/0 eligible issues found\./)).toBeNull();
  });

  it("walks the own-repo branch and sends the exact action payloads", async () => {
    const user = userEvent.setup();
    const initBodies: unknown[] = [];
    const connectBodies: unknown[] = [];
    const runBodies: unknown[] = [];
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState() }),
          "/guided/actions/init-instance": (init) => {
            initBodies.push(parseBody(init));
            return { body: { exitCode: 0, stdout: "initialized\n", stderr: "" } };
          },
          "/guided/actions/connect": (init) => {
            connectBodies.push(parseBody(init));
            return {
              body: {
                exitCode: 0,
                envelope: {
                  action: "connect",
                  version: 2,
                  created: ["label:goobers", "issue:hello-goobers"],
                  updated: ["instance.yaml", "config/gaggles/example/gaggle.yaml"],
                  skipped: [],
                  path: "/work/tutorial-instance",
                  nextCommand: "goobers run default-implement '/work/tutorial-instance'",
                },
                stderr: "",
              },
            };
          },
          "/guided/actions/run": (init) => {
            runBodies.push(parseBody(init));
            return { status: 202, body: { jobId: "job-a" } };
          },
          "/guided/jobs/job-a": () => ({
            body: {
              id: "job-a",
              kind: "run",
              done: true,
              exitCode: 0,
              runId: "01JZOWNREPORUN",
              output: [
                "created run 01JZOWNREPORUN (workflow=default-implement gaggle=starter)",
                "stage push-branch started (run=01JZOWNREPORUN, attempt=1, elapsed=1s)",
                "stage push-branch finished (run=01JZOWNREPORUN, attempt=1, status=success, elapsed=2s)",
              ],
            },
          }),
          "/guided/status": () => ({
            body: { exitCode: 0, envelope: {}, stderr: "" },
          }),
        })}
      />,
    );

    await chooseOwnRepo(user);

    // Step A1: initialize the starter instance.
    await user.click(
      await screen.findByRole("button", { name: "Initialize the starter instance" }),
    );
    expect(initBodies).toEqual([{ template: "starter" }]);

    // Step A2: connect, with a custom token-env name and seeding on by default.
    const seed = screen.getByRole("checkbox", { name: /Seed the backlog/ });
    expect(seed).toBeChecked();
    const connectButton = screen.getByRole("button", { name: "Connect the repository" });
    expect(connectButton).toBeDisabled();
    await user.type(
      screen.getByRole("textbox", { name: "Repository (owner/repo)" }),
      "acme/widgets",
    );
    const tokenEnv = screen.getByRole("textbox", {
      name: /Token environment variable/,
    });
    await user.clear(tokenEnv);
    await user.type(tokenEnv, "MY_GH_TOKEN");
    expect(connectButton).toBeEnabled();
    await user.click(connectButton);
    expect(connectBodies).toEqual([
      { repo: "acme/widgets", tokenEnv: "MY_GH_TOKEN", seed: true },
    ]);
    expect(
      await screen.findByText("config/gaggles/example/gaggle.yaml"),
    ).toBeInTheDocument();

    // Step A5: run default-implement.
    await user.click(screen.getByRole("button", { name: "Start the run" }));
    expect(runBodies).toEqual([{ workflow: "default-implement" }]);
    const pushBranch = await screen.findByText("push-branch", { selector: ".guided-stage" });
    expect(pushBranch).toHaveAttribute("data-state", "done");
  });

  it("renders connect pending-credential entries as pending, and offers --replace only after a refusal", async () => {
    const user = userEvent.setup();
    let refuse = true;
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({ body: guidedState({ instanceExists: true }) }),
          "/guided/actions/connect": () => {
            if (refuse) {
              return {
                body: {
                  exitCode: 1,
                  envelope: null,
                  stderr: "refusing to overwrite existing repository acme/old (use --replace)",
                },
              };
            }
            return {
              body: {
                exitCode: 0,
                envelope: {
                  action: "connect",
                  version: 2,
                  created: ["label:goobers"],
                  updated: ["instance.yaml"],
                  skipped: ["issue:hello-goobers (pending: credentials unavailable)"],
                },
                stderr: "",
              },
            };
          },
        })}
      />,
    );

    await chooseOwnRepo(user);
    expect(screen.queryByText(/Re-run with/)).toBeNull();
    await user.type(
      await screen.findByRole("textbox", { name: "Repository (owner/repo)" }),
      "acme/widgets",
    );
    await user.click(screen.getByRole("button", { name: "Connect the repository" }));

    // Refusal: stderr surfaces, and only now is --replace offered, unchecked.
    expect(await screen.findByText(/refusing to overwrite/)).toBeInTheDocument();
    const replace = screen.getByRole("checkbox", { name: /Re-run with --replace/ });
    expect(replace).not.toBeChecked();

    refuse = false;
    await user.click(replace);
    await user.click(screen.getByRole("button", { name: "Connect the repository" }));

    expect(await screen.findByText("issue:hello-goobers")).toBeInTheDocument();
    expect(screen.getByText("pending: credentials unavailable")).toBeInTheDocument();
    expect(screen.getByText(/Pending is not an error/)).toBeInTheDocument();
    expect(screen.getByText("label:goobers")).toBeInTheDocument();
    // The rewritten files render under an "Updated" kicker.
    expect(screen.getByText("Updated")).toBeInTheDocument();
  });

  it("drives the connect step's done state from the server's connected repo", async () => {
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({
              instanceExists: true,
              connected: { repo: "acme/widgets" },
            }),
          }),
        })}
      />,
    );

    // A connected repo infers the own-repo path with nothing stored.
    const connect = (
      await screen.findByRole("heading", { name: "Connect your repository", level: 2 })
    ).closest("li");
    expect(connect).toHaveAttribute("data-state", "done");
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    const init = screen
      .getByRole("heading", { name: "Initialize a starter instance" })
      .closest("li");
    expect(init).toHaveAttribute("data-state", "done");
  });

  it("preserves server-truth step state across a branch switch", async () => {
    const user = userEvent.setup();
    render(
      <GettingStartedPage
        client={clientWith({
          "/guided/state": () => ({
            body: guidedState({ sampleExists: true, instanceExists: true }),
          }),
        })}
      />,
    );

    // Inferred sample branch; switch to own-repo.
    expect(
      await screen.findByRole("heading", { name: "Materialize the sample" }),
    ).toBeInTheDocument();
    await chooseOwnRepo(user);

    expect(screen.queryByRole("heading", { name: "Materialize the sample" })).toBeNull();
    const init = (
      await screen.findByRole("heading", { name: "Initialize a starter instance" })
    ).closest("li");
    expect(init).toHaveAttribute("data-state", "done");
    expect(window.sessionStorage.getItem("goobers-guided-path")).toBe("own-repo");

    // And back: the sample branch's server-attested steps are still done.
    await chooseSample(user);
    const materialize = (
      await screen.findByRole("heading", { name: "Materialize the sample" })
    ).closest("li");
    expect(materialize).toHaveAttribute("data-state", "done");
  });
});
