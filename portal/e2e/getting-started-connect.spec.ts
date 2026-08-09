import { expect, test, type Page, type Route } from "@playwright/test";
import { defaultPortalConfig } from "../src/cobrand";

// The guided Getting Started walkthrough's own-repo branch (#2449): path
// chooser → starter init → connect your repository → export the token →
// validate → one default-implement run → success readout. The spec drives the
// page by fulfilling /guided/* directly, mirroring getting-started.spec.ts.

const runId = "01JZCONNECTRUN";
const jobId = "guided-job-connect";
const workdir = "/work";
const instancePath = `${workdir}/tutorial-instance`;
const repo = "acme/widgets";

interface ConnectFixture {
  instanceExists: boolean;
  connectedRepo: string | null;
  jobStarted: boolean;
}

function guidedState(fixture: ConnectFixture) {
  return {
    version: 1,
    workdir,
    samplePath: `${workdir}/getting-started-task-api`,
    instancePath,
    sampleExists: false,
    instanceExists: fixture.instanceExists,
    env: {
      tokenEnv: "GOOBERS_GITHUB_TOKEN",
      goobersGithubToken: true,
      goobersGithubIssuesToken: false,
    },
    job: fixture.jobStarted ? jobDetail() : null,
    apiReady: fixture.instanceExists,
    connected: { repo: fixture.connectedRepo },
  };
}

function jobDetail() {
  return {
    id: jobId,
    kind: "run",
    done: true,
    exitCode: 0,
    runId,
    output: [
      `created run ${runId} (workflow=default-implement gaggle=starter)`,
      `stage query-backlog started (run=${runId}, attempt=1, elapsed=0s)`,
      `stage query-backlog finished (run=${runId}, attempt=1, status=success, elapsed=1s)`,
      `stage implement started (run=${runId}, attempt=1, elapsed=1s)`,
      `stage implement finished (run=${runId}, attempt=1, status=success, elapsed=80s)`,
      `stage push-branch started (run=${runId}, attempt=1, elapsed=81s)`,
      `stage push-branch finished (run=${runId}, attempt=1, status=success, elapsed=85s)`,
      `stage open-pr started (run=${runId}, attempt=1, elapsed=86s)`,
      `stage open-pr finished (run=${runId}, attempt=1, status=success, elapsed=95s)`,
    ],
  };
}

async function routeGuided(page: Page, fixture: ConnectFixture): Promise<void> {
  // Match on the URL path, not a "**/guided/**" glob: the vite dev server
  // itself serves /src/guided/client.ts as a module, which such a glob would
  // also intercept and corrupt.
  await page.route(
    (url) => url.pathname.startsWith("/guided/"),
    async (route: Route) => {
      const request = route.request();
      const path = new URL(request.url()).pathname;
      if (path === "/guided/state") {
        await route.fulfill({ json: guidedState(fixture) });
        return;
      }
      if (path === "/guided/actions/init-instance" && request.method() === "POST") {
        expect(request.postDataJSON()).toEqual({ template: "starter" });
        fixture.instanceExists = true;
        await route.fulfill({
          json: { exitCode: 0, stdout: `initialized ${instancePath}\n`, stderr: "" },
        });
        return;
      }
      if (path === "/guided/actions/connect" && request.method() === "POST") {
        expect(request.postDataJSON()).toEqual({ repo, seed: true });
        fixture.connectedRepo = repo;
        await route.fulfill({
          json: {
            exitCode: 0,
            envelope: {
              action: "connect",
              version: 2,
              created: ["label:goobers"],
              updated: ["instance.yaml", "config/gaggles/example/gaggle.yaml"],
              skipped: ["issue:hello-goobers (pending: credentials unavailable)"],
              path: instancePath,
              nextCommand: `goobers validate --check-harness --check-repos '${instancePath}'`,
            },
            stderr: "",
          },
        });
        return;
      }
      if (path === "/guided/actions/validate" && request.method() === "POST") {
        await route.fulfill({
          json: {
            exitCode: 0,
            envelope: { ok: true, counts: { errors: 0, warnings: 0 }, findings: [] },
            stderr: "",
          },
        });
        return;
      }
      if (path === "/guided/actions/run" && request.method() === "POST") {
        expect(request.postDataJSON()).toEqual({ workflow: "default-implement" });
        fixture.jobStarted = true;
        await route.fulfill({ status: 202, json: { jobId } });
        return;
      }
      if (path === `/guided/jobs/${jobId}`) {
        await route.fulfill({ json: jobDetail() });
        return;
      }
      if (path === "/guided/status") {
        await route.fulfill({
          json: {
            exitCode: 0,
            envelope: {
              timeToFirstPR: { anchor: "initCompletedAt", milliseconds: 540_000 },
            },
            stderr: "",
          },
        });
        return;
      }
      await route.fulfill({
        status: 500,
        json: { code: "unexpected_test_request", message: path },
      });
    },
  );
}

