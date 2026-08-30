import { expect, test, type Page, type Route } from "@playwright/test";
import { defaultPortalConfig } from "../src/cobrand";

const workdir = "C:\\work";
const instancePath = `${workdir}\\tutorial-instance`;
const configPath = `${workdir}\\tutorial-instance-config`;
const runId = "01JZCONNECTRUN";
const jobId = "guided-job-connect";

interface Fixture {
  instanceExists: boolean;
  jobStarted: boolean;
}

function state(fixture: Fixture) {
  return {
    version: 2,
    platform: "windows",
    workdir,
    instancePath,
    configPath,
    suggestedStack: "Node.js",
    suggestedCICommand: ["npm", "run", "ci"],
    suggestedCapability: "node@20",
    instanceExists: fixture.instanceExists,
    env: {
      tokenEnv: "GOOBERS_GITHUB_REPO_TOKEN",
      goobersGithubToken: true,
      goobersGithubIssuesToken: true,
    },
    job: null,
    apiReady: fixture.instanceExists,
    connected: { repo: fixture.instanceExists ? "acme/widgets" : null },
  };
}

async function routeGuided(page: Page, fixture: Fixture) {
  await page.route(
    (url) => url.pathname.startsWith("/guided/"),
    async (route: Route) => {
      const request = route.request();
      const path = new URL(request.url()).pathname;
      if (path === "/guided/state") {
        await route.fulfill({ json: state(fixture) });
        return;
      }
      if (path === "/guided/actions/inspect-repository") {
        await route.fulfill({
          json: {
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
            auth: { kind: "github-cli", ready: true, identity: "octocat" },
          },
        });
        return;
      }
      if (path === "/guided/actions/init-instance") {
        expect(request.postDataJSON()).toEqual({
          template: "guided",
          guided: {
            provider: "github",
            owner: "acme",
            name: "widgets",
            localPath: "C:\\src\\widgets",
            configPath: "C:\\src\\widgets-goobers",
            branch: "trunk",
            workflows: ["work-nomination", "backlog-curation", "implementation"],
            issueScope: "all",
            ciCommand: ["npm", "run", "ci"],
            requiredCapabilities: ["node@20"],
            harness: "copilot",
            repoTokenEnv: "GOOBERS_GITHUB_REPO_TOKEN",
            workTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
            githubCLIUser: "octocat",
            pullRequestTokenEnv: "GOOBERS_GITHUB_PR_TOKEN",
            repoPushTokenEnv: "GOOBERS_GITHUB_PUSH_TOKEN",
          },
        });
        fixture.instanceExists = true;
        await route.fulfill({
          json: {
            exitCode: 0,
            stdout: `Created ${instancePath} with 3 workflow module(s) from ${configPath}.`,
            stderr: "",
          },
        });
        return;
      }
      if (path === "/guided/actions/validate") {
        await route.fulfill({
          json: {
            exitCode: 0,
            envelope: { ok: true, counts: { errors: 0, warnings: 0 }, findings: [] },
            stderr: "",
          },
        });
        return;
      }
      if (path === "/guided/actions/prepare-repository") {
        await route.fulfill({
          json: {
            provider: "github",
            repository: "acme/widgets",
            selectorLabels: ["goobers:approved", "goobers:ready"],
            lifecycleLabels: ["goobers:claimed", "goobers/status:in-review"],
            missingLabels: [],
            eligibleCount: 1,
          },
        });
        return;
      }
      if (path === "/guided/actions/run") {
        expect(request.postDataJSON()).toEqual({ workflow: "implementation" });
        fixture.jobStarted = true;
        await route.fulfill({ status: 202, json: { jobId } });
        return;
      }
      if (path === `/guided/jobs/${jobId}`) {
        await route.fulfill({
          json: {
            id: jobId,
            kind: "run",
            done: true,
            exitCode: 0,
            runId,
            output: [
              `stage query-backlog started (run=${runId})`,
              `stage query-backlog finished (run=${runId})`,
              `stage open-pr started (run=${runId})`,
              `stage open-pr finished (run=${runId})`,
            ],
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

async function routeDaemon(page: Page) {
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/portal/config") {
      await route.fulfill({ json: defaultPortalConfig });
      return;
    }
    if (path === "/api/v1/events") {
      await route.fulfill({
        body: "id: browser:1\nevent: invalidate\ndata: {\"cursor\":\"browser:1\",\"models\":[],\"runIds\":[],\"workflows\":[]}\n\n",
        contentType: "text/event-stream",
      });
      return;
    }
    await route.fulfill({
      status: 500,
      json: { error: { code: "unexpected_test_request", message: path } },
    });
  });
}

test("configures a repository through the multi-page guided wizard", async ({ page }) => {
  const fixture: Fixture = { instanceExists: false, jobStarted: false };
  await routeGuided(page, fixture);
  await routeDaemon(page);

  await page.goto("/?mode=getting-started");
  await expect(page.getByRole("heading", { name: "Welcome to Goobers" })).toBeVisible();
  await expect(page.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "1");

  await page.getByRole("button", { name: "Continue" }).click();

  await page.getByRole("textbox", { name: "Local clone" }).fill("C:\\src\\widgets");
  await page.getByRole("button", { name: "Inspect clone" }).click();
  await expect(page.getByText("GitHub CLI authentication is ready as octocat.")).toBeVisible();
  await expect(page.getByText("npm run ci")).toBeVisible();
  await page.getByRole("textbox", { name: "Local clone" }).press("Enter");
  await expect(page.getByRole("heading", { name: "Choose where Instance Configuration lives" })).toBeVisible();
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByRole("heading", { name: "Set up your first gaggle" })).toBeVisible();
  await page.getByRole("button", { name: "Continue" }).click();

  await page.getByRole("button", { name: "Continue" }).click();

  await page.getByRole("button", { name: "Create Goobers instance" }).click();
  await expect(page.getByText(/Created C:\\work\\tutorial-instance/)).toBeVisible();
  await page.getByRole("button", { name: "Continue" }).click();

  await page.getByRole("button", { name: "Check labels and ready issues" }).click();
  await expect(page.getByText(/eligible work for the first run/)).toBeVisible();
  await page.getByRole("button", { name: "Continue" }).click();

  await page.getByRole("button", { name: "Run checks" }).click();
  await expect(page.getByText(/All configuration, harness, and repository checks passed/)).toBeVisible();
  await page.getByRole("button", { name: "Continue" }).click();

  await page.getByRole("button", { name: "Start run" }).click();
  await expect(page.getByText(`Run ${runId}`)).toBeVisible();
  await expect(page.getByText("The workflow completed successfully.")).toBeVisible();

  await page.getByRole("button", { name: "Back" }).click();
  await expect(page.getByRole("heading", { name: "Check the setup" })).toBeVisible();
});
