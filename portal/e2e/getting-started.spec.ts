import { expect, test, type Page, type Route } from "@playwright/test";
import { defaultPortalConfig } from "../src/cobrand";

// The guided Getting Started walkthrough (#437), happy path. The dev server
// serves the daemon-mode index; the page renders the interactive wizard
// whenever /guided/state answers OK (the meta mode marker only chooses the
// default route), so the spec drives it by fulfilling /guided/* directly.

const runId = "01JZGUIDEDRUN";
const jobId = "guided-job-1";
const workdir = "/work";
const samplePath = `${workdir}/getting-started-task-api`;
const instancePath = `${workdir}/tutorial-instance`;

interface GuidedFixture {
  sampleExists: boolean;
  instanceExists: boolean;
  jobStarted: boolean;
}

function guidedState(fixture: GuidedFixture) {
  return {
    version: 1,
    workdir,
    samplePath,
    instancePath,
    sampleExists: fixture.sampleExists,
    instanceExists: fixture.instanceExists,
    env: {
      tokenEnv: "GOOBERS_GITHUB_TOKEN",
      goobersGithubToken: true,
      goobersGithubIssuesToken: false,
    },
    job: fixture.jobStarted ? jobDetail() : null,
    apiReady: fixture.instanceExists,
    connected: { repo: null },
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
      `created run ${runId} (workflow=quickstart gaggle=example)`,
      `stage query-backlog started (run=${runId}, attempt=1, elapsed=0s)`,
      `stage query-backlog finished (run=${runId}, attempt=1, status=success, elapsed=1s)`,
      `stage implement started (run=${runId}, attempt=1, elapsed=1s)`,
      `stage implement finished (run=${runId}, attempt=1, status=success, elapsed=90s)`,
      `stage open-pr started (run=${runId}, attempt=1, elapsed=200s)`,
      `stage open-pr finished (run=${runId}, attempt=1, status=success, elapsed=210s)`,
    ],
  };
}

async function routeGuided(page: Page, fixture: GuidedFixture): Promise<void> {
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
    if (path === "/guided/actions/stub-sample" && request.method() === "POST") {
      fixture.sampleExists = true;
      await route.fulfill({
        json: {
          exitCode: 0,
          envelope: {
            action: "stub-sample",
            version: 2,
            created: ["src/server.js", "package.json"],
            skipped: ["issue:TASK-9 (pending: GOOBERS_GITHUB_ISSUES_TOKEN unset)"],
            path: samplePath,
            nextCommand: "goobers init --template=quickstart ./tutorial-instance",
          },
          stderr: "",
        },
      });
      return;
    }
    if (path === "/guided/actions/init-instance" && request.method() === "POST") {
      fixture.instanceExists = true;
      await route.fulfill({
        json: { exitCode: 0, stdout: `initialized ${instancePath}\n`, stderr: "" },
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
            timeToFirstPR: { anchor: "initCompletedAt", milliseconds: 480_000 },
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

test("walks the guided happy path from welcome to the success readout", async ({ page }) => {
  const fixture: GuidedFixture = {
    sampleExists: false,
    instanceExists: false,
    jobStarted: false,
  };
  await routeGuided(page, fixture);
  await routeDaemonAPI(page);

  await page.goto("/#/getting-started");
  await expect(page.getByRole("heading", { name: "Getting Started" })).toBeVisible();
  await expect(page.getByText("GOOBERS_GITHUB_TOKEN set")).toBeVisible();
  await page.getByRole("button", { name: "Start the walkthrough" }).click();

  // Step 2: the path chooser — the own-repo card leads; this spec takes the
  // disposable-sample branch.
  await expect(
    page.getByRole("button", { name: /Connect your repository/ }),
  ).toBeVisible();
  await page.getByRole("button", { name: /Try the disposable sample/ }).click();

  // Step 3: materialize the sample; pending seeds render as pending, not errors.
  await page.getByRole("button", { name: "Materialize the sample" }).click();
  await expect(page.getByText("src/server.js")).toBeVisible();
  await expect(
    page.getByText("pending: GOOBERS_GITHUB_ISSUES_TOKEN unset"),
  ).toBeVisible();

  // Step 4 is manual: the repo-creation commands are printed, never run.
  await expect(page.getByText("gh repo create <owner>/<repo> --private")).toBeVisible();
  await page
    .getByRole("checkbox", { name: "I created the repository and pushed the sample" })
    .check();

  // Step 5: initialize the tutorial instance.
  await page.getByRole("button", { name: "Initialize the instance" }).click();
  await expect(page.getByText(/single source\s+of truth/)).toBeVisible();

  // Step 6 is manual: placeholder edits are listed explicitly.
  await page
    .getByRole("checkbox", { name: "I edited the placeholders and exported the token" })
    .check();

  // Step 7: validate.
  await page.getByRole("button", { name: "Run the checks" }).click();
  await expect(page.getByText("All systems go.")).toBeVisible();

  // Step 8: run, poll the job, and land on the live run link.
  await page.getByRole("button", { name: "Start the run" }).click();
  await expect(page.getByRole("link", { name: `Watch run ${runId} live →` })).toHaveAttribute(
    "href",
    `#/run/${runId}`,
  );
  await expect(page.getByText(`created run ${runId}`)).toBeVisible();

  // Step 9: the local Time-to-First-PR readout.
  await expect(page.getByText("Your first autonomous PR opened in 8 minutes.")).toBeVisible();
  await expect(page.getByRole("link", { name: `Revisit run ${runId} →` })).toBeVisible();
});