async function routeDaemonAPI(page: Page): Promise<void> {
  let eventSequence = 0;
  await page.route("**/api/v1/**", async (route: Route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/events") {
      eventSequence += 1;
      await new Promise((resolve) => setTimeout(resolve, 1_000));
      const cursor = `browser:${eventSequence}`;
      await route.fulfill({
        body: `id: ${cursor}\nevent: invalidate\ndata: ${JSON.stringify({
          cursor,
          models: [],
          runIds: [],
          workflows: [],
        })}\n\n`,
        contentType: "text/event-stream",
      });
      return;
    }
    if (path === "/api/v1/portal/config") {
      await route.fulfill({ json: defaultPortalConfig });
      return;
    }
    await route.fulfill({
      status: 500,
      json: { error: { code: "unexpected_test_request", message: path } },
    });
  });
}

test("walks the own-repo branch from the chooser to the success readout", async ({ page }) => {
  const fixture: ConnectFixture = {
    instanceExists: false,
    connectedRepo: null,
    jobStarted: false,
  };
  await routeGuided(page, fixture);
  await routeDaemonAPI(page);

  await page.goto("/#/getting-started");
  await expect(page.getByRole("heading", { name: "Getting Started" })).toBeVisible();
  await page.getByRole("button", { name: "Start the walkthrough" }).click();

  // The path chooser: the own-repo card is first and recommended.
  const chooser = page.getByRole("group", { name: "Path chooser" });
  await expect(chooser.getByRole("button").first()).toContainText("Connect your repository");
  await expect(chooser.getByRole("button").first()).toContainText("Recommended");
  await chooser.getByRole("button", { name: /Connect your repository/ }).click();

  // Step A1: initialize the starter instance (asserted argv template=starter).
  await page.getByRole("button", { name: "Initialize the starter instance" }).click();
  await expect(page.getByText(/single source\s+of truth/)).toBeVisible();

  // Step A2: connect the repository; seeding is on by default, and the
  // pending-credential starter issue renders as pending, not as an error.
  await page.getByRole("textbox", { name: "Repository (owner/repo)" }).fill(repo);
  await expect(page.getByRole("checkbox", { name: /Seed the backlog/ })).toBeChecked();
  await page.getByRole("button", { name: "Connect the repository" }).click();
  await expect(page.getByText("label:goobers")).toBeVisible();
  await expect(page.getByText("pending: credentials unavailable")).toBeVisible();
  await expect(page.getByText("Connected to")).toBeVisible();

  // Step A3 is manual: the export chip and the live badge.
  const exportStep = page.locator("li.guided-step", {
    has: page.getByRole("heading", { name: "Export the token" }),
  });
  await expect(exportStep.locator(".recovery-action code")).toHaveText(
    "export GOOBERS_GITHUB_TOKEN=...",
  );
  await page.getByRole("checkbox", { name: "I exported the token" }).check();

  // Step A4: validate.
  await page.getByRole("button", { name: "Run the checks" }).click();
  await expect(page.getByText("All systems go.")).toBeVisible();

  // Step A5: run default-implement, poll the job, watch the stage chips.
  await page.getByRole("button", { name: "Start the run" }).click();
  await expect(page.getByRole("link", { name: `Watch run ${runId} live →` })).toHaveAttribute(
    "href",
    `#/run/${runId}`,
  );
  await expect(page.locator(".guided-stage", { hasText: "push-branch" })).toHaveAttribute(
    "data-state",
    "done",
  );

  // Step A6: the local Time-to-First-PR readout and own-repo next steps.
  await expect(page.getByText("Your first autonomous PR opened in 9 minutes.")).toBeVisible();
  await expect(page.getByText(/Label more of your issues/)).toBeVisible();
  await expect(page.getByRole("link", { name: `Revisit run ${runId} →` })).toBeVisible();
});
