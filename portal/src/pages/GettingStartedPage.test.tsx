import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
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
    env: { goobersGithubToken: false, goobersGithubIssuesToken: false },
    job: null,
    apiReady: false,
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

describe("GettingStartedPage", () => {
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
              env: { goobersGithubToken: true, goobersGithubIssuesToken: false },
            }),
          }),
        })}
      />,
    );

    const badges = await screen.findByLabelText("Token environment status");
    expect(badges).toHaveTextContent("GOOBERS_GITHUB_TOKEN set");
    expect(badges).toHaveTextContent("GOOBERS_GITHUB_ISSUES_TOKEN not set");
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
    render(
      <GettingStartedPage
        client={clientWith({ "/guided/state": () => ({ body: guidedState() }) })}
      />,
    );

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
  });
});
