import { expect, test, type Locator, type Page } from "@playwright/test";

// A signed-out GitHub inspection renders the tallest repository step: the
// authentication callout, PAT steps, and the sign-in action sit below the fold.
const signedOutInspection = {
  provider: "github",
  owner: "acme",
  name: "widgets",
  displayName: "acme/widgets",
  gaggleName: "widgets",
  localPath: "C:\\src\\widgets",
  defaultBranch: "main",
  stack: "Node.js",
  ciCommand: ["npm", "run", "ci"],
  requiredCapabilities: ["node@20"],
  discovery: "deterministic",
  evidence: ["package.json: scripts.ci"],
  needsClone: false,
  peerInstancePath: "C:\\src\\widgets-goobers",
  auth: {
    kind: "github-cli",
    ready: false,
    message: "GitHub CLI authentication is required before setup can continue.",
    remediationCommand: "gh auth login --hostname github.com --git-protocol https --web",
    needsLogin: true,
  },
};

async function routeGuided(page: Page) {
  await page.route(
    (url) => url.pathname.startsWith("/guided/"),
    async (route) => {
      const path = new URL(route.request().url()).pathname;
      if (path === "/guided/state") {
        await route.fulfill({
          json: {
            version: 2,
            platform: "windows",
            workdir: "C:\\work",
            instancePath: "C:\\work\\tutorial-instance",
            instanceExists: false,
            env: {
              tokenEnv: "GOOBERS_GITHUB_REPO_TOKEN",
              goobersGithubToken: false,
              goobersGithubIssuesToken: false,
            },
            job: null,
            apiReady: false,
            connected: { repo: null },
          },
        });
        return;
      }
      if (path === "/guided/actions/inspect-repository") {
        await route.fulfill({ json: signedOutInspection });
        return;
      }
      await route.fulfill({ status: 404, json: { code: "not_found", message: path } });
    },
  );
}

async function clearOfFooter(target: Locator, footer: Locator): Promise<boolean> {
  const [box, footerBox] = await Promise.all([target.boundingBox(), footer.boundingBox()]);
  return (
    box !== null &&
    footerBox !== null &&
    box.y >= 0 &&
    box.y + box.height <= footerBox.y
  );
}

test("scrolls a tall setup step to content hidden behind the fixed footer", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 640 });
  await routeGuided(page);

  await page.goto("/?mode=getting-started");
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("textbox", { name: "Local clone" }).fill("C:\\src\\widgets");
  await page.getByRole("button", { name: "Inspect clone" }).click();

  const signIn = page.getByRole("button", { name: "Sign in with GitHub" });
  const footer = page.getByRole("navigation", { name: "Setup progress" });
  await expect(signIn).toBeAttached();
  expect(await clearOfFooter(signIn, footer)).toBe(false);

  await page.mouse.move(640, 320);
  await page.mouse.wheel(0, 1200);

  await expect.poll(() => clearOfFooter(signIn, footer)).toBe(true);
  await expect(page.getByRole("button", { name: "Continue" })).toBeDisabled();
});
