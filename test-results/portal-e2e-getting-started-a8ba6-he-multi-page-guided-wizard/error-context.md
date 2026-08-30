# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: portal\e2e\getting-started-connect.spec.ts >> configures a repository through the multi-page guided wizard
- Location: portal\e2e\getting-started-connect.spec.ts:178:1

# Error details

```
Error: page.goto: Protocol error (Page.navigate): Cannot navigate to invalid URL
Call log:
  - navigating to "/?mode=getting-started", waiting until "load"

```

# Test source

```ts
  83  |             requiredCapabilities: ["node@20"],
  84  |             harness: "copilot",
  85  |             repoTokenEnv: "GOOBERS_GITHUB_REPO_TOKEN",
  86  |             workTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
  87  |             githubCLIUser: "octocat",
  88  |             pullRequestTokenEnv: "GOOBERS_GITHUB_PR_TOKEN",
  89  |             repoPushTokenEnv: "GOOBERS_GITHUB_PUSH_TOKEN",
  90  |           },
  91  |         });
  92  |         fixture.instanceExists = true;
  93  |         await route.fulfill({
  94  |           json: {
  95  |             exitCode: 0,
  96  |             stdout: `Created ${instancePath} with 3 workflow module(s) from ${configPath}.`,
  97  |             stderr: "",
  98  |           },
  99  |         });
  100 |         return;
  101 |       }
  102 |       if (path === "/guided/actions/validate") {
  103 |         await route.fulfill({
  104 |           json: {
  105 |             exitCode: 0,
  106 |             envelope: { ok: true, counts: { errors: 0, warnings: 0 }, findings: [] },
  107 |             stderr: "",
  108 |           },
  109 |         });
  110 |         return;
  111 |       }
  112 |       if (path === "/guided/actions/prepare-repository") {
  113 |         await route.fulfill({
  114 |           json: {
  115 |             provider: "github",
  116 |             repository: "acme/widgets",
  117 |             selectorLabels: ["goobers:approved", "goobers:ready"],
  118 |             lifecycleLabels: ["goobers:claimed", "goobers/status:in-review"],
  119 |             missingLabels: [],
  120 |             eligibleCount: 1,
  121 |           },
  122 |         });
  123 |         return;
  124 |       }
  125 |       if (path === "/guided/actions/run") {
  126 |         expect(request.postDataJSON()).toEqual({ workflow: "implementation" });
  127 |         fixture.jobStarted = true;
  128 |         await route.fulfill({ status: 202, json: { jobId } });
  129 |         return;
  130 |       }
  131 |       if (path === `/guided/jobs/${jobId}`) {
  132 |         await route.fulfill({
  133 |           json: {
  134 |             id: jobId,
  135 |             kind: "run",
  136 |             done: true,
  137 |             exitCode: 0,
  138 |             runId,
  139 |             output: [
  140 |               `stage query-backlog started (run=${runId})`,
  141 |               `stage query-backlog finished (run=${runId})`,
  142 |               `stage open-pr started (run=${runId})`,
  143 |               `stage open-pr finished (run=${runId})`,
  144 |             ],
  145 |           },
  146 |         });
  147 |         return;
  148 |       }
  149 |       await route.fulfill({
  150 |         status: 500,
  151 |         json: { code: "unexpected_test_request", message: path },
  152 |       });
  153 |     },
  154 |   );
  155 | }
  156 | 
  157 | async function routeDaemon(page: Page) {
  158 |   await page.route("**/api/v1/**", async (route) => {
  159 |     const path = new URL(route.request().url()).pathname;
  160 |     if (path === "/api/v1/portal/config") {
  161 |       await route.fulfill({ json: defaultPortalConfig });
  162 |       return;
  163 |     }
  164 |     if (path === "/api/v1/events") {
  165 |       await route.fulfill({
  166 |         body: "id: browser:1\nevent: invalidate\ndata: {\"cursor\":\"browser:1\",\"models\":[],\"runIds\":[],\"workflows\":[]}\n\n",
  167 |         contentType: "text/event-stream",
  168 |       });
  169 |       return;
  170 |     }
  171 |     await route.fulfill({
  172 |       status: 500,
  173 |       json: { error: { code: "unexpected_test_request", message: path } },
  174 |     });
  175 |   });
  176 | }
  177 | 
  178 | test("configures a repository through the multi-page guided wizard", async ({ page }) => {
  179 |   const fixture: Fixture = { instanceExists: false, jobStarted: false };
  180 |   await routeGuided(page, fixture);
  181 |   await routeDaemon(page);
  182 | 
> 183 |   await page.goto("/?mode=getting-started");
      |              ^ Error: page.goto: Protocol error (Page.navigate): Cannot navigate to invalid URL
  184 |   await expect(page.getByRole("heading", { name: "Welcome to Goobers" })).toBeVisible();
  185 |   await expect(page.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "1");
  186 | 
  187 |   await page.getByRole("button", { name: "Continue" }).click();
  188 | 
  189 |   await page.getByRole("textbox", { name: "Local clone" }).fill("C:\\src\\widgets");
  190 |   await page.getByRole("button", { name: "Inspect clone" }).click();
  191 |   await expect(page.getByText("GitHub CLI authentication is ready as octocat.")).toBeVisible();
  192 |   await expect(page.getByText("npm run ci")).toBeVisible();
  193 |   await page.getByRole("textbox", { name: "Local clone" }).press("Enter");
  194 |   await expect(page.getByRole("heading", { name: "Choose where Instance Configuration lives" })).toBeVisible();
  195 |   await page.getByRole("button", { name: "Continue" }).click();
  196 |   await expect(page.getByRole("heading", { name: "Set up your first gaggle" })).toBeVisible();
  197 |   await page.getByRole("button", { name: "Continue" }).click();
  198 |   await expect(page.getByRole("heading", { name: "Choose which ready issues Goobers may implement" })).toBeVisible();
  199 |   await page.getByRole("button", { name: "Continue" }).click();
  200 |   await expect(page.getByRole("heading", { name: "Configure the agent runtime" })).toBeVisible();
  201 |   await page.getByRole("button", { name: "Continue" }).click();
  202 |   await expect(page.getByRole("heading", { name: "Review and create the instance" })).toBeVisible();
  203 | 
  204 |   await page.getByRole("button", { name: "Create Goobers instance" }).click();
  205 |   await expect(page.getByText(/Created C:\\work\\tutorial-instance/)).toBeVisible();
  206 |   await page.getByRole("button", { name: "Continue" }).click();
  207 | 
  208 |   await page.getByRole("button", { name: "Check labels and ready issues" }).click();
  209 |   await expect(page.getByText(/eligible work for the first run/)).toBeVisible();
  210 |   await page.getByRole("button", { name: "Continue" }).click();
  211 | 
  212 |   await page.getByRole("button", { name: "Run checks" }).click();
  213 |   await expect(page.getByText(/All configuration, harness, and repository checks passed/)).toBeVisible();
  214 |   await page.getByRole("button", { name: "Continue" }).click();
  215 | 
  216 |   await expect(page.getByRole("heading", { name: "Setup complete" })).toBeVisible();
  217 |   await expect(page.getByText('goobers run implementation "C:\\work\\tutorial-instance"')).toBeVisible();
  218 |   expect(fixture.jobStarted).toBe(false);
  219 | 
  220 |   await page.getByRole("button", { name: "Back" }).click();
  221 |   await expect(page.getByRole("heading", { name: "Check the setup" })).toBeVisible();
  222 | });
  223 | 
```