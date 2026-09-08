// The read-only probe.
//
// READ-ONLY BY CONSTRUCTION: this file never clicks, fills, or submits. It
// only navigates and reads. That matters because the live dashboard carries
// destructive controls (the attention queue's "Dismiss" buttons), and a
// monitor that could press one is a monitor that can destroy real state.
//
// Non-GET requests are OBSERVED and reported as violations rather than
// intercepted and blocked. Blocking would mean routing every request through
// Playwright, which proxies response bodies and is unreliable for the SSE
// stream — and the health of that stream is one of the things being measured.
// Observation cannot break what it watches.
import { loadPlaywright } from "./resolve-playwright.mjs";

const READ_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

function checkOf(browser, kind, route, ok, detail, ms) {
  return { browser, kind, route, ok, detail, ms };
}

async function probeBrowser(pw, name, config, evidenceDir) {
  const checks = [];
  const browser = await pw[name].launch();
  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  await context.tracing.start({ screenshots: true, snapshots: true });

  const consoleErrors = [];
  const failedRequests = [];
  const badStatuses = [];
  const writeAttempts = [];

  const page = await context.newPage();
  const ignore = config.ignoreConsole ?? [];
  const ignored = (t) => ignore.some((p) => t.includes(p));

  page.on("console", (m) => {
    if (m.type() === "error" && !ignored(m.text())) consoleErrors.push(m.text());
  });
  page.on("pageerror", (e) => consoleErrors.push("pageerror: " + e.message));
  page.on("requestfailed", (r) => {
    const t = r.failure()?.errorText ?? "";
    // Aborted SSE/streaming on teardown is expected, not a defect.
    if (t.includes("NS_BINDING_ABORTED") || t.includes("net::ERR_ABORTED")) return;
    if (!ignored(r.url())) failedRequests.push(`${r.url()} — ${t}`);
  });
  page.on("response", (r) => {
    if (r.status() >= 400 && !ignored(r.url())) badStatuses.push(`HTTP ${r.status()} ${r.url()}`);
  });
  page.on("request", (r) => {
    if (!READ_METHODS.has(r.method())) writeAttempts.push(`${r.method()} ${r.url()}`);
  });

  // 1. Every route renders its heading.
  for (const route of config.routes) {
    const t0 = Date.now();
    try {
      await page.goto(`${config.baseUrl}/${route.path}`, {
        waitUntil: "domcontentloaded",
        timeout: config.thresholds.navigationMs,
      });
      const name_ = route.regex ? new RegExp(route.heading, "i") : route.heading;
      await page
        .getByRole("heading", { name: name_ })
        .first()
        .waitFor({ state: "visible", timeout: config.thresholds.routeHeadingMs });
      checks.push(checkOf(name, "route", route.path, true, null, Date.now() - t0));
    } catch (e) {
      checks.push(
        checkOf(name, "route", route.path, false, firstLine(e), Date.now() - t0),
      );
    }
  }

  // 2. The live-update stream reaches "connected" promptly. This is the check
  //    that catches an SSE regression: the stream can be established at the
  //    HTTP level while the client still shows "reconnecting".
  const t1 = Date.now();
  try {
    await page.goto(`${config.baseUrl}/#/overview`, { waitUntil: "domcontentloaded" });
    await page
      .getByText(config.connectedText)
      .first()
      .waitFor({ state: "visible", timeout: config.thresholds.liveConnectedMs });
    checks.push(checkOf(name, "sse", "#/overview", true, null, Date.now() - t1));
  } catch (e) {
    checks.push(checkOf(name, "sse", "#/overview", false, firstLine(e), Date.now() - t1));
  }

  // 3. Passive observations gathered across the whole session.
  //
  // One check per DISTINCT error, never one check carrying a joined list.
  // Aggregating first makes the fingerprint depend on how many errors happened
  // to co-occur, so a run with two 503s does not match a run with one and the
  // same defect is tracked as several findings.
  const pushEach = (kind, items) => {
    const distinct = [...new Set(items.map(String))];
    if (distinct.length === 0) {
      checks.push(checkOf(name, kind, "*", true, null, null));
      return;
    }
    for (const item of distinct.slice(0, 10)) {
      checks.push(checkOf(name, kind, "*", false, item, null));
    }
  };
  pushEach("console", consoleErrors);
  pushEach("network", failedRequests);
  pushEach("http", badStatuses);
  pushEach("readonly", writeAttempts);

  const failed = checks.some((c) => !c.ok);
  let trace = null;
  let shot = null;
  if (failed && evidenceDir) {
    trace = `${evidenceDir}/trace-${name}.zip`;
    shot = `${evidenceDir}/screen-${name}.png`;
    await page.screenshot({ path: shot, fullPage: true }).catch(() => (shot = null));
  }
  await context.tracing.stop(trace ? { path: trace } : {});
  await browser.close();
  return { checks, trace, shot, consoleErrors, failedRequests, badStatuses, writeAttempts };
}

function firstLine(e) {
  return String(e?.message ?? e).split("\n")[0].slice(0, 200);
}

/** Runs the daemon health check plus a browser pass per configured engine. */
export async function runProbe(config, evidenceDir) {
  const { playwright, from } = loadPlaywright();
  const checks = [];
  const artifacts = [];

  const t0 = Date.now();
  try {
    const res = await fetch(`${config.daemonUrl}/api/v1/health`, {
      signal: AbortSignal.timeout(10000),
    });
    checks.push(checkOf("-", "daemon", "/api/v1/health", res.ok, res.ok ? null : `HTTP ${res.status}`, Date.now() - t0));
  } catch (e) {
    checks.push(checkOf("-", "daemon", "/api/v1/health", false, firstLine(e), Date.now() - t0));
  }

  for (const name of config.browsers) {
    try {
      const r = await probeBrowser(playwright, name, config, evidenceDir);
      checks.push(...r.checks);
      if (r.trace) artifacts.push(r.trace);
      if (r.shot) artifacts.push(r.shot);
    } catch (e) {
      checks.push(checkOf(name, "launch", "-", false, firstLine(e), null));
    }
  }
  return { at: new Date().toISOString(), playwrightFrom: from, checks, artifacts };
}
