import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import {
  mkdir,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { createServer } from "node:net";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { plantFixtureResponse } from "./plant-harness-fixtures.mjs";

const toolsRoot = dirname(fileURLToPath(import.meta.url));
const portalRoot = resolve(toolsRoot, "..");
const options = parseArgs(process.argv.slice(2));
const outputRoot = resolve(portalRoot, options.output);
const checks = [];
const captures = {};
const screenshotData = {};
/** The scenario a network request belongs to, for the asset budget diagnostic. */
let currentScenario = "boot";
const diagnostics = {
  alignment: [],
  browserConsole: [],
  cdpErrors: [],
  contextCycles: [],
  networkErrors: [],
  renderedContrast: {},
  /** Every request for the 540 KB illustration, with the state that caused it. */
  bitmapRequests: [],
  fallbackLatency: undefined,
  forcedColors: undefined,
  riskMotion: undefined,
  rigidNavigation: undefined,
  safeAreas: [],
  vite: [],
  zoomPacking: [],
};

async function main() {
  let vite;
  try {
  await rm(outputRoot, { recursive: true, force: true });
  await mkdir(outputRoot, { recursive: true });

  let baseUrl = options.baseUrl;
  if (!baseUrl) {
    vite = await startVite();
    baseUrl = vite.baseUrl;
  }

  const browserPath = await findBrowser(options.browser);
  const webgl = await launchBrowser(browserPath, "webgl", []);
  try {
    const page = await openPage(webgl, baseUrl);
    captures.lightWorld = await captureScenario(page, "light-world");
    check("light World initializes WebGL", captures.lightWorld.snapshot.renderer.state === "ready", {
      state: captures.lightWorld.snapshot.renderer.state,
    });
    check("light World reports a sized canvas", canvasReady(captures.lightWorld.snapshot), {
      canvas: captures.lightWorld.snapshot.canvas,
    });
    check("light World reports the fixture model and collision-free layout", captures.lightWorld.snapshot.model?.lens === "world" && layoutReady(captures.lightWorld.snapshot), {
      model: captures.lightWorld.snapshot.model,
      layout: captures.lightWorld.snapshot.layout,
    });
    check(
      "light World renders inside its authored luminance band",
      luminanceBandReport(captures.lightWorld).ok,
      { luminance: luminanceBandReport(captures.lightWorld) },
    );
    check(
      "light World meets every authored contrast gate",
      contrastReport(captures.lightWorld).ok,
      { contrast: contrastReport(captures.lightWorld) },
    );
    diagnostics.renderedContrast = { light: await measureRenderedContrast(page) };
    check(
      "light World representative machine pixels clear the deck by 3:1",
      diagnostics.renderedContrast.light.sampled > 0 &&
        diagnostics.renderedContrast.light.p20MachineVsDeck >= 3,
      { renderedContrast: diagnostics.renderedContrast.light },
    );
    const freshnessLink = await readFreshnessLink(page);
    check(
      "the Plant reads the page's own freshness rather than inferring it",
      freshnessLink.dom !== undefined &&
        freshnessLink.dom === freshnessLink.plant &&
        freshnessLink.forcedColors === false,
      freshnessLink,
    );
    check(
      "draw calls, triangles and backing pixels stay inside budget",
      budgetReport(captures.lightWorld).ok,
      { budget: budgetReport(captures.lightWorld) },
    );

    await resetProbe(page);
    await clickTheme(page, "dark");
    captures.darkWorld = await captureScenario(page, "dark-world");
    check("dark World is captured after theme application", captures.darkWorld.snapshot.model?.theme === "dark", {
      theme: captures.darkWorld.snapshot.model?.theme,
    });
    check("theme change reuses the runtime and scene", noRebuild(captures.darkWorld.snapshot) && reconciledWithoutCreating(captures.darkWorld.snapshot), {
      rebuild: rebuildMeasurement(captures.darkWorld.snapshot),
    });
    check(
      "dark World renders inside its authored luminance band",
      luminanceBandReport(captures.darkWorld).ok,
      { luminance: luminanceBandReport(captures.darkWorld) },
    );
    check(
      "dark World meets every authored contrast gate",
      contrastReport(captures.darkWorld).ok,
      { contrast: contrastReport(captures.darkWorld) },
    );
    diagnostics.renderedContrast.dark = await measureRenderedContrast(page);
    check(
      "dark World representative machine pixels clear the deck by 3:1",
      diagnostics.renderedContrast.dark.sampled > 0 &&
        diagnostics.renderedContrast.dark.p20MachineVsDeck >= 3,
      { renderedContrast: diagnostics.renderedContrast.dark },
    );
    check(
      "the theme swap repaints authored scene colours, not UI tokens",
      paletteSwapReport(captures.lightWorld, captures.darkWorld).ok,
      { palette: paletteSwapReport(captures.lightWorld, captures.darkWorld) },
    );

    await clickTheme(page, "light");
    await waitFor(page, "light theme", `document.documentElement.dataset.theme === "light"`);
    await resetProbe(page);
    await setPlantRoute(page, "risk");
    captures.lightRisk = await captureScenario(page, "light-risk");
    check("light Risk is captured with WebGL ready", captures.lightRisk.snapshot.renderer.state === "ready" && captures.lightRisk.snapshot.model?.lens === "risk", {
      state: captures.lightRisk.snapshot.renderer.state,
      lens: captures.lightRisk.snapshot.model?.lens,
    });
    check("lens change reuses the runtime and scene", noRebuild(captures.lightRisk.snapshot) && reconciledWithoutCreating(captures.lightRisk.snapshot), {
      rebuild: rebuildMeasurement(captures.lightRisk.snapshot),
    });
    check(
      "light Risk keeps the hall legible instead of erasing it",
      riskLegibilityReport(captures.lightRisk, captures.lightWorld).ok,
      { risk: riskLegibilityReport(captures.lightRisk, captures.lightWorld) },
    );
    check(
      "light Risk renders inside its authored luminance band",
      luminanceBandReport(captures.lightRisk).ok,
      { luminance: luminanceBandReport(captures.lightRisk) },
    );
    check(
      "the Risk headline states confirmed risk and completeness separately",
      riskHeadlineHonest(captures.lightRisk.snapshot.model?.risk),
      { risk: captures.lightRisk.snapshot.model?.risk },
    );
    diagnostics.riskDifference = await compareScreenshots(
      page,
      screenshotData["light-world"],
      screenshotData["light-risk"],
    );
    check(
      "Risk materially differs from World without replacing the healthy context",
      diagnostics.riskDifference.changedPixelRatio >= 0.025 &&
        diagnostics.riskDifference.changedPixelRatio <= 0.5 &&
        diagnostics.riskDifference.p95ChannelDelta >= 20,
      { difference: diagnostics.riskDifference },
    );
    const hierarchy = await readRiskHierarchy(page);
    check(
      "Risk exposes distinct text and non-hue silhouettes for every hierarchy level",
      hierarchy.levels.every(
        (entry) =>
          entry.labels.length > 0 &&
          entry.shapes.length === 1 &&
          entry.shapes[0] === entry.expectedShape,
      ) &&
        new Set(hierarchy.levels.map((entry) => entry.shapes[0])).size ===
          hierarchy.levels.length,
      hierarchy,
    );
    await selectFirstOverlayItem(page);
    await evaluate(
      page,
      `document.querySelector('.factory-plant-overlay-item[data-kind="station"]')?.focus()`,
    );
    diagnostics.renderedContrast.lightRisk = await measureRenderedContrast(page);
    diagnostics.semanticContrast = await measureSemanticContrast(page);
    check(
      "rendered labels, markers, focus, selection, and deck samples meet contrast gates",
      diagnostics.semanticContrast.label >= 4.5 &&
        diagnostics.semanticContrast.marker >= 3 &&
        diagnostics.semanticContrast.focus >= 3 &&
        diagnostics.semanticContrast.selection >= 3 &&
        diagnostics.renderedContrast.lightRisk.p20MachineVsDeck >= 3,
      {
        rendered: diagnostics.renderedContrast.lightRisk,
        semantic: diagnostics.semanticContrast,
      },
    );

    await delay(500);
    await resetProbe(page);
    const riskMotion = await measureMotion(page);
    diagnostics.riskMotion = riskMotion;
    /*
     * "Stopped" means no animation loop, not "no pixels ever change". A
     * scheduler that renders once in response to a discrete event and then goes
     * quiet is the design; requesting animation frames while nothing is
     * happening is the defect. rafRequestsAdded is therefore the assertion, and
     * the frame allowance covers a single on-demand repaint.
     */
    check(
      "Risk lens stops the frame loop",
      riskMotion.motion === false &&
        riskMotion.rafRequestsAdded <= riskMotion.modelUpdatesAdded + 1 &&
        riskMotion.framesAdded <= riskMotion.modelUpdatesAdded + 1,
      {
        motion: riskMotion,
      },
    );

    await resetProbe(page);
    await clickTheme(page, "dark");
    captures.darkRisk = await captureScenario(page, "dark-risk");
    check("dark Risk is captured after theme application", captures.darkRisk.snapshot.renderer.state === "ready" && captures.darkRisk.snapshot.model?.theme === "dark", {
      state: captures.darkRisk.snapshot.renderer.state,
      theme: captures.darkRisk.snapshot.model?.theme,
    });
    check(
      "dark Risk keeps the hall legible instead of erasing it",
      riskLegibilityReport(captures.darkRisk, captures.darkWorld).ok,
      { risk: riskLegibilityReport(captures.darkRisk, captures.darkWorld) },
    );
    check(
      "dark Risk renders inside its authored luminance band",
      luminanceBandReport(captures.darkRisk).ok,
      { luminance: luminanceBandReport(captures.darkRisk) },
    );
    check(
      "no lens or theme ever mounts scene fog",
      [
        captures.lightWorld,
        captures.darkWorld,
        captures.lightRisk,
        captures.darkRisk,
      ].every((capture) => capture.snapshot.visual?.fog === false),
      {
        fog: {
          darkRisk: captures.darkRisk.snapshot.visual?.fog,
          darkWorld: captures.darkWorld.snapshot.visual?.fog,
          lightRisk: captures.lightRisk.snapshot.visual?.fog,
          lightWorld: captures.lightWorld.snapshot.visual?.fog,
        },
      },
    );

    await setPlantRoute(page, "world");
    await resetProbe(page);
    await evaluate(
      page,
      `fetch("/__plant-harness/refresh", { method: "POST" }).then((response) => {
        if (!response.ok) throw new Error("Unable to arm fixture refresh");
        return response.json();
      })`,
    );
    let refreshError;
    try {
      await waitFor(
        page,
        "a live model refresh",
        `(() => {
          const snapshot = window.__plantProbe.snapshot();
          return snapshot.modelUpdates >= 1 &&
            snapshot.model?.counts?.activeRuns === 6 &&
            snapshot.overlay?.entries?.some(
              (entry) => entry.anchorId?.includes("01PLANTREFRESH"),
            );
        })()`,
        20_000,
      );
    } catch (error) {
      refreshError = error.message;
    }
    captures.modelRefresh = await captureScenario(page, "model-refresh");
    const refreshedCarrier =
      captures.modelRefresh.snapshot.overlay?.entries?.find(
        (entry) => entry.anchorId?.includes("01PLANTREFRESH"),
      );
    check("live model refresh reaches WebGL and the semantic overlay without rebuilding", (captures.modelRefresh.snapshot.modelUpdates ?? 0) >= 1 &&
      captures.modelRefresh.snapshot.model?.counts?.activeRuns === 6 &&
      Boolean(refreshedCarrier) &&
      noRebuild(captures.modelRefresh.snapshot), {
      error: refreshError,
      activeRuns: captures.modelRefresh.snapshot.model?.counts?.activeRuns,
      refreshedCarrier,
      rebuild: rebuildMeasurement(captures.modelRefresh.snapshot),
    });
    /*
     * Asset budget. Everything above ran on a page whose WebGL context was
     * healthy from first paint, so the 540 KB illustration must not have been
     * fetched. Measured here, before the first deliberate reload or context
     * loss, because those legitimately do reach the bitmap.
     */
    diagnostics.readyPathBitmapRequests = diagnostics.bitmapRequests.length;
    check(
      "the successful WebGL path never downloads the bitmap fallback",
      diagnostics.bitmapRequests.length === 0,
      { bitmapRequests: diagnostics.bitmapRequests.slice() },
    );

    await evaluate(
      page,
      `localStorage.setItem("goobers-theme", "light"); location.hash = "#/factory?layout=plant"`,
    );
    await page.cdp.send("Page.reload", { ignoreCache: true });
    await waitFor(
      page,
      "fresh light World Plant",
      `Boolean(window.__plantProbe) &&
       window.__plantProbe.snapshot().renderer.state === "ready" &&
       window.__plantProbe.snapshot().model?.lens === "world" &&
       window.__plantProbe.snapshot().model?.theme === "light"`,
      15_000,
    );
    await delay(750);
    await resetProbe(page);
    diagnostics.fallbackLatency = await measureFallbackLatency(page);
    const lost = diagnostics.fallbackLatency.lost;
    let lossError;
    try {
      await waitFor(
        page,
        "lost WebGL context",
        `window.__plantProbe.snapshot().renderer.state === "fallback" &&
         window.__plantProbe.snapshot().renderer.losses >= 1`,
        5_000,
      );
    } catch (error) {
      lossError = error.message;
    }
    captures.contextLost = await captureScenario(page, "context-lost");
    check("WEBGL_lose_context is available", lost === true, { returned: lost });
    check("context loss reveals fallback", captures.contextLost.snapshot.renderer.state === "fallback", {
      error: lossError,
      renderer: captures.contextLost.snapshot.renderer,
    });
    /*
     * Lazy-loading the illustration is only honest if losing the context still
     * leaves a complete plant on screen immediately. The authored backdrop plus
     * the exact topology and controls are what carry that, so they are what is
     * timed - not the bitmap, which arrives afterwards as enhancement.
     */
    check(
      "context loss shows a complete DOM fallback within 200ms",
      diagnostics.fallbackLatency.completeAfterMs !== undefined &&
        diagnostics.fallbackLatency.completeAfterMs <= 200 &&
        diagnostics.fallbackLatency.authoredBackdrop === true &&
        diagnostics.fallbackLatency.machines > 0 &&
        diagnostics.fallbackLatency.controls > 0,
      { fallbackLatency: diagnostics.fallbackLatency },
    );

    const restored = await evaluate(page, `window.__plantProbe.restoreContext()`);
    let restorationError;
    try {
      await waitFor(
        page,
        "restored WebGL context",
        `window.__plantProbe.snapshot().renderer.state === "ready" &&
         window.__plantProbe.snapshot().renderer.restores >= 1`,
        5_000,
      );
    } catch (error) {
      restorationError = error.message;
    }
    captures.contextRestored = await captureScenario(page, "context-restored");
    check("context restoration returns the renderer to ready", restored === true && captures.contextRestored.snapshot.renderer.state === "ready", {
      returned: restored,
      error: restorationError,
      renderer: captures.contextRestored.snapshot.renderer,
    });
    check("restoration keeps the retained scene", captures.contextRestored.snapshot.scene.builds === 0 && captures.contextRestored.snapshot.scene.disposals === 0 && captures.contextRestored.snapshot.renderer.contexts === 0, {
      rebuild: rebuildMeasurement(captures.contextRestored.snapshot),
    });

    await resetProbe(page);
    const repeatedCycles = [];
    for (let attempt = 2; attempt <= 3; attempt += 1) {
      repeatedCycles.push(await cycleContext(page, attempt));
    }
    captures.contextRepeated = await captureScenario(page, "context-repeated");
    diagnostics.contextCycles = repeatedCycles.map((cycle) => ({
      attempt: cycle.attempt,
      lost: cycle.lost,
      lossError: cycle.lossError,
      lostState: cycle.lostSnapshot.renderer.state,
      restored: cycle.restored,
      restorationError: cycle.restorationError,
      restoredState: cycle.restoredSnapshot.renderer.state,
    }));
    check("repeated context loss and restoration keeps working", repeatedCycles.every((cycle) => cycle.lost === true && cycle.restored === true && cycle.lostSnapshot.renderer.state === "fallback" && cycle.restoredSnapshot.renderer.state === "ready"), {
      cycles: diagnostics.contextCycles,
      renderer: captures.contextRepeated.snapshot.renderer,
      rebuild: rebuildMeasurement(captures.contextRepeated.snapshot),
    });

    const alignment = [];
    for (const viewport of ALIGNMENT_VIEWPORTS) {
      for (const inspectorOpen of [true, false]) {
        alignment.push(
          await measureAlignment(page, { ...viewport, inspectorOpen }),
        );
      }
    }
    diagnostics.alignment = alignment;
    captures.alignment = await captureScenario(page, "alignment");

    const measured = alignment.filter((entry) => entry.overlay);
    check(
      "every viewport reports a live WebGL projection source",
      measured.length === alignment.length &&
        alignment.every((entry) => entry.overlay?.source === "webgl") &&
        alignment.every((entry) => (entry.overlay?.entries ?? 0) > 0),
      { alignment: alignment.map(summarizeAlignment) },
    );
    check(
      "DOM hit targets sit on the projected anchor within 6 CSS px",
      measured.length > 0 &&
        measured.every(
          (entry) =>
            entry.dom.count > 0 && entry.dom.max <= PROJECTION_TOLERANCE_PX,
        ),
      {
        tolerance: PROJECTION_TOLERANCE_PX,
        worst: worstAlignment(measured, (entry) => entry.dom.max),
        alignment: measured.map(summarizeAlignment),
      },
    );
    check(
      "the runtime's own projection probe agrees with the DOM anchors",
      measured.every(
        (entry) =>
          entry.projection.count > 0 &&
          entry.projection.maxDrift <= PROJECTION_TOLERANCE_PX,
      ),
      {
        tolerance: PROJECTION_TOLERANCE_PX,
        worst: worstAlignment(measured, (entry) => entry.projection.maxDrift),
        alignment: measured.map(summarizeAlignment),
      },
    );
    check(
      "priority labels never overlap or clip",
      measured.every(
        (entry) => entry.overlay.collisions === 0 && entry.overlay.clipped === 0,
      ),
      { alignment: measured.map(summarizeAlignment) },
    );
    check(
      "hit targets keep their minimum size at every viewport",
      measured.every((entry) => entry.overlay.hitTargets.belowMinimum === 0),
      {
        alignment: measured.map((entry) => ({
          label: entry.label,
          hitTargets: entry.overlay.hitTargets,
        })),
      },
    );
    check(
      "the inspector never occludes selected or critical semantics",
      measured.every(
        (entry) =>
          entry.overlay.occlusion.selected === 0 &&
          entry.overlay.occlusion.critical === 0,
      ),
      {
        alignment: measured.map((entry) => ({
          inspectorOpen: entry.inspectorOpen,
          label: entry.label,
          occlusion: entry.overlay.occlusion,
        })),
      },
    );
    check(
      "selection camera motion clears the real inspector before Fit all",
      measured.every(
        (entry) =>
          !entry.inspectorOpen || entry.preFitSelectedOcclusion === 0,
      ),
      {
        alignment: measured.map((entry) => ({
          label: entry.label,
          preFitSelectedOcclusion: entry.preFitSelectedOcclusion,
        })),
      },
    );
    check(
      "the safe-area model matches the drawer at common laptop viewports",
      measured.every((entry) => {
        if (!entry.inspectorOpen) return entry.actualInspector === undefined;
        const expected = entry.inspector;
        const actual = entry.actualInspector;
        return Boolean(
          expected &&
            actual &&
            Math.abs(expected.x - actual.x) <= 2 &&
            Math.abs(expected.y - actual.y) <= 2 &&
            Math.abs(expected.width - actual.width) <= 2 &&
            Math.abs(expected.height - actual.height) <= 2,
        );
      }),
      {
        alignment: measured.map((entry) => ({
          actual: entry.actualInspector,
          expected: entry.inspector,
          label: entry.label,
        })),
      },
    );
    check(
      "Fit all shows the whole world inside the unobscured rectangle",
      measured.every((entry) => entry.fit.contained),
      { alignment: measured.map((entry) => ({ fit: entry.fit, label: entry.label })) },
    );
    check(
      "closing the inspector restores the full safe area",
      ALIGNMENT_VIEWPORTS.every((viewport) => {
        const open = measured.find(
          (entry) => entry.label === alignmentLabel(viewport, true),
        );
        const closed = measured.find(
          (entry) => entry.label === alignmentLabel(viewport, false),
        );
        if (!open || !closed) {
          return false;
        }
        return (
          closed.safeArea.width >= open.safeArea.width &&
          Math.abs(closed.safeArea.width - closed.viewport.width) <= 1 &&
          Math.abs(closed.safeArea.height - closed.viewport.height) <= 1
        );
      }),
      {
        alignment: measured.map((entry) => ({
          inspectorOpen: entry.inspectorOpen,
          label: entry.label,
          safeArea: entry.safeArea,
          viewport: entry.viewport,
        })),
      },
    );

    const zoomPacking = [];
    for (const zoom of ZOOM_LEVELS) {
      zoomPacking.push(await measureZoomPacking(page, zoom));
    }
    diagnostics.zoomPacking = zoomPacking;
    check(
      "label packing and probe rectangles match rendered CSS at every zoom",
      zoomPacking.every(
        (entry) =>
          Math.abs(entry.camera.zoom - entry.zoom) <= 0.001 &&
          entry.actual.collisions === 0 &&
          entry.actual.collisions === entry.probe.collisions &&
          entry.actual.maxRectDelta <= 1 &&
          entry.actual.minHitWidth >= 31.99 &&
          entry.actual.minHitHeight >= 31.99 &&
          entry.probe.hitTargets.belowMinimum === 0,
      ),
      { zoomPacking },
    );
    check(
      "zoomed probe occlusion matches rendered rectangles and keeps critical semantics clear",
      zoomPacking.every(
        (entry) =>
          entry.actual.occlusion.total === entry.probe.occlusion.total &&
          entry.actual.occlusion.critical === entry.probe.occlusion.critical &&
          entry.actual.occlusion.selected === entry.probe.occlusion.selected &&
          entry.probe.occlusion.critical === 0 &&
          entry.probe.occlusion.selected === 0,
      ),
      { zoomPacking },
    );

    diagnostics.rigidNavigation = await measureRigidNavigation(page);
    check(
      "outer zoom and pan rigidly move the Plant without refitting the WebGL camera",
      diagnostics.rigidNavigation.projectionRevisionStable &&
        diagnostics.rigidNavigation.localProjectionStable &&
        diagnostics.rigidNavigation.anchorDeltaMatches,
      diagnostics.rigidNavigation,
    );
    check(
      "fallback switching preserves the outer camera pose",
      diagnostics.rigidNavigation.fallbackPoseStable,
      diagnostics.rigidNavigation,
    );

    diagnostics.safeAreas = [];
    for (const viewport of [
      { height: 320, width: 640 },
      { height: 200, width: 360 },
    ]) {
      diagnostics.safeAreas.push(await measureSmallSafeArea(page, viewport));
    }
    check(
      "short viewports keep an explicit usable Plant area and visible critical controls",
      diagnostics.safeAreas.every(
        (entry) =>
          entry.closed.safeArea.width >= MIN_USABLE_PLANT_WIDTH &&
          entry.closed.safeArea.height >= MIN_USABLE_PLANT_HEIGHT &&
          entry.closed.viewport.height >= MIN_USABLE_PLANT_HEIGHT &&
          entry.closed.criticalVisible &&
          entry.closed.triggerVisible &&
          !entry.closed.documentOverflowX &&
          !entry.closed.documentOverflowY &&
          entry.open.snapshotSeparated &&
          entry.open.actualSeparated &&
          entry.open.safeArea.height >= MIN_USABLE_PLANT_HEIGHT,
      ),
      {
        thresholds: {
          height: MIN_USABLE_PLANT_HEIGHT,
          width: MIN_USABLE_PLANT_WIDTH,
        },
        safeAreas: diagnostics.safeAreas,
      },
    );

    await page.cdp.send("Emulation.clearDeviceMetricsOverride");
    await delay(300);

    await page.cdp.send("Emulation.setDeviceMetricsOverride", {
      width: 640,
      height: 480,
      deviceScaleFactor: 1,
      mobile: false,
    });
    await delay(500);
    await setInspector(page, false);
    await evaluate(page, `document.querySelector(".factory-viewport-fit")?.click()`);
    await delay(350);
    captures.viewportOverflow = await captureScenario(page, "viewport-overflow");
    check("Plant viewport clips its world", captures.viewportOverflow.snapshot.viewport?.viewport.overflowX === "hidden" && captures.viewportOverflow.snapshot.viewport?.viewport.overflowY === "hidden", {
      viewport: captures.viewportOverflow.snapshot.viewport,
    });
  } finally {
    await closeBrowser(webgl);
  }

  const stress = await launchBrowser(browserPath, "stress", []);
  try {
    const page = await openPage(stress, baseUrl, "ready", { fixture: "stress" });
    captures.stress = await captureScenario(page, "stress-50x50");
    const stressIdentity = await evaluate(
      page,
      `(() => {
        const labels = [...document.querySelectorAll(
          '.factory-plant-overlay-item[data-kind="bay"] .factory-plant-overlay-label',
        )].map((node) => node.textContent.trim());
        return {
          labels,
          unique: new Set(labels).size,
          allCarryIdentity: labels.every((label) => /scale\\s*·\\s*line-\\d+/i.test(label)),
        };
      })()`,
    );
    check(
      "the browser stress fixture contains 50 runs and 50 workers",
      captures.stress.snapshot.model?.counts?.carriers === 50 &&
        captures.stress.snapshot.model?.counts?.workers === 50 &&
        captures.stress.snapshot.model?.counts?.renderedCarriers === 50 &&
        captures.stress.snapshot.model?.counts?.renderedWorkers === 50,
      { model: captures.stress.snapshot.model },
    );
    check(
      "the real 50-run/50-worker frame stays inside render budgets",
      budgetReport(captures.stress).ok,
      { budget: budgetReport(captures.stress) },
    );
    check(
      "duplicate workflow display names retain unique visible gaggle and workflow identity",
      stressIdentity.labels.length === 10 &&
        stressIdentity.unique === stressIdentity.labels.length &&
        stressIdentity.allCarryIdentity,
      stressIdentity,
    );
  } finally {
    await closeBrowser(stress);
  }

  const commons = await launchBrowser(browserPath, "commons", []);
  try {
    const page = await openPage(commons, baseUrl, "ready", { fixture: "commons" });
    captures.commons = await captureScenario(page, "commons-idle-workers");
    check(
      "idle roster workers render in the commons without invented work",
      captures.commons.snapshot.model?.counts?.carriers === 0 &&
        captures.commons.snapshot.model?.counts?.workers === 2 &&
        captures.commons.snapshot.model?.counts?.renderedWorkers === 2,
      { model: captures.commons.snapshot.model },
    );
    const commonsWorkerTargets = await evaluate(
      page,
      `(() => {
        const rects = [...document.querySelectorAll(
          '.factory-plant-overlay-item[data-kind="worker"] .factory-plant-overlay-hit',
        )].map((node) => {
          const rect = node.getBoundingClientRect();
          return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom };
        });
        const overlaps = [];
        for (let left = 0; left < rects.length; left += 1) {
          for (let right = left + 1; right < rects.length; right += 1) {
            const a = rects[left];
            const b = rects[right];
            if (
              a.left < b.right &&
              a.right > b.left &&
              a.top < b.bottom &&
              a.bottom > b.top
            ) {
              overlaps.push([left, right]);
            }
          }
        }
        return { count: rects.length, overlaps };
      })()`,
    );
    check(
      "commons worker hit targets remain individually usable",
      commonsWorkerTargets.count === 2 &&
        commonsWorkerTargets.overlaps.length === 0,
      commonsWorkerTargets,
    );
    check(
      "the commons frame stays inside render budgets",
      budgetReport(captures.commons).ok,
      { budget: budgetReport(captures.commons) },
    );
  } finally {
    await closeBrowser(commons);
  }

  const rejectedChunk = await launchBrowser(browserPath, "chunk-rejection", []);
  try {
    const page = await openPage(rejectedChunk, baseUrl, "fallback", {
      rejectRendererImportOnce: true,
    });
    captures.chunkRejected = await captureScenario(page, "chunk-rejected");
    const rejected = await evaluate(
      page,
      `(() => ({
        authoredBackdrop: Boolean(document.querySelector(".factory-plant-backdrop-authored")),
        controls: document.querySelectorAll("[data-plant-focus-id]").length,
        machines: document.querySelectorAll(
          ".factory-plant-exact-fallback .factory-station",
        ).length,
        retry: Boolean(document.querySelector(".factory-plant-renderer-status button")),
        status: document.querySelector(".factory-plant-renderer-status")?.textContent.trim(),
      }))()`,
    );
    check(
      "a rejected renderer import keeps the complete fallback and semantic controls",
      rejected.authoredBackdrop &&
        rejected.controls > 0 &&
        rejected.machines > 0 &&
        rejected.retry &&
        /renderer unavailable/i.test(rejected.status ?? ""),
      rejected,
    );
    await evaluate(
      page,
      `document.querySelector(".factory-plant-renderer-status button")?.click()`,
    );
    await waitFor(
      page,
      "renderer import retry",
      `window.__plantProbe.snapshot().renderer.state === "ready"`,
      15_000,
    );
    captures.chunkRetried = await captureScenario(page, "chunk-retried");
    const retried = await evaluate(
      page,
      `({
        semanticControls: document.querySelectorAll("[data-plant-focus-id]").length,
        statusVisible: Boolean(document.querySelector(".factory-plant-renderer-status")),
      })`,
    );
    check(
      "renderer import retry restores WebGL without losing the plant",
      captures.chunkRetried.snapshot.renderer.state === "ready" &&
        retried.semanticControls > 0 &&
        !retried.statusVisible,
      { renderer: captures.chunkRetried.snapshot.renderer, retried },
    );
  } finally {
    await closeBrowser(rejectedChunk);
  }

  const truth = await launchBrowser(browserPath, "read-truth", []);
  try {
    for (const variant of ["lagging", "partial"]) {
      const page = await openPage(truth, baseUrl, "ready", { fixture: variant });
      await setPlantRoute(page, "risk");
      captures[`${variant}Risk`] = await captureScenario(
        page,
        `${variant}-risk`,
      );
      const risk = captures[`${variant}Risk`].snapshot.model?.risk;
      const readState = captures[`${variant}Risk`].snapshot.model?.readState;
      check(
        `${variant} reads can never produce a complete all-clear`,
        readState?.data?.kind === variant &&
          risk?.complete === false &&
          risk?.allClear === false &&
          /Incomplete read:/i.test(risk?.detail ?? "") &&
          !/^No confirmed current risk$/i.test(risk?.headline ?? ""),
        { readState, risk },
      );
    }
  } finally {
    await closeBrowser(truth);
  }

  const noWebgl = await launchBrowser(browserPath, "no-webgl", [
    "--disable-webgl",
    "--disable-gpu",
  ]);
  try {
    const page = await openPage(noWebgl, baseUrl, "fallback");
    captures.noWebglFallback = await captureScenario(page, "no-webgl-fallback");
    check("no-WebGL mode renders the image fallback", captures.noWebglFallback.snapshot.renderer.state === "fallback", {
      renderer: captures.noWebglFallback.snapshot.renderer,
    });
    const fallbackDom = await readFallbackDom(page);
    check(
      "the fallback carries a complete plant in the DOM, not a blank frame",
      fallbackDom.authoredBackdrop &&
        fallbackDom.machines > 0 &&
        fallbackDom.controls > 0,
      { fallback: fallbackDom },
    );
  } finally {
    await closeBrowser(noWebgl);
  }

  /*
   * Forced colours. The OS has replaced the author's palette, and a canvas is
   * exempt from that substitution, so the honest response is to skip WebGL and
   * hand over the DOM plant the system can actually recolour.
   */
  const forcedColors = await launchBrowser(browserPath, "forced-colors", [
    "--force-prefers-reduced-motion",
  ]);
  try {
    const page = await openPage(forcedColors, baseUrl, "exact", {
      exactTopology: true,
      emulatedMedia: [
        { name: "forced-colors", value: "active" },
        { name: "prefers-color-scheme", value: "dark" },
      ],
    });
    captures.forcedColors = await captureScenario(page, "forced-colors-fallback");
    const forced = await evaluate(
      page,
      `(() => {
        const exact = document.querySelector(".factory-forced-colors-exact");
        const hidden = [...document.querySelectorAll(".factory-station")]
          .filter((node) => {
            const style = getComputedStyle(node);
            return style.display === "none" || style.visibility === "hidden";
          }).length;
        return {
          active: matchMedia("(forced-colors: active)").matches,
          bitmaps: document.querySelectorAll("img.factory-plant-backdrop").length,
          canvases: document.querySelectorAll("canvas.factory-plant-webgl").length,
          exact: exact?.getAttribute("data-exact-topology") ?? null,
          floor: Boolean(document.querySelector(".factory-forced-colors-exact .factory-floor")),
          hiddenMachines: hidden,
          machines: document.querySelectorAll(".factory-station").length,
          notice: document.querySelector(".factory-forced-colors-notice")?.textContent.trim(),
          statusShapes: document.querySelectorAll(".factory-station[data-status]").length,
        };
      })()`,
    );
    diagnostics.forcedColors = forced;
    check(
      "forced colours skip WebGL and keep a usable DOM plant",
      forced.active === true &&
        forced.canvases === 0 &&
        forced.bitmaps === 0 &&
        forced.exact === "true" &&
        forced.floor === true &&
        forced.machines > 0 &&
        forced.hiddenMachines === 0 &&
        forced.statusShapes > 0 &&
        /exact Lines topology/i.test(forced.notice ?? ""),
      { forcedColors: forced },
    );
    const forcedFocusId = await evaluate(
      page,
      `(() => {
        const target = document.querySelector(
          '.factory-forced-colors-exact .factory-station[data-plant-focus-id]',
        );
        target?.focus();
        return target?.getAttribute("data-plant-focus-id") ?? null;
      })()`,
    );
    await page.cdp.send("Emulation.setEmulatedMedia", {
      features: [
        { name: "forced-colors", value: "none" },
        { name: "prefers-color-scheme", value: "dark" },
        { name: "prefers-reduced-motion", value: "reduce" },
      ],
    });
    await waitFor(
      page,
      "forced-colors exit",
      `Boolean(document.querySelector(".factory-plant-overlay"))`,
    );
    const webglFocusId = await evaluate(
      page,
      `document.activeElement?.getAttribute("data-plant-focus-id") ?? null`,
    );
    await page.cdp.send("Emulation.setEmulatedMedia", {
      features: [
        { name: "forced-colors", value: "active" },
        { name: "prefers-color-scheme", value: "dark" },
        { name: "prefers-reduced-motion", value: "reduce" },
      ],
    });
    await waitFor(
      page,
      "forced-colors re-entry",
      `Boolean(document.querySelector(".factory-forced-colors-exact"))`,
    );
    const exactFocusId = await evaluate(
      page,
      `document.activeElement?.getAttribute("data-plant-focus-id") ?? null`,
    );
    check(
      "forced-colors branch replacement preserves semantic focus",
      Boolean(forcedFocusId) &&
        webglFocusId === forcedFocusId &&
        exactFocusId === forcedFocusId,
      { exactFocusId, forcedFocusId, webglFocusId },
    );
    await evaluate(
      page,
      `(() => {
        const select = document.querySelector('.factory-control select');
        const option = select && [...select.options].find((candidate) => candidate.value);
        if (select && option) {
          select.value = option.value;
          select.dispatchEvent(new Event("change", { bubbles: true }));
        }
      })()`,
    );
    await waitFor(
      page,
      "forced-colors gaggle filter",
      `Boolean(document.querySelector('.factory-control select')?.value)`,
    );
    await evaluate(
      page,
      `(() => {
        const select = document.querySelectorAll('.factory-control select')[1];
        const option = select && [...select.options].find((candidate) => candidate.value);
        if (select && option) {
          select.value = option.value;
          select.dispatchEvent(new Event("change", { bubbles: true }));
        }
      })()`,
    );
    await waitFor(
      page,
      "forced-colors workflow filter",
      `Boolean(document.querySelectorAll('.factory-control select')[1]?.value)`,
    );
    const forcedBefore = await evaluate(
      page,
      `({
        gaggle: document.querySelector('.factory-control select')?.value ?? "",
        workflow: document.querySelectorAll('.factory-control select')[1]?.value ?? "",
      })`,
    );
    await evaluate(
      page,
      `document.querySelector(".factory-station")?.click()`,
    );
    await waitFor(
      page,
      "forced-colors station selection",
      `Boolean(document.querySelector('.factory-station[aria-pressed="true"]'))`,
    );
    const forcedInteraction = await evaluate(
      page,
      `({
        before: ${JSON.stringify(forcedBefore)},
        inspector: document.querySelector(".factory-inspector")?.textContent ?? "",
        selected: Boolean(document.querySelector('.factory-station[aria-pressed="true"]')),
      })`,
    );
    await evaluate(
      page,
      `(() => {
        const risk = [...document.querySelectorAll('[aria-label="Floor lens"] button')]
          .find((button) => button.textContent.trim() === "Risk");
        if (risk instanceof HTMLElement) risk.click();
      })()`,
    );
    await waitFor(
      page,
      "forced-colors Risk exact topology",
      `Boolean(document.querySelector(".factory-forced-colors-exact")) &&
       [...document.querySelectorAll('[aria-label="Floor lens"] button')]
         .some((button) => button.textContent.trim() === "Risk" &&
           button.getAttribute("aria-pressed") === "true")`,
    );
    const forcedAfter = await evaluate(
      page,
      `({
        exact: Boolean(document.querySelector(".factory-forced-colors-exact")),
        gaggle: document.querySelector('.factory-control select')?.value ?? "",
        selected: Boolean(document.querySelector('.factory-station[aria-pressed="true"]')),
        workflow: document.querySelectorAll('.factory-control select')[1]?.value ?? "",
      })`,
    );
    check(
      "forced-colors exact topology preserves selection, inspector, lens, and filters",
      forcedInteraction.selected &&
        /stage|machine|WIP/i.test(forcedInteraction.inspector) &&
        forcedAfter.exact &&
        forcedAfter.selected &&
        forcedAfter.gaggle === forcedInteraction.before.gaggle &&
        forcedAfter.workflow === forcedInteraction.before.workflow,
      { after: forcedAfter, interaction: forcedInteraction },
    );
  } finally {
    await closeBrowser(forcedColors);
  }

  /*
   * Asset budget, whole run. The illustration is a fallback asset, so it must
   * arrive only where a fallback image is actually shown: never on the healthy
   * WebGL path, and never under forced colours, where the DOM plant - not a
   * bitmap the system cannot recolour - is the fallback.
   */
  const bitmapsByBrowser = (label) =>
    diagnostics.bitmapRequests.filter((request) => request.browser === label)
      .length;
  check(
    "the bitmap fallback loads only where an image fallback is shown",
    bitmapsByBrowser("forced-colors") === 0 && bitmapsByBrowser("no-webgl") >= 1,
    {
      byBrowser: {
        forcedColors: bitmapsByBrowser("forced-colors"),
        noWebgl: bitmapsByBrowser("no-webgl"),
        webgl: bitmapsByBrowser("webgl"),
      },
      readyPathBitmapRequests: diagnostics.readyPathBitmapRequests,
      bitmapRequests: diagnostics.bitmapRequests,
    },
  );

  const result = {
    ok:
      checks.every((item) => item.ok) &&
      diagnostics.cdpErrors.length === 0 &&
      diagnostics.networkErrors.length === 0,
    baseUrl,
    fixtureMode: options.fixtures,
    browser: browserPath,
    checks,
    measurements: {
      themeRebuild: rebuildMeasurement(captures.darkWorld?.snapshot),
      riskRebuild: rebuildMeasurement(captures.lightRisk?.snapshot),
      darkRiskRebuild: rebuildMeasurement(captures.darkRisk?.snapshot),
      modelRefreshRebuild: rebuildMeasurement(captures.modelRefresh?.snapshot),
      restorationRebuild: rebuildMeasurement(captures.contextRestored?.snapshot),
      repeatedContextRebuild: rebuildMeasurement(captures.contextRepeated?.snapshot),
      layout: captures.lightWorld?.snapshot.layout,
      drawCalls: {
        actual: captures.lightWorld?.snapshot.renderer?.info?.calls,
        layoutPlan: captures.lightWorld?.snapshot.layout?.drawCalls,
      },
      budget: budgetReport(captures.lightWorld),
      stressBudget: budgetReport(captures.stress),
      luminance: {
        darkRisk: luminanceBandReport(captures.darkRisk),
        darkWorld: luminanceBandReport(captures.darkWorld),
        lightRisk: luminanceBandReport(captures.lightRisk),
        lightWorld: luminanceBandReport(captures.lightWorld),
      },
      contrast: {
        dark: contrastReport(captures.darkWorld),
        light: contrastReport(captures.lightWorld),
      },
      renderedContrast: diagnostics.renderedContrast,
      semanticContrast: diagnostics.semanticContrast,
      riskDifference: diagnostics.riskDifference,
      riskLegibility: {
        dark: riskLegibilityReport(captures.darkRisk, captures.darkWorld),
        light: riskLegibilityReport(captures.lightRisk, captures.lightWorld),
      },
      riskSummary: {
        dark: captures.darkRisk?.snapshot.model?.risk,
        light: captures.lightRisk?.snapshot.model?.risk,
      },
      paletteSwap: paletteSwapReport(captures.lightWorld, captures.darkWorld),
      forcedColors: diagnostics.forcedColors,
      fallbackLatency: diagnostics.fallbackLatency,
      bitmapRequests: diagnostics.bitmapRequests,
      readyPathBitmapRequests: diagnostics.readyPathBitmapRequests,
      riskMotion: diagnostics.riskMotion,
      lightWorldProjection: captures.lightWorld?.snapshot.projection,
      darkWorldProjection: captures.darkWorld?.snapshot.projection,
      lightRiskProjection: captures.lightRisk?.snapshot.projection,
      darkRiskProjection: captures.darkRisk?.snapshot.projection,
      alignment: diagnostics.alignment,
      projectionTolerancePx: PROJECTION_TOLERANCE_PX,
      rigidNavigation: diagnostics.rigidNavigation,
      safeAreas: diagnostics.safeAreas,
      viewport: captures.viewportOverflow?.snapshot.viewport,
      zoomPacking: diagnostics.zoomPacking,
    },
    captures,
    diagnostics,
  };
  const jsonPath = join(outputRoot, "results.json");
  await writeFile(jsonPath, `${JSON.stringify(result, null, 2)}\n`, "utf8");
  emitTap(result, jsonPath);
  if (!result.ok) {
    process.exitCode = 1;
  }
  } catch (error) {
    console.error(`not ok 1 - Plant harness completed\n  ---\n  message: ${yamlString(error instanceof Error ? error.stack ?? error.message : String(error))}\n  ...`);
    process.exitCode = 1;
  } finally {
    if (vite) {
      await terminateChild(vite.process);
    }
  }
}

async function openPage(browser, baseUrl, expectedState = "ready", extra = {}) {
  const target = await fetchJson(
    `http://127.0.0.1:${browser.port}/json/new?${encodeURIComponent("about:blank")}`,
    { method: "PUT" },
  );
  const cdp = await CdpConnection.connect(target.webSocketDebuggerUrl);
  browser.connections.add(cdp);
  const loadingEvents = [];
  cdp.on("Runtime.consoleAPICalled", (event) => {
    const values = event.args
      .map((arg) => arg.value ?? arg.description)
      .filter((value) => value !== undefined);
    diagnostics.browserConsole.push({ type: event.type, values });
  });
  cdp.on("Runtime.exceptionThrown", (event) => {
    const description =
      event.exceptionDetails.exception?.description ?? event.exceptionDetails.text;
    diagnostics.cdpErrors.push(description);
  });
  cdp.on("Network.requestWillBeSent", (event) => {
    /*
     * Asset budget: the illustration must not ride along on a healthy WebGL
     * page. Recorded with the browser profile and the last captured scenario.
     * The profile is the reliable attribution - the scenario tag is only ever
     * as fresh as the last capture, and a lazy fetch can land after it.
     */
    if (event.request.url.includes("/factory-plant-base.png")) {
      diagnostics.bitmapRequests.push({
        browser: browser.label,
        scenario: currentScenario,
        url: event.request.url,
      });
    }
  });
  cdp.on("Network.responseReceived", (event) => {
    const url = event.response.url;
    if (url.includes("/api/v1/")) {
      diagnostics.browserConsole.push({
        type: "network",
        values: [event.response.status, url],
      });
      if (event.response.status < 200 || event.response.status >= 300) {
        diagnostics.networkErrors.push({
          status: event.response.status,
          url,
        });
      }
    }
  });
  cdp.on("Page.loadEventFired", () => loadingEvents.push("load"));
  cdp.on("Page.frameStoppedLoading", (event) => loadingEvents.push(`stopped:${event.frameId}`));
  cdp.onError((error) => diagnostics.cdpErrors.push(error.message));
  await Promise.all([
    cdp.send("Page.enable"),
    cdp.send("Runtime.enable"),
    cdp.send("Network.enable"),
    cdp.send("Emulation.setEmulatedMedia", {
      features: [
        { name: "prefers-reduced-motion", value: "no-preference" },
        ...(extra.emulatedMedia ?? []),
      ],
    }),
  ]);
  if (options.fixtures) {
    await installFixtures(cdp, extra.fixture ?? "default");
  }
  if (extra.rejectRendererImportOnce) {
    await cdp.send("Page.addScriptToEvaluateOnNewDocument", {
      source: `
        (() => {
          let attempts = 0;
          window.__plantRendererImport = () => {
            attempts += 1;
            if (attempts === 1) {
              return Promise.reject(new Error("synthetic Plant renderer chunk rejection"));
            }
            return import("/src/components/FactoryWebGLScene.tsx");
          };
        })();
      `,
    });
  }
  const url = `${baseUrl.replace(/\/$/, "")}/?plant-probe=1#/factory?layout=plant`;
  const navigation = await cdp.send("Page.navigate", { url });
  try {
    await waitFor(
      { cdp },
      "Plant probe",
      extra.exactTopology
        ? `Boolean(window.__plantProbe && document.querySelector(".factory-forced-colors-exact"))`
        : `Boolean(window.__plantProbe && document.querySelector('[aria-label="Factory plant"]'))`,
      15_000,
    );
  } catch (error) {
    const state = await evaluate(
      { cdp },
      `({
        href: location.href,
        readyState: document.readyState,
        title: document.title,
        text: document.body?.innerText?.slice(0, 500),
        hasProbe: Boolean(window.__plantProbe),
        renderer: document.querySelector('.factory-plant-renderer')?.getAttribute('data-webgl')
      })`,
    ).catch((evaluationError) => ({ evaluationError: evaluationError.message }));
    throw new Error(
      `${error.message} Navigation=${JSON.stringify(navigation)} Loading=${JSON.stringify(
        loadingEvents,
      )} State=${JSON.stringify(state)} CDP=${JSON.stringify(
        diagnostics.cdpErrors,
      )} Console=${JSON.stringify(diagnostics.browserConsole.slice(-30))}`,
    );
  }
  if (expectedState !== "exact") {
    await waitFor(
      { cdp },
      `Plant renderer ${expectedState}`,
      `window.__plantProbe.snapshot().renderer.state === ${JSON.stringify(expectedState)}`,
      15_000,
    );
  }
  if (expectedState === "ready") {
    await evaluate({ cdp }, `window.__plantProbe.waitForFrames(2, 5000)`);
  }
  return { cdp };
}

async function installFixtures(cdp, variant = "default") {
  const fixtureState = { eventRequests: 0, variant };
  await cdp.send("Fetch.enable", {
    patterns: [
      { urlPattern: "*://*/api/v1/*", requestStage: "Request" },
      { urlPattern: "*://*/__plant-harness/*", requestStage: "Request" },
    ],
  });
  cdp.on("Fetch.requestPaused", async (event) => {
    const response = plantFixtureResponse(event.request.url, fixtureState);
    if (
      (variant === "lagging" || variant === "partial") &&
      response.status >= 200 &&
      response.status < 300 &&
      response.headers.some(
        (header) =>
          header.name.toLowerCase() === "content-type" &&
          header.value.includes("application/json"),
      )
    ) {
      const value = JSON.parse(response.body);
      value.readState =
        variant === "lagging"
          ? {
              epoch: "plant-harness",
              appliedSeq: 90,
              lagSeconds: 120,
              completeness: "complete",
              missing: [],
              degraded: ["repair sweep behind"],
            }
          : {
              epoch: "plant-harness",
              appliedSeq: 95,
              lagSeconds: 4,
              completeness: "partial",
              missing: [
                {
                  name: "run-signals",
                  reason: "partition unavailable",
                  expectedBy: "2026-08-03T22:01:00Z",
                },
              ],
              degraded: [],
            };
      response.body = JSON.stringify(value);
    }
    try {
      await cdp.send("Fetch.fulfillRequest", {
        requestId: event.requestId,
        responseCode: response.status,
        responseHeaders: response.headers,
        body: Buffer.from(response.body, "utf8").toString("base64"),
      });
    } catch (error) {
      if (!(error instanceof Error) || !error.message.includes("Invalid InterceptionId")) {
        throw error;
      }
    }
  });
}

async function captureScenario(page, name) {
  currentScenario = name;
  await delay(250);
  const snapshot = await evaluate(page, `window.__plantProbe.snapshot()`);
  const clip = await evaluate(
    page,
    `(() => {
      const element =
        window.innerHeight <= 600
          ? document.querySelector(".factory-page")
          : document.querySelector(".factory-plant-renderer");
      if (!(element instanceof HTMLElement)) return undefined;
      const bounds = element.getBoundingClientRect();
      const left = Math.max(0, bounds.left);
      const top = Math.max(0, bounds.top);
      const right = Math.min(window.innerWidth, bounds.right);
      const bottom = Math.min(window.innerHeight, bounds.bottom);
      return {
        x: left,
        y: top,
        width: Math.max(1, right - left),
        height: Math.max(1, bottom - top),
        scale: 1
      };
    })()`,
  );
  const screenshot = await page.cdp.send("Page.captureScreenshot", {
    format: "png",
    fromSurface: true,
    captureBeyondViewport: false,
    ...(clip ? { clip } : {}),
  });
  const screenshotPath = join(outputRoot, `${name}.png`);
  screenshotData[name] = screenshot.data;
  await writeFile(screenshotPath, Buffer.from(screenshot.data, "base64"));
  const image = await evaluate(
    page,
    `(async () => {
      const image = new Image();
      image.src = ${JSON.stringify(`data:image/png;base64,${screenshot.data}`)};
      await image.decode();
      const canvas = document.createElement("canvas");
      canvas.width = image.naturalWidth;
      canvas.height = image.naturalHeight;
      const context = canvas.getContext("2d", { willReadFrequently: true });
      if (!context) throw new Error("Unable to sample Plant screenshot.");
      context.drawImage(image, 0, 0);
      const pixels = context.getImageData(0, 0, canvas.width, canvas.height).data;
      let luminance = 0;
      let dark = 0;
      let nearBlack = 0;
      let chromatic = 0;
      let warm = 0;
      let bright = 0;
      let samples = 0;
      const stride = Math.max(1, Math.floor(Math.sqrt((canvas.width * canvas.height) / 20000)));
      for (let y = 0; y < canvas.height; y += stride) {
        for (let x = 0; x < canvas.width; x += stride) {
          const offset = (y * canvas.width + x) * 4;
          const r = pixels[offset];
          const g = pixels[offset + 1];
          const b = pixels[offset + 2];
          const value = (r * 0.2126 + g * 0.7152 + b * 0.0722) / 255;
          luminance += value;
          dark += value < 0.2 ? 1 : 0;
          nearBlack += value < 0.06 ? 1 : 0;
          bright += value > 0.94 ? 1 : 0;
          // Neutral-first art direction means a chromatic pixel is a status
          // pixel: a beacon, a marking, or an accent trim.
          const spread = Math.max(r, g, b) - Math.min(r, g, b);
          chromatic += spread >= 40 ? 1 : 0;
          warm += r >= g + 30 && r >= b + 30 ? 1 : 0;
          samples += 1;
        }
      }
      return {
        width: canvas.width,
        height: canvas.height,
        meanLuminance: luminance / samples,
        darkPixelRatio: dark / samples,
        nearBlackRatio: nearBlack / samples,
        brightPixelRatio: bright / samples,
        chromaticRatio: chromatic / samples,
        warmPixelRatio: warm / samples,
        warmPixels: warm,
        samples
      };
    })()`,
  );
  return {
    image,
    screenshot: screenshotPath,
    snapshot,
  };
}

async function resetProbe(page) {
  await evaluate(page, `window.__plantProbe.reset()`);
}

async function compareScreenshots(page, left, right) {
  return evaluate(
    page,
    `(async () => {
      const load = async (data) => {
        const image = new Image();
        image.src = "data:image/png;base64," + data;
        await image.decode();
        const canvas = document.createElement("canvas");
        canvas.width = image.naturalWidth;
        canvas.height = image.naturalHeight;
        const context = canvas.getContext("2d", { willReadFrequently: true });
        context.drawImage(image, 0, 0);
        return {
          width: canvas.width,
          height: canvas.height,
          pixels: context.getImageData(0, 0, canvas.width, canvas.height).data,
        };
      };
      const a = await load(${JSON.stringify(left)});
      const b = await load(${JSON.stringify(right)});
      const width = Math.min(a.width, b.width);
      const height = Math.min(a.height, b.height);
      const deltas = [];
      let changed = 0;
      let samples = 0;
      const stride = Math.max(1, Math.floor(Math.sqrt((width * height) / 50000)));
      for (let y = 0; y < height; y += stride) {
        for (let x = 0; x < width; x += stride) {
          const ao = (y * a.width + x) * 4;
          const bo = (y * b.width + x) * 4;
          const delta = Math.max(
            Math.abs(a.pixels[ao] - b.pixels[bo]),
            Math.abs(a.pixels[ao + 1] - b.pixels[bo + 1]),
            Math.abs(a.pixels[ao + 2] - b.pixels[bo + 2]),
          );
          deltas.push(delta);
          changed += delta >= 12 ? 1 : 0;
          samples += 1;
        }
      }
      deltas.sort((x, y) => x - y);
      return {
        changedPixelRatio: changed / samples,
        p50ChannelDelta: deltas[Math.floor(deltas.length * 0.5)] ?? 0,
        p95ChannelDelta: deltas[Math.floor(deltas.length * 0.95)] ?? 0,
        samples,
      };
    })()`,
  );
}

async function readRiskHierarchy(page) {
  return evaluate(
    page,
    `(() => {
      const expected = {
        blocked: "stop-octagon",
        held: "pause-bars",
        impeded: "warning-triangle",
        unknown: "open-diamond",
      };
      return {
        levels: Object.entries(expected).map(([level, expectedShape]) => {
          const badges = [...document.querySelectorAll(
            '.factory-plant-risk-badge[data-risk="' + level + '"]',
          )];
          return {
            level,
            expectedShape,
            labels: [...new Set(badges.map((badge) => badge.textContent.trim()))],
            shapes: [...new Set(badges.map((badge) => badge.getAttribute("data-shape")))],
          };
        }),
      };
    })()`,
  );
}

async function measureSemanticContrast(page) {
  return evaluate(
    page,
    `(() => {
      const channels = (value) => {
        const match = String(value).match(/rgba?\\(([^)]+)\\)/i);
        if (!match) return undefined;
        return match[1].split(/[ ,/]+/).slice(0, 3).map(Number);
      };
      const luminance = (value) => {
        const rgb = channels(value);
        if (!rgb || rgb.some((channel) => !Number.isFinite(channel))) return undefined;
        const linear = rgb.map((channel) => {
          const v = channel / 255;
          return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
        });
        return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
      };
      const ratio = (foreground, background) => {
        const a = luminance(foreground);
        const b = luminance(background);
        return a === undefined || b === undefined
          ? 0
          : (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
      };
      const label = document.querySelector(".factory-plant-overlay-label");
      const badge = document.querySelector(".factory-plant-risk-badge");
      const marker = document.querySelector(".factory-plant-risk-badge > i");
      const ring = document.querySelector(".factory-plant-overlay-ring");
      const selected = document.querySelector(
        '.factory-plant-overlay-item[data-selected="true"] .factory-plant-overlay-label',
      );
      const labelStyle = label ? getComputedStyle(label) : undefined;
      const badgeStyle = badge ? getComputedStyle(badge) : undefined;
      const markerStyle = marker ? getComputedStyle(marker) : undefined;
      const ringStyle = ring ? getComputedStyle(ring) : undefined;
      const selectedStyle = selected ? getComputedStyle(selected) : undefined;
      const background = labelStyle?.backgroundColor ?? "rgb(255,255,255)";
      const markerColour =
        markerStyle?.borderColor !== "rgba(0, 0, 0, 0)" &&
        markerStyle?.borderStyle !== "none"
          ? markerStyle?.borderColor
          : badgeStyle?.color;
      return {
        label: ratio(labelStyle?.color, background),
        marker: ratio(markerColour, background),
        focus: ratio(ringStyle?.borderColor, background),
        selection: ratio(selectedStyle?.borderColor, selectedStyle?.backgroundColor),
        sampled: {
          label: labelStyle?.color,
          labelBackground: background,
          marker: markerColour,
          focus: ringStyle?.borderColor,
          selection: selectedStyle?.borderColor,
        },
      };
    })()`,
  );
}

/**
 * Contrast measured on the pixels the operator actually sees.
 *
 * The palette gates are arithmetic on authored albedo, which a lighting rig can
 * silently violate: a body authored at 5:1 against the deck can render as a
 * black blob once its vertical faces fall away from the key. This samples the
 * rendered frame at the projected machine anchors - the same anchors the
 * alignment checks prove are within 6px of the geometry - and reports the real
 * ratio, so the rig cannot drift away from the palette unnoticed.
 */
async function measureRenderedContrast(page) {
  await evaluate(
    page,
    `(() => {
      const style = document.createElement("style");
      style.id = "plant-contrast-sampling";
      style.textContent =
        ".factory-plant-overlay-label,.factory-plant-overlay-chip,.factory-plant-overlay-ring{visibility:hidden!important}";
      document.head.append(style);
    })()`,
  );
  let screenshot;
  try {
    screenshot = await page.cdp.send("Page.captureScreenshot", {
      format: "png",
      fromSurface: true,
      captureBeyondViewport: false,
    });
  } finally {
    await evaluate(
      page,
      `document.querySelector("#plant-contrast-sampling")?.remove()`,
    );
  }
  return evaluate(
    page,
    `(async () => {
      const image = new Image();
      image.src = ${JSON.stringify(`data:image/png;base64,${screenshot.data}`)};
      await image.decode();
      const canvas = document.createElement("canvas");
      canvas.width = image.naturalWidth;
      canvas.height = image.naturalHeight;
      const context = canvas.getContext("2d", { willReadFrequently: true });
      context.drawImage(image, 0, 0);
      const scaleX = canvas.width / window.innerWidth;
      const scaleY = canvas.height / window.innerHeight;
      const pixels = context.getImageData(0, 0, canvas.width, canvas.height).data;
      const luminanceAt = (x, y) => {
        const px = Math.round(x * scaleX);
        const py = Math.round(y * scaleY);
        if (px < 0 || py < 0 || px >= canvas.width || py >= canvas.height) return undefined;
        const offset = (py * canvas.width + px) * 4;
        const channel = (value) => {
          const v = value / 255;
          return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
        };
        return (
          0.2126 * channel(pixels[offset]) +
          0.7152 * channel(pixels[offset + 1]) +
          0.0722 * channel(pixels[offset + 2])
        );
      };
      const ratio = (a, b) =>
        (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
      const percentile = (values, fraction) => {
        if (!values.length) return undefined;
        const sorted = [...values].sort((a, b) => a - b);
        return sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * fraction))];
      };

      const host = document.querySelector(".factory-plant-renderer");
      const bounds = host.getBoundingClientRect();
      const neutral = (cx, cy) => {
        const px = Math.round(cx * scaleX);
        const py = Math.round(cy * scaleY);
        if (px < 0 || py < 0 || px >= canvas.width || py >= canvas.height) return false;
        const offset = (py * canvas.width + px) * 4;
        const r = pixels[offset];
        const g = pixels[offset + 1];
        const b = pixels[offset + 2];
        return Math.max(r, g, b) - Math.min(r, g, b) < 30;
      };

      const machines = [];
      const anchors = document.querySelectorAll(
        '.factory-plant-overlay-item[data-kind="station"] [data-plant-anchor-origin]',
      );
      for (const node of anchors) {
        const rect = node.getBoundingClientRect();
        const cx = rect.left + rect.width / 2;
        const cy = rect.top + rect.height / 2;
        const stationControl = node.closest("[data-plant-anchor-id]");
        const openGate = stationControl?.getAttribute("data-stage-kind") === "gate";
        /*
         * Local deck, not a global median: an aisle or a wall elsewhere in the
         * frame is not what this machine has to stand out from. Sampled to both
         * sides so a machine near a bay edge still gets a fair reading.
         */
        const deckPatch = [];
        for (const dx of [-26, -22, -18, 18, 22, 26]) {
          for (const dy of [2, 6, 10]) {
            const value = luminanceAt(cx + dx, cy + dy);
            if (value !== undefined) deckPatch.push(value);
          }
        }
        if (!deckPatch.length) continue;
        deckPatch.sort((a, b) => a - b);
        const deck = deckPatch[Math.floor(deckPatch.length / 2)];
        /*
         * Representative foreground mask, never a best pixel. The mask covers
         * the machine's projected body, excludes chromatic status markers, and
         * segments neutral foreground from the local deck by a fixed luminance
         * distance. The lower quintile is the gate, so one bright edge or dark
         * outlier cannot rescue an otherwise unreadable machine.
         */
        const candidates = new Map();
        for (let dy = -12; dy <= 10; dy += 1) {
          for (let dx = -8; dx <= 8; dx += 1) {
            if (
              openGate &&
              (Math.abs(dx) < 4 || dy < -4 || dy > 6)
            ) {
              continue;
            }
            const value = luminanceAt(cx + dx, cy + dy);
            if (value === undefined || !neutral(cx + dx, cy + dy)) continue;
            if (Math.abs(value - deck) < 0.045) continue;
            candidates.set(dx + "," + dy, { dx, dy, value });
          }
        }
        const components = [];
        const remaining = new Map(candidates);
        while (remaining.size > 0) {
          const first = remaining.entries().next().value;
          if (!first) break;
          const [firstKey, firstPixel] = first;
          remaining.delete(firstKey);
          const queue = [firstPixel];
          const component = [];
          while (queue.length > 0) {
            const pixel = queue.pop();
            component.push(pixel);
            for (const [nx, ny] of [
              [pixel.dx - 1, pixel.dy],
              [pixel.dx + 1, pixel.dy],
              [pixel.dx, pixel.dy - 1],
              [pixel.dx, pixel.dy + 1],
            ]) {
              const key = nx + "," + ny;
              const neighbour = remaining.get(key);
              if (!neighbour) continue;
              remaining.delete(key);
              queue.push(neighbour);
            }
          }
          if (
            component.length >= 6 &&
            component.some(
              (pixel) => Math.abs(pixel.dx) <= 6 && pixel.dy >= -10 && pixel.dy <= 9,
            )
          ) {
            components.push(component);
          }
        }
        components.sort((left, right) => right.length - left.length);
        // The largest contiguous neutral component is the machine body.
        // A second component is commonly the cast floor shadow or a detached
        // neutral marker, neither of which is part of silhouette contrast.
        const bodyPixels = components.slice(0, 1).flat();
        const bodyValues = bodyPixels.map((pixel) => pixel.value);
        const bodyRatios = bodyValues.map((value) => ratio(value, deck));
        if (bodyRatios.length < 8) continue;
        const body = percentile(bodyValues, 0.5);
        const p20 = percentile(bodyRatios, 0.2);
        const median = percentile(bodyRatios, 0.5);
        if (body === undefined || p20 === undefined || median === undefined) continue;
        machines.push({
          body: Math.round(body * 10000) / 10000,
          deck: Math.round(deck * 10000) / 10000,
          gate: openGate,
          maskPixels: bodyRatios.length,
          key:
            node.closest("[data-plant-anchor-id]")?.getAttribute("data-plant-anchor-id") ??
            node.textContent.trim(),
          p20VsDeck: Math.round(p20 * 100) / 100,
          medianVsDeck: Math.round(median * 100) / 100,
        });
      }
      const ratios = machines.map((machine) => machine.p20VsDeck);
      const deckAll = [];
      for (let ty = 0.25; ty <= 0.8; ty += 0.05) {
        for (let tx = 0.15; tx <= 0.85; tx += 0.05) {
          const value = luminanceAt(
            bounds.left + bounds.width * tx,
            bounds.top + bounds.height * ty,
          );
          if (value !== undefined) deckAll.push(value);
        }
      }
      deckAll.sort((a, b) => a - b);
      return {
        deck: Math.round(deckAll[Math.floor(deckAll.length / 2)] * 10000) / 10000,
        machines,
        p20MachineVsDeck: ratios.length > 0 ? Math.min(...ratios) : undefined,
        medianMachineVsDeck: percentile(ratios, 0.5),
        sampled: machines.length,
      };
    })()`,
  );
}

/**
 * Time from losing the GL context to a complete, usable DOM plant.
 *
 * Runs entirely in the page so the measurement is not inflated by CDP
 * round-trips. Polls on animation frames because that is the granularity at
 * which React can actually have committed the fallback.
 */
async function measureFallbackLatency(page) {
  return evaluate(
    page,
    `(async () => {
      const complete = () => {
        const root = document.querySelector(".factory-plant-renderer");
        return {
          authoredBackdrop: Boolean(
            document.querySelector(".factory-plant-backdrop-authored"),
          ),
          controls: document.querySelectorAll(
            ".factory-plant-exact-fallback button[aria-label]",
          ).length,
          machines: document.querySelectorAll(
            ".factory-plant-exact-fallback .factory-station",
          ).length,
          state: root ? root.getAttribute("data-webgl") : undefined,
        };
      };
      const started = performance.now();
      const lost = window.__plantProbe.loseContext();
      let seen = complete();
      let elapsed = performance.now() - started;
      const deadline = started + 2000;
      while (
        performance.now() < deadline &&
        !(
          seen.state === "fallback" &&
          seen.authoredBackdrop &&
          seen.machines > 0 &&
          seen.controls > 0
        )
      ) {
        await new Promise((resolve) => requestAnimationFrame(() => resolve()));
        seen = complete();
        elapsed = performance.now() - started;
      }
      return {
        ...seen,
        lost,
        completeAfterMs:
          seen.state === "fallback" ? Math.round(elapsed * 100) / 100 : undefined,
      };
    })()`,
  );
}

/**
 * The Plant must not guess how fresh its data is.
 *
 * Both readings are taken in one evaluate so a poll landing between them cannot
 * manufacture a false mismatch.
 */
async function readFreshnessLink(page) {
  return evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.snapshot();
      const chip = document.querySelector(".factory-freshness");
      return {
        dom: chip ? chip.getAttribute("data-state") ?? undefined : undefined,
        plant: snapshot.model ? snapshot.model.freshness : undefined,
        forcedColors: snapshot.model ? snapshot.model.forcedColors : undefined,
        label: chip ? chip.textContent.trim() : undefined,
      };
    })()`,
  );
}

/**
 * What an operator can actually see and use when WebGL is not drawing.
 *
 * "Fallback" only counts if the plant is still on screen: an authored backdrop,
 * the machine silhouettes, and the DOM controls that carry selection.
 */
async function readFallbackDom(page) {
  return evaluate(
    page,
    `(() => ({
      authoredBackdrop: Boolean(
        document.querySelector(".factory-plant-backdrop-authored"),
      ),
      bitmap: Boolean(document.querySelector("img.factory-plant-backdrop")),
      controls: document.querySelectorAll(
        ".factory-plant-exact-fallback button[aria-label]",
      ).length,
      machines: document.querySelectorAll(
        ".factory-plant-exact-fallback .factory-station",
      ).length,
      legend: Boolean(document.querySelector(".factory-legend")),
    }))()`,
  );
}

/** Maximum tolerated distance between a projected anchor and its DOM control. */
const PROJECTION_TOLERANCE_PX = 6;
const MIN_USABLE_PLANT_HEIGHT = 84;
const MIN_USABLE_PLANT_WIDTH = 240;

/**
 * The viewport sizes alignment is proved at.
 *
 * 1440x1000 is the reference window; the other two exist because a projection
 * that only lines up at one size is not a projection, it is a coincidence.
 */
const ALIGNMENT_VIEWPORTS = [
  { height: 1000, width: 1440 },
  { height: 800, width: 1280 },
  { height: 900, width: 1100 },
];
const ZOOM_LEVELS = [0.6, 0.8, 1, 1.25, 2];

function alignmentLabel(viewport, inspectorOpen) {
  return `${viewport.width}x${viewport.height} inspector ${
    inspectorOpen ? "open" : "closed"
  }`;
}

function summarizeAlignment(entry) {
  return {
    label: entry.label,
    inspectorOpen: entry.inspectorOpen,
    source: entry.overlay?.source,
    entries: entry.overlay?.entries,
    domMaxDrift: entry.dom?.max,
    domMeanDrift: entry.dom?.mean,
    projectionMaxDrift: entry.projection?.maxDrift,
    collisions: entry.overlay?.collisions,
    clipped: entry.overlay?.clipped,
    collapsed: entry.overlay?.collapsed,
    chips: entry.overlay?.chips,
    occlusion: entry.overlay?.occlusion,
  };
}

function worstAlignment(entries, select) {
  return entries.reduce(
    (worst, entry) => {
      const value = select(entry);
      return value > worst.value ? { label: entry.label, value } : worst;
    },
    { label: "none", value: 0 },
  );
}

/**
 * Measures the projection contract at one viewport and inspector state.
 *
 * Everything asserted on is read from the page after a selection is made, so a
 * drift, a label collision or an occluded selection is measured under exactly
 * the conditions a person would hit.
 */
async function measureAlignment(page, { height, inspectorOpen, width }) {
  await page.cdp.send("Emulation.setDeviceMetricsOverride", {
    deviceScaleFactor: 1,
    height,
    mobile: false,
    width,
  });
  await delay(400);
  await setInspector(page, inspectorOpen);
  await selectFirstOverlayItem(page, inspectorOpen);
  await setInspector(page, inspectorOpen);
  await evaluate(page, `window.__plantProbe.remeasure() && true`).catch(() => {});
  const preFitSelectedOcclusion = await evaluate(
    page,
    `window.__plantProbe.snapshot().overlay?.occlusion?.selected ?? 0`,
  );
  await evaluate(page, `document.querySelector(".factory-viewport-fit")?.click()`);
  await delay(450);
  await evaluate(page, `window.__plantProbe.waitForFrames(2, 5000)`).catch(() => {});
  await delay(200);
  await evaluate(page, `window.__plantProbe.remeasure() && true`).catch(() => {});
  const measurement = await evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.snapshot();
      const overlay = snapshot.overlay;
      const viewportElement = document.querySelector(".factory-viewport");
      const viewportBounds = viewportElement?.getBoundingClientRect();
      const drawerBounds = document
        .querySelector(".factory-inspector-drawer.is-open")
        ?.getBoundingClientRect();
      const drifts = [];
      const worst = [];
      for (const entry of overlay?.entries ?? []) {
        const node = document.querySelector(
          '[data-plant-anchor-id="' + entry.anchorId + '"] [data-plant-anchor-origin]',
        );
        if (!node || !viewportBounds) continue;
        const bounds = node.getBoundingClientRect();
        const domX = bounds.left + bounds.width / 2 - viewportBounds.left;
        const domY = bounds.top + bounds.height / 2 - viewportBounds.top;
        const dx = domX - entry.projected.x;
        const dy = domY - entry.projected.y;
        const distance = Math.hypot(dx, dy);
        drifts.push(distance);
        worst.push({ id: entry.id, distance, dom: { x: domX, y: domY }, projected: entry.projected });
      }
      worst.sort((left, right) => right.distance - left.distance);
      const world = document.querySelector(".factory-viewport-world");
      const worldBounds = world?.getBoundingClientRect();
      const safeArea = snapshot.viewport?.safeArea;
      const contained = Boolean(
        worldBounds && viewportBounds && safeArea &&
        worldBounds.left - viewportBounds.left >= safeArea.x - 1 &&
        worldBounds.top - viewportBounds.top >= safeArea.y - 1 &&
        worldBounds.right - viewportBounds.left <= safeArea.x + safeArea.width + 1 &&
        worldBounds.bottom - viewportBounds.top <= safeArea.y + safeArea.height + 1,
      );
      return {
        actualInspector:
          drawerBounds && viewportBounds
            ? {
                x: drawerBounds.left - viewportBounds.left,
                y: drawerBounds.top - viewportBounds.top,
                width: drawerBounds.width,
                height: drawerBounds.height,
              }
            : undefined,
        dom: {
          count: drifts.length,
          max: drifts.length ? Math.max(...drifts) : 0,
          mean: drifts.length ? drifts.reduce((sum, value) => sum + value, 0) / drifts.length : 0,
          worst: worst.slice(0, 5),
        },
        fit: {
          contained,
          safeArea,
          world: worldBounds && viewportBounds ? {
            left: worldBounds.left - viewportBounds.left,
            top: worldBounds.top - viewportBounds.top,
            right: worldBounds.right - viewportBounds.left,
            bottom: worldBounds.bottom - viewportBounds.top,
          } : undefined,
        },
        inspector: snapshot.viewport?.inspector,
        preFitSelectedOcclusion: ${preFitSelectedOcclusion},
        overlay: overlay ? {
          chips: overlay.chips,
          clipped: overlay.clipped,
          collapsed: overlay.collapsed,
          collisions: overlay.collisions,
          entries: overlay.entries.length,
          hitTargets: overlay.hitTargets,
          occlusion: overlay.occlusion,
          offscreen: overlay.offscreen,
          safeArea: overlay.safeArea,
          source: overlay.source,
        } : undefined,
        projection: {
          count: snapshot.projection.entries.length,
          maxDrift: snapshot.projection.maxDrift,
          meanDrift: snapshot.projection.meanDrift,
        },
        rendererState: snapshot.renderer.state,
        safeArea: snapshot.viewport?.safeArea ?? { x: 0, y: 0, width: 0, height: 0 },
        selected: (overlay?.entries ?? []).filter((entry) => entry.selected).length,
        viewport: {
          width: snapshot.viewport?.viewport.width ?? 0,
          height: snapshot.viewport?.viewport.height ?? 0,
        },
      };
    })()`,
  );
  return {
    ...measurement,
    inspectorOpen,
    label: alignmentLabel({ height, width }, inspectorOpen),
    requested: { height, width },
  };
}

async function measureZoomPacking(page, zoom) {
  await page.cdp.send("Emulation.setDeviceMetricsOverride", {
    deviceScaleFactor: 1,
    height: 800,
    mobile: false,
    width: 1280,
  });
  await delay(300);
  await setInspector(page, true);
  await selectFirstOverlayItem(page);
  const applied = await evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.snapshot();
      const camera = snapshot.viewport?.camera;
      const safe = snapshot.viewport?.safeArea;
      if (!camera || !safe) return false;
      const centerX = safe.x + safe.width / 2;
      const centerY = safe.y + safe.height / 2;
      const worldX = (centerX - camera.x) / camera.zoom;
      const worldY = (centerY - camera.y) / camera.zoom;
      return window.__plantProbe.setViewportCamera({
        x: centerX - worldX * ${JSON.stringify(zoom)},
        y: centerY - worldY * ${JSON.stringify(zoom)},
        zoom: ${JSON.stringify(zoom)}
      });
    })()`,
  );
  if (!applied) {
    throw new Error("Plant probe could not apply the requested viewport camera.");
  }
  await waitFor(
    page,
    `Plant zoom ${zoom}`,
    `Math.abs(window.__plantProbe.snapshot().viewport.camera.zoom - ${JSON.stringify(zoom)}) < 0.001`,
    5_000,
  );
  // Re-select after the manual pose so the existing "selected stays visible"
  // contract gets a chance to pan, without changing the requested zoom.
  await selectFirstOverlayItem(page);
  await delay(250);
  await evaluate(page, `window.__plantProbe.remeasure() && true`);
  const measurement = await evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.snapshot();
      const overlay = snapshot.overlay;
      const viewport = document.querySelector(".factory-viewport");
      const viewportBounds = viewport?.getBoundingClientRect();
      if (!overlay || !viewportBounds) return undefined;
      const relativeRect = (element) => {
        const bounds = element.getBoundingClientRect();
        return {
          x: bounds.left - viewportBounds.left,
          y: bounds.top - viewportBounds.top,
          width: bounds.width,
          height: bounds.height,
          right: bounds.right - viewportBounds.left,
          bottom: bounds.bottom - viewportBounds.top,
        };
      };
      const overlaps = (left, right) =>
        left.x < right.right - 0.01 &&
        left.right > right.x + 0.01 &&
        left.y < right.bottom - 0.01 &&
        left.bottom > right.y + 0.01;
      const packedRects = Array.from(
        document.querySelectorAll(
          ".factory-plant-overlay-label, .factory-plant-overlay-chip",
        ),
      )
        .map(relativeRect)
        .filter((rect) => rect.width > 0 && rect.height > 0);
      let collisions = 0;
      for (let left = 0; left < packedRects.length; left += 1) {
        for (let right = left + 1; right < packedRects.length; right += 1) {
          collisions += overlaps(packedRects[left], packedRects[right]) ? 1 : 0;
        }
      }

      const probeEntries = new Map(
        overlay.entries.map((entry) => [entry.anchorId, entry]),
      );
      let maxRectDelta = 0;
      let minHitWidth = Infinity;
      let minHitHeight = Infinity;
      let total = 0;
      let critical = 0;
      let selected = 0;
      const occluded = [];
      const inspector = snapshot.viewport?.inspector
        ? {
            x: snapshot.viewport.inspector.x,
            y: snapshot.viewport.inspector.y,
            width: snapshot.viewport.inspector.width,
            height: snapshot.viewport.inspector.height,
            right:
              snapshot.viewport.inspector.x +
              snapshot.viewport.inspector.width,
            bottom:
              snapshot.viewport.inspector.y +
              snapshot.viewport.inspector.height,
          }
        : undefined;
      for (const item of document.querySelectorAll(
        '.factory-plant-overlay-item[data-onscreen="true"]',
      )) {
        const hitNode = item.querySelector(".factory-plant-overlay-hit");
        if (!hitNode) continue;
        const hit = relativeRect(hitNode);
        minHitWidth = Math.min(minHitWidth, hit.width);
        minHitHeight = Math.min(minHitHeight, hit.height);
        const entry = probeEntries.get(item.getAttribute("data-plant-anchor-id"));
        if (entry) {
          maxRectDelta = Math.max(
            maxRectDelta,
            Math.abs(hit.x - entry.hit.x),
            Math.abs(hit.y - entry.hit.y),
            Math.abs(hit.width - entry.hit.width),
            Math.abs(hit.height - entry.hit.height),
          );
        }
        if (inspector && overlaps(hit, inspector)) {
          total += 1;
          critical += item.getAttribute("data-critical") === "true" ? 1 : 0;
          selected += item.getAttribute("data-selected") === "true" ? 1 : 0;
          occluded.push({
            anchorId: item.getAttribute("data-plant-anchor-id"),
            critical: item.getAttribute("data-critical") === "true",
            kind: item.getAttribute("data-kind"),
            selected: item.getAttribute("data-selected") === "true",
          });
        }
      }
      for (const chip of document.querySelectorAll(
        ".factory-plant-overlay-chip",
      )) {
        const hit = relativeRect(chip);
        minHitWidth = Math.min(minHitWidth, hit.width);
        minHitHeight = Math.min(minHitHeight, hit.height);
      }
      return {
        actual: {
          collisions,
          maxRectDelta,
          minHitHeight: Number.isFinite(minHitHeight) ? minHitHeight : 0,
          minHitWidth: Number.isFinite(minHitWidth) ? minHitWidth : 0,
          occlusion: { total, critical, selected, entries: occluded },
        },
        camera: snapshot.viewport.camera,
        probe: {
          collisions: overlay.collisions,
          hitTargets: overlay.hitTargets,
          occlusion: overlay.occlusion,
        },
      };
    })()`,
  );
  if (!measurement) {
    throw new Error(`Unable to measure Plant packing at zoom ${zoom}.`);
  }
  return { ...measurement, zoom };
}

async function measureRigidNavigation(page) {
  await setInspector(page, false);
  await evaluate(page, `document.querySelector(".factory-viewport-fit")?.click()`);
  await delay(350);
  await evaluate(page, `window.__plantProbe.remeasure() && true`);
  const before = await evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.snapshot();
      const viewport = document.querySelector(".factory-viewport");
      const viewportBounds = viewport?.getBoundingClientRect();
      const anchor = document.querySelector(
        '.factory-plant-overlay-item[data-kind="station"] [data-plant-anchor-origin]',
      );
      const anchorBounds = anchor?.getBoundingClientRect();
      const projection = snapshot.projection.entries.find(
        (entry) => entry.kind === "station",
      );
      return {
        camera: snapshot.viewport.camera,
        overlayRevision: snapshot.overlay.revision,
        anchor: anchorBounds && viewportBounds
          ? {
              x: anchorBounds.left + anchorBounds.width / 2 - viewportBounds.left,
              y: anchorBounds.top + anchorBounds.height / 2 - viewportBounds.top,
            }
          : undefined,
        projection: projection
          ? { id: projection.id, x: projection.actual.x, y: projection.actual.y }
          : undefined,
      };
    })()`,
  );
  const delta = { x: 47, y: -31 };
  await evaluate(
    page,
    `window.__plantProbe.setViewportCamera({
      x: ${JSON.stringify(before.camera.x + delta.x)},
      y: ${JSON.stringify(before.camera.y + delta.y)},
      zoom: ${JSON.stringify(before.camera.zoom)}
    })`,
  );
  await delay(300);
  await evaluate(page, `window.__plantProbe.remeasure() && true`);
  const after = await evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.snapshot();
      const viewport = document.querySelector(".factory-viewport");
      const viewportBounds = viewport?.getBoundingClientRect();
      const anchor = document.querySelector(
        '.factory-plant-overlay-item[data-kind="station"] [data-plant-anchor-origin]',
      );
      const anchorBounds = anchor?.getBoundingClientRect();
      const projection = snapshot.projection.entries.find(
        (entry) => entry.id === ${JSON.stringify(before.projection?.id)},
      );
      return {
        camera: snapshot.viewport.camera,
        overlayRevision: snapshot.overlay.revision,
        anchor: anchorBounds && viewportBounds
          ? {
              x: anchorBounds.left + anchorBounds.width / 2 - viewportBounds.left,
              y: anchorBounds.top + anchorBounds.height / 2 - viewportBounds.top,
            }
          : undefined,
        projection: projection
          ? { id: projection.id, x: projection.actual.x, y: projection.actual.y }
          : undefined,
      };
    })()`,
  );

  const poseBeforeFallback = after.camera;
  const lost = await evaluate(page, `window.__plantProbe.loseContext()`);
  await waitFor(
    page,
    "rigid navigation fallback",
    `window.__plantProbe.snapshot().renderer.state === "fallback"`,
    5_000,
  );
  const lostPose = await evaluate(
    page,
    `window.__plantProbe.snapshot().viewport.camera`,
  );
  const restored = await evaluate(page, `window.__plantProbe.restoreContext()`);
  await waitFor(
    page,
    "rigid navigation restore",
    `window.__plantProbe.snapshot().renderer.state === "ready"`,
    5_000,
  );
  const restoredPose = await evaluate(
    page,
    `window.__plantProbe.snapshot().viewport.camera`,
  );

  const close = (left, right) => Math.abs(left - right) <= 0.05;
  return {
    before,
    after,
    delta,
    projectionRevisionStable:
      before.overlayRevision === after.overlayRevision,
    localProjectionStable:
      Boolean(before.projection && after.projection) &&
      close(before.projection.x, after.projection.x) &&
      close(before.projection.y, after.projection.y),
    anchorDeltaMatches:
      Boolean(before.anchor && after.anchor) &&
      close(after.anchor.x - before.anchor.x, delta.x) &&
      close(after.anchor.y - before.anchor.y, delta.y),
    fallbackPoseStable:
      lost === true &&
      restored === true &&
      close(lostPose.x, poseBeforeFallback.x) &&
      close(lostPose.y, poseBeforeFallback.y) &&
      close(lostPose.zoom, poseBeforeFallback.zoom) &&
      close(restoredPose.x, poseBeforeFallback.x) &&
      close(restoredPose.y, poseBeforeFallback.y) &&
      close(restoredPose.zoom, poseBeforeFallback.zoom),
  };
}

async function measureSmallSafeArea(page, requested) {
  await page.cdp.send("Emulation.setDeviceMetricsOverride", {
    deviceScaleFactor: 1,
    height: requested.height,
    mobile: false,
    width: requested.width,
  });
  await delay(500);
  await setInspector(page, false);
  await evaluate(page, `document.querySelector(".factory-viewport-fit")?.click()`);
  await delay(350);
  const closed = await evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.remeasure();
      const viewport = document.querySelector(".factory-viewport");
      const viewportBounds = viewport?.getBoundingClientRect();
      const safe = snapshot.viewport?.safeArea;
      const visible = (node) => {
        if (!(node instanceof HTMLElement)) return false;
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return rect.width > 0 && rect.height > 0 &&
          style.display !== "none" && style.visibility !== "hidden";
      };
      const critical = [
        ...document.querySelectorAll(".factory-control select"),
        document.querySelector('[aria-label="Floor layout"]'),
        document.querySelector('[aria-label="Floor lens"]'),
      ];
      const trigger = document.querySelector(".factory-inspector-toggle");
      if (!viewportBounds || !safe) {
        return {
          criticalVisible: false,
          documentOverflowX: true,
          documentOverflowY: true,
          safeArea: safe ?? { x: 0, y: 0, width: 0, height: 0 },
          triggerVisible: false,
          viewport: snapshot.viewport?.viewport ?? { width: 0, height: 0 },
        };
      }
      return {
        criticalVisible: critical.every(visible),
        boxes: Object.fromEntries(
          [
            [".page-content-workspace", "page"],
            [".factory-page", "factoryPage"],
            [".factory-heading", "heading"],
            [".factory-layout", "layout"],
            [".factory-stage-area", "stage"],
          ].map(([selector, key]) => {
            const rect = document.querySelector(selector)?.getBoundingClientRect();
            return [key, rect ? { height: rect.height, top: rect.top } : undefined];
          }),
        ),
        documentOverflowX:
          document.documentElement.scrollWidth > document.documentElement.clientWidth + 1,
        documentOverflowY:
          document.documentElement.scrollHeight > document.documentElement.clientHeight + 1,
        safeArea: safe,
        triggerVisible: visible(trigger) && trigger.getAttribute("aria-pressed") === "false",
        viewport: {
          width: viewportBounds.width,
          height: viewportBounds.height,
        },
      };
    })()`,
  );
  await setInspector(page, true);
  const open = await evaluate(
    page,
    `(() => {
      const snapshot = window.__plantProbe.remeasure();
      const viewport = document.querySelector(".factory-viewport");
      const drawer = document.querySelector(".factory-inspector-drawer.is-open");
      const viewportBounds = viewport?.getBoundingClientRect();
      const drawerBounds = drawer?.getBoundingClientRect();
      const safe = snapshot.viewport?.safeArea;
      const inspector = snapshot.viewport?.inspector;
      if (!viewportBounds || !drawerBounds || !safe) {
        return {
          actualSeparated: false,
          safeArea: safe ?? { x: 0, y: 0, width: 0, height: 0 },
          snapshotSeparated: false,
        };
      }
      const actualInspector = {
        x: drawerBounds.left - viewportBounds.left,
        y: drawerBounds.top - viewportBounds.top,
        width: drawerBounds.width,
        height: drawerBounds.height,
      };
      const overlap = (left, right) =>
        left.x < right.x + right.width - 0.01 &&
        left.x + left.width > right.x + 0.01 &&
        left.y < right.y + right.height - 0.01 &&
        left.y + left.height > right.y + 0.01;
      const emptySafeArea = safe.width <= 0 || safe.height <= 0;
      return {
        actualInspector,
        actualSeparated: emptySafeArea || !overlap(safe, actualInspector),
        inspector,
        safeArea: safe,
        snapshotSeparated:
          emptySafeArea || Boolean(inspector && !overlap(safe, inspector)),
      };
    })()`,
  );
  await setInspector(page, false);
  captures[`short${requested.width}x${requested.height}`] =
    await captureScenario(page, `short-${requested.width}x${requested.height}`);
  return { closed, open, requested };
}

/** Drives the real inspector affordances rather than mutating React state. */
async function setInspector(page, open) {
  await evaluate(
    page,
    `(() => {
      const drawer = document.querySelector(".factory-inspector-drawer");
      const isOpen = Boolean(drawer && drawer.classList.contains("is-open"));
      if (isOpen === ${open ? "true" : "false"}) return isOpen;
      const toggle = document.querySelector(".factory-inspector-toggle");
      if (toggle instanceof HTMLElement) toggle.click();
      return !isOpen;
    })()`,
  );
  await delay(350);
}

/** Selects a real semantic so occlusion and focus are measured, not assumed. */
async function selectFirstOverlayItem(page, rightmost = false) {
  await evaluate(
    page,
    `(() => {
      const stations = [...document.querySelectorAll(
        '.factory-plant-overlay-item[data-kind="station"]',
      )];
      const button = ${rightmost ? "true" : "false"}
        ? stations.sort(
            (left, right) =>
              right.getBoundingClientRect().left -
              left.getBoundingClientRect().left,
          )[0]
        : stations[0];
      const item = button?.querySelector(".factory-plant-overlay-hit") ?? button;
      if (item instanceof HTMLElement) {
        item.click();
        return true;
      }
      return false;
    })()`,
  );
  await delay(350);
}

/**
 * Samples the frame loop over a quiet window.
 *
 * A stopped loop is the claim under test, so the measurement reads both the
 * runtime's own motion flag and the frame counter it cannot fake. A single
 * frame is tolerated because a live model refresh legitimately renders once.
 */
async function measureMotion(page, windowMs = 900) {
  const before = await evaluate(page, `window.__plantProbe.snapshot()`);
  await delay(windowMs);
  const after = await evaluate(page, `window.__plantProbe.snapshot()`);
  return {
    motion: after.animation.motion,
    framesAdded: after.animation.frames - before.animation.frames,
    rafRequestsAdded: after.animation.rafRequests - before.animation.rafRequests,
    // A render caused by a real model refresh is honest work; a render caused
    // by a running frame loop is the budget defect. Separating them is the
    // only way to assert "idle costs nothing" on a page that is also live.
    modelUpdatesAdded: (after.modelUpdates ?? 0) - (before.modelUpdates ?? 0),
    animatedCount: after.animation.animatedCount,
    windowMs,
  };
}

async function cycleContext(page, attempt) {
  const lost = await evaluate(page, `window.__plantProbe.loseContext()`);
  let lossError;
  try {
    await waitFor(
      page,
      `lost WebGL context (${attempt})`,
      `window.__plantProbe.snapshot().renderer.state === "fallback"`,
      5_000,
    );
  } catch (error) {
    lossError = error.message;
  }
  const lostSnapshot = await evaluate(page, `window.__plantProbe.snapshot()`);
  const restored = await evaluate(page, `window.__plantProbe.restoreContext()`);
  let restorationError;
  try {
    await waitFor(
      page,
      `restored WebGL context (${attempt})`,
      `window.__plantProbe.snapshot().renderer.state === "ready"`,
      5_000,
    );
  } catch (error) {
    restorationError = error.message;
  }
  const restoredSnapshot = await evaluate(page, `window.__plantProbe.snapshot()`);
  return {
    attempt,
    lost,
    lossError,
    lostSnapshot,
    restored,
    restorationError,
    restoredSnapshot,
  };
}

async function clickTheme(page, theme) {
  const current = await evaluate(page, `document.documentElement.dataset.theme`);
  if (current !== theme) {
    const label = theme === "dark" ? "Use dark theme" : "Use light theme";
    const clicked = await evaluate(
      page,
      `(() => {
        const button = document.querySelector(${JSON.stringify(`[aria-label="${label}"]`)});
        if (!(button instanceof HTMLButtonElement)) return false;
        button.click();
        return true;
      })()`,
    );
    if (!clicked) {
      throw new Error(`Unable to find the ${label} button.`);
    }
  }
  await waitFor(
    page,
    `${theme} theme`,
    `document.documentElement.dataset.theme === ${JSON.stringify(theme)} &&
     window.__plantProbe.snapshot().model?.theme === ${JSON.stringify(theme)} &&
     window.__plantProbe.snapshot().renderer.state === "ready"`,
  );
}

async function setPlantRoute(page, lens) {
  await evaluate(
    page,
    `location.hash = ${JSON.stringify(
      lens === "world"
        ? "#/factory?layout=plant"
        : `#/factory?lens=${lens}&layout=plant`,
    )}`,
  );
  await waitFor(
    page,
    `${lens} Plant lens`,
    `window.__plantProbe.snapshot().model?.lens === ${JSON.stringify(lens)} &&
     window.__plantProbe.snapshot().renderer.state === "ready"`,
  );
}

function rebuildMeasurement(snapshot) {
  if (!snapshot) {
    return undefined;
  }
  return {
    contexts: snapshot.renderer.contexts,
    rendererDisposals: snapshot.renderer.disposals,
    sceneBuilds: snapshot.scene.builds,
    sceneDisposals: snapshot.scene.disposals,
    frames: snapshot.animation.frames,
    rafRequests: snapshot.animation.rafRequests,
    entities: snapshot.entities,
    modelUpdates: snapshot.modelUpdates,
  };
}

/**
 * True when a measurement window contains no runtime or scene rebuild.
 *
 * Keyed entity reconciliation is expected and welcome; a new WebGL context, a
 * renderer disposal, or a scene build inside the window is the WP-1 defect.
 */
function noRebuild(snapshot) {
  return Boolean(
    snapshot &&
      snapshot.renderer.contexts === 0 &&
      snapshot.renderer.disposals === 0 &&
      snapshot.scene.builds === 0 &&
      snapshot.scene.disposals === 0,
  );
}

function reconciledWithoutCreating(snapshot) {
  return Boolean(
    snapshot &&
      snapshot.entities &&
      snapshot.entities.reconciles >= 1 &&
      snapshot.entities.created === 0 &&
      snapshot.entities.replaced === 0 &&
      snapshot.entities.live > 0,
  );
}

function canvasReady(snapshot) {
  return Boolean(
    snapshot.canvas &&
      snapshot.canvas.cssWidth > 0 &&
      snapshot.canvas.cssHeight > 0 &&
      snapshot.canvas.backingWidth > 0 &&
      snapshot.canvas.backingHeight > 0,
  );
}

function layoutReady(snapshot) {
  const layout = snapshot?.layout;
  const world = layout?.bounds?.world;
  const projected = layout?.bounds?.projected;
  const finite = [
    world?.minX,
    world?.minY,
    world?.minZ,
    world?.maxX,
    world?.maxY,
    world?.maxZ,
    world?.width,
    world?.height,
    world?.depth,
    projected?.minX,
    projected?.minY,
    projected?.maxX,
    projected?.maxY,
    projected?.width,
    projected?.height,
  ].every(Number.isFinite);
  return Boolean(
    layout &&
      layout.boundsFinite === true &&
      finite &&
      layout.collisions?.bayCells === 0 &&
      layout.collisions?.machines === 0 &&
      layout.collisions?.duplicateStationCoordinates === 0 &&
      layout.unresolvedTracks === 0 &&
      layout.counts?.workflows > 0 &&
      layout.counts?.stations > 0 &&
      layout.counts?.batches > 0 &&
      Number.isFinite(layout.drawCalls?.actual),
  );
}

/* --------------------------------------------------------------------------
 * WP-5 visual system gates
 *
 * The rendered frame is judged against the same authored numbers the app
 * holds: the bands and gates travel through the probe rather than being
 * copied here, so a palette change cannot silently loosen a gate.
 * ----------------------------------------------------------------------- */

/** Draw-call, triangle and GPU backing-pixel ceilings for the reference view. */
const PLANT_BUDGET = {
  maxBackingPixels: 4_000_000,
  maxDrawCalls: 120,
  maxTriangles: 180_000,
};

function bandFor(capture) {
  const visual = capture?.snapshot?.visual;
  const theme = visual?.theme ?? capture?.snapshot?.model?.theme ?? "light";
  return visual?.bands?.[theme];
}

/** The rendered frame sits inside its theme's authored luminance band. */
function luminanceBandReport(capture) {
  const band = bandFor(capture);
  const image = capture?.image;
  if (!band || !image) {
    return { ok: false, reason: "no band or image" };
  }
  const inMean = image.meanLuminance >= band.minMean && image.meanLuminance <= band.maxMean;
  const inDark = image.darkPixelRatio <= band.maxDarkPixelRatio;
  const inNearBlack = image.nearBlackRatio <= band.maxNearBlackRatio;
  return {
    band,
    darkPixelRatio: image.darkPixelRatio,
    inDark,
    inMean,
    inNearBlack,
    meanLuminance: image.meanLuminance,
    nearBlackRatio: image.nearBlackRatio,
    ok: inMean && inDark && inNearBlack,
    theme: capture.snapshot?.visual?.theme,
  };
}

/** Every authored contrast gate, measured on the palette that drew the frame. */
function contrastReport(capture) {
  const visual = capture?.snapshot?.visual;
  const contrast = visual?.contrast;
  const gates = visual?.gates;
  if (!contrast || !gates) {
    return { ok: false, reason: "no contrast report" };
  }
  const failures = [];
  const require = (label, value, gate) => {
    if (!(value >= gate)) {
      failures.push({ gate, label, value });
    }
  };
  require("machine vs floor", contrast.machineVsFloor, gates.machineVsDeck);
  require("machine vs pad", contrast.machineVsPad, gates.machineVsDeck);
  require("alternate machine vs pad", contrast.machineAltVsPad, gates.machineVsDeck);
  require("blocked marker vs body", contrast.riskBlockedVsBody, gates.riskMarkerVsBody);
  require("held marker vs body", contrast.riskHeldVsBody, gates.riskMarkerVsBody);
  require("impeded marker vs body", contrast.riskImpededVsBody, gates.riskMarkerVsBody);
  require("unread marker vs body", contrast.riskUnknownVsBody, gates.riskMarkerVsBody);
  require("selection ring vs pad", contrast.selectionRingVsPad, gates.ringVsDeck);
  require("focus ring vs pad", contrast.focusRingVsPad, gates.ringVsDeck);
  require("text vs background", contrast.textVsBackground, gates.textVsBackground);
  require("text vs floor", contrast.textVsFloor, gates.textVsBackground);
  return { contrast, failures, gates, ok: failures.length === 0, theme: contrast.theme };
}

/**
 * The Risk lens keeps the hall on screen.
 *
 * The defect this replaces dropped a black fog wall over everything that was
 * not at risk, which erased the map the hazard was supposed to be located on.
 * The test is comparative: Risk may desaturate, but it may not move the frame's
 * own brightness far from the same theme's World view, and it may never fog.
 */
function riskLegibilityReport(riskCapture, worldCapture) {
  const visual = riskCapture?.snapshot?.visual;
  const risk = riskCapture?.image;
  const world = worldCapture?.image;
  if (!visual || !risk || !world) {
    return { ok: false, reason: "no risk or world capture" };
  }
  const drift = Math.abs(risk.meanLuminance - world.meanLuminance);
  return {
    contextOpacity: visual.contextOpacity,
    drift,
    fog: visual.fog,
    markers: visual.markers,
    ok:
      visual.fog === false &&
      visual.contextOpacity >= 0.75 &&
      drift <= 0.12 &&
      visual.markers >= 1 &&
      risk.warmPixels >= 1,
    riskMean: risk.meanLuminance,
    warmPixels: risk.warmPixels,
    worldMean: world.meanLuminance,
  };
}

function budgetReport(capture) {
  const snapshot = capture?.snapshot;
  const info = snapshot?.renderer?.info;
  const canvas = snapshot?.canvas;
  const cap = snapshot?.visual?.backingPixelCap ?? PLANT_BUDGET.maxBackingPixels;
  if (!info || !canvas) {
    return { ok: false, reason: "no renderer info" };
  }
  const backingPixels = canvas.backingWidth * canvas.backingHeight;
  return {
    backingPixelCap: cap,
    backingPixels,
    drawCalls: info.calls,
    maxDrawCalls: PLANT_BUDGET.maxDrawCalls,
    maxTriangles: PLANT_BUDGET.maxTriangles,
    ok:
      info.calls <= PLANT_BUDGET.maxDrawCalls &&
      info.triangles <= PLANT_BUDGET.maxTriangles &&
      backingPixels <= cap &&
      backingPixels <= PLANT_BUDGET.maxBackingPixels,
    triangles: info.triangles,
  };
}

/** The authored palette swapped without a rebuild, in both directions. */
function paletteSwapReport(light, dark) {
  const lightVisual = light?.snapshot?.visual;
  const darkVisual = dark?.snapshot?.visual;
  if (!lightVisual || !darkVisual) {
    return { ok: false, reason: "no visual measurement" };
  }
  return {
    darkBackground: darkVisual.palette.background,
    darkKeyLight: darkVisual.palette.keyLight,
    darkTheme: darkVisual.theme,
    lightBackground: lightVisual.palette.background,
    lightKeyLight: lightVisual.palette.keyLight,
    lightTheme: lightVisual.theme,
    ok:
      lightVisual.theme === "light" &&
      darkVisual.theme === "dark" &&
      lightVisual.palette.background !== darkVisual.palette.background &&
      // The bug this replaced lit the dark hall with a near-black panel token.
      encodedLuminanceOf(darkVisual.palette.keyLight) > 0.7 &&
      encodedLuminanceOf(lightVisual.palette.keyLight) > 0.7 &&
      noRebuild(dark.snapshot),
  };
}

function encodedLuminanceOf(hex) {
  const value = String(hex).replace(/^#/, "");
  const r = Number.parseInt(value.slice(0, 2), 16);
  const g = Number.parseInt(value.slice(2, 4), 16);
  const b = Number.parseInt(value.slice(4, 6), 16);
  return (r * 0.2126 + g * 0.7152 + b * 0.0722) / 255;
}

/**
 * The Risk sentence never promises more than the read supports.
 *
 * "No confirmed current risk" is only allowed when the read was complete; an
 * incomplete read may only say nothing was confirmed in what was read.
 */
function riskHeadlineHonest(risk) {
  if (!risk) {
    return false;
  }
  const claimsCurrent = risk.headline === "No confirmed current risk";
  if (claimsCurrent && !(risk.complete && risk.allClear && risk.confirmed === 0)) {
    return false;
  }
  if (risk.allClear && !risk.complete) {
    return false;
  }
  if (!risk.complete && claimsCurrent) {
    return false;
  }
  // Unread entities are never counted as confirmed hazards.
  return (
    typeof risk.headline === "string" &&
    risk.headline.length > 0 &&
    risk.confirmed >= 0 &&
    (risk.level !== "unknown" || risk.confirmed === 0)
  );
}

function check(name, ok, details) {
  checks.push({ name, ok: Boolean(ok), details });
}

function emitTap(result, jsonPath) {
  let diagnosticChecks = 0;
  console.log("TAP version 13");
  result.checks.forEach((item, index) => {
    console.log(`${item.ok ? "ok" : "not ok"} ${index + 1} - ${item.name}`);
    if (!item.ok) {
      console.log(`  ---\n  details: ${yamlString(JSON.stringify(item.details))}\n  ...`);
    }
  });
  if (diagnostics.cdpErrors.length > 0) {
    diagnosticChecks += 1;
    console.log(`not ok ${result.checks.length + diagnosticChecks} - CDP event handlers completed`);
    console.log(`  ---\n  details: ${yamlString(JSON.stringify(diagnostics.cdpErrors))}\n  ...`);
  }
  if (diagnostics.networkErrors.length > 0) {
    diagnosticChecks += 1;
    console.log(`not ok ${result.checks.length + diagnosticChecks} - Fixture API requests succeeded`);
    console.log(`  ---\n  details: ${yamlString(JSON.stringify(diagnostics.networkErrors))}\n  ...`);
  }
  console.log(`1..${result.checks.length + diagnosticChecks}`);
  console.log(`# JSON results ${jsonPath}`);
}

async function startVite() {
  const port = await reservePort();
  const viteBin = join(portalRoot, "node_modules", "vite", "bin", "vite.js");
  if (!existsSync(viteBin)) {
    throw new Error(`Vite is not installed at ${viteBin}.`);
  }
  const child = spawn(
    process.execPath,
    [viteBin, "--host", "127.0.0.1", "--port", String(port), "--strictPort"],
    {
      cwd: portalRoot,
      env: {
        ...process.env,
        ...(options.daemonUrl ? { GOOBERS_DAEMON_URL: options.daemonUrl } : {}),
      },
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    },
  );
  const record = (chunk) => {
    diagnostics.vite.push(String(chunk).trim());
    diagnostics.vite = diagnostics.vite.slice(-20);
  };
  child.stdout.on("data", record);
  child.stderr.on("data", record);
  const baseUrl = `http://127.0.0.1:${port}`;
  try {
    await waitForHttp(baseUrl, child, 20_000);
  } catch (error) {
    await terminateChild(child);
    throw error;
  }
  return { process: child, baseUrl };
}

async function launchBrowser(browserPath, name, extraArgs) {
  const port = await reservePort();
  const profile = join(outputRoot, `profile-${name}`);
  await rm(profile, { recursive: true, force: true });
  await mkdir(profile, { recursive: true });
  const args = [
    "--headless=new",
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profile}`,
    "--no-first-run",
    "--no-default-browser-check",
    "--disable-background-networking",
    "--disable-component-update",
    "--disable-extensions",
    "--disable-sync",
    "--metrics-recording-only",
    "--mute-audio",
    "--ignore-gpu-blocklist",
    "--enable-unsafe-swiftshader",
    "--window-size=1440,1000",
    ...extraArgs,
    "about:blank",
  ];
  const child = spawn(browserPath, args, {
    cwd: portalRoot,
    stdio: ["ignore", "pipe", "pipe"],
    windowsHide: true,
  });
  child.stdout.on("data", (chunk) => diagnostics.browserConsole.push({
    type: `${name}:stdout`,
    values: [String(chunk).trim()],
  }));
  child.stderr.on("data", (chunk) => diagnostics.browserConsole.push({
    type: `${name}:stderr`,
    values: [String(chunk).trim()],
  }));
  try {
    await waitForHttp(`http://127.0.0.1:${port}/json/version`, child, 15_000);
  } catch (error) {
    await terminateChild(child);
    await rm(profile, { recursive: true, force: true });
    throw error;
  }
  if (child.exitCode !== null) {
    throw new Error(`Browser exited with code ${child.exitCode}.`);
  }
  return { process: child, port, profile, label: name, connections: new Set() };
}

async function closeBrowser(browser) {
  for (const connection of browser.connections) {
    connection.close();
  }
  await terminateChild(browser.process);
  await rm(browser.profile, {
    recursive: true,
    force: true,
    maxRetries: 10,
    retryDelay: 200,
  });
}

async function terminateChild(child) {
  if (!child || child.exitCode !== null) {
    return;
  }
  const exited = new Promise((resolveExit) => {
    child.once("exit", resolveExit);
  });
  child.kill();
  await Promise.race([exited, delay(3_000)]);
}

async function findBrowser(explicitPath) {
  if (explicitPath) {
    const path = resolve(explicitPath);
    if (!existsSync(path)) {
      throw new Error(`Browser does not exist: ${path}`);
    }
    return path;
  }
  const installed = [
    join(process.env.ProgramFiles ?? "", "Google", "Chrome", "Application", "chrome.exe"),
    join(process.env["ProgramFiles(x86)"] ?? "", "Google", "Chrome", "Application", "chrome.exe"),
    join(process.env.ProgramFiles ?? "", "Microsoft", "Edge", "Application", "msedge.exe"),
    join(process.env["ProgramFiles(x86)"] ?? "", "Microsoft", "Edge", "Application", "msedge.exe"),
  ];
  const installedBrowser = installed.find(
    (candidate) => candidate && existsSync(candidate),
  );
  if (installedBrowser) {
    return installedBrowser;
  }
  const candidates = [];
  const localAppData = process.env.LOCALAPPDATA;
  if (localAppData) {
    candidates.push(
      ...(await findFiles(join(localAppData, "ms-playwright"), new Set([
        "chrome.exe",
        "chrome-headless-shell.exe",
        "msedge.exe",
      ]))),
    );
  }
  const browser = candidates.find((candidate) => candidate && existsSync(candidate));
  if (!browser) {
    throw new Error("No cached Chromium, Chrome, or Edge executable was found.");
  }
  return browser;
}

async function findFiles(root, names) {
  if (!existsSync(root)) {
    return [];
  }
  const matches = [];
  const pending = [root];
  while (pending.length > 0) {
    const directory = pending.pop();
    for (const entry of await readdir(directory, { withFileTypes: true })) {
      const path = join(directory, entry.name);
      if (entry.isDirectory()) {
        pending.push(path);
      } else if (names.has(entry.name.toLowerCase())) {
        matches.push(path);
      }
    }
  }
  return matches.sort((left, right) => {
    const leftHeadless = left.includes("headless") ? 1 : 0;
    const rightHeadless = right.includes("headless") ? 1 : 0;
    return leftHeadless - rightHeadless || right.localeCompare(left);
  });
}

async function waitForHttp(url, child, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`Process exited with code ${child.exitCode} while waiting for ${url}.`);
    }
    try {
      const response = await fetch(url);
      if (response.ok) {
        return;
      }
    } catch {
      // The process is still starting.
    }
    await delay(100);
  }
  throw new Error(`Timed out waiting for ${url}.`);
}

async function fetchJson(url, init) {
  const response = await fetch(url, init);
  if (!response.ok) {
    throw new Error(`${init?.method ?? "GET"} ${url} returned ${response.status}.`);
  }
  return response.json();
}

async function reservePort() {
  return new Promise((resolvePort, reject) => {
    const server = createServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      if (!address || typeof address === "string") {
        server.close();
        reject(new Error("Unable to reserve a local port."));
        return;
      }
      server.close((error) => {
        if (error) {
          reject(error);
        } else {
          resolvePort(address.port);
        }
      });
    });
  });
}

async function waitFor(page, description, expression, timeoutMs = 8_000) {
  const deadline = Date.now() + timeoutMs;
  let lastError;
  while (Date.now() < deadline) {
    try {
      if (await evaluate(page, `Boolean(${expression})`)) {
        return;
      }
    } catch (error) {
      lastError = error;
    }
    await delay(100);
  }
  throw new Error(
    `Timed out waiting for ${description}.${lastError ? ` ${lastError.message}` : ""}`,
  );
}

async function evaluate(page, expression) {
  const response = await page.cdp.send("Runtime.evaluate", {
    expression,
    awaitPromise: true,
    returnByValue: true,
    userGesture: true,
  });
  if (response.exceptionDetails) {
    throw new Error(
      response.exceptionDetails.exception?.description ??
        response.exceptionDetails.text ??
        "Browser evaluation failed.",
    );
  }
  return response.result.value;
}

function delay(ms) {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, ms));
}

function parseArgs(args) {
  const parsed = {
    baseUrl: undefined,
    browser: undefined,
    daemonUrl: undefined,
    fixtures: true,
    output: ".plant-harness",
  };
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (argument === "--live-daemon") {
      parsed.fixtures = false;
      continue;
    }
    const [name, inlineValue] = argument.split("=", 2);
    const value = inlineValue ?? args[index + 1];
    if (name === "--base-url") {
      parsed.baseUrl = value;
    } else if (name === "--browser") {
      parsed.browser = value;
    } else if (name === "--daemon-url") {
      parsed.daemonUrl = value;
    } else if (name === "--output") {
      parsed.output = value;
    } else {
      throw new Error(`Unknown Plant harness option: ${argument}`);
    }
    if (inlineValue === undefined) {
      index += 1;
    }
    if (!value) {
      throw new Error(`${name} requires a value.`);
    }
  }
  return parsed;
}

function yamlString(value) {
  return JSON.stringify(value);
}

class CdpConnection {
  static async connect(url) {
    const socket = new WebSocket(url);
    await new Promise((resolveOpen, reject) => {
      socket.addEventListener("open", resolveOpen, { once: true });
      socket.addEventListener("error", () => reject(new Error(`Unable to connect to ${url}.`)), {
        once: true,
      });
    });
    return new CdpConnection(socket);
  }

  constructor(socket) {
    this.socket = socket;
    this.nextId = 1;
    this.pending = new Map();
    this.listeners = new Map();
    this.errorListeners = new Set();
    this.closing = false;
    socket.addEventListener("message", (event) => this.handleMessage(event.data));
    socket.addEventListener("close", () => {
      for (const pending of this.pending.values()) {
        pending.reject(new Error("CDP connection closed."));
      }
      this.pending.clear();
    });
  }

  send(method, params = {}) {
    const id = this.nextId;
    this.nextId += 1;
    return new Promise((resolveCommand, reject) => {
      this.pending.set(id, { resolve: resolveCommand, reject });
      this.socket.send(JSON.stringify({ id, method, params }));
    });
  }

  on(method, listener) {
    const listeners = this.listeners.get(method) ?? new Set();
    listeners.add(listener);
    this.listeners.set(method, listeners);
  }

  onError(listener) {
    this.errorListeners.add(listener);
  }

  close() {
    this.closing = true;
    this.socket.close();
  }

  handleMessage(raw) {
    const message = JSON.parse(String(raw));
    if (message.id !== undefined) {
      const pending = this.pending.get(message.id);
      if (!pending) {
        return;
      }
      this.pending.delete(message.id);
      if (message.error) {
        pending.reject(new Error(`${message.error.message} (${message.error.code})`));
      } else {
        pending.resolve(message.result);
      }
      return;
    }
    for (const listener of this.listeners.get(message.method) ?? []) {
      Promise.resolve(listener(message.params)).catch((error) => {
        const normalized = error instanceof Error ? error : new Error(String(error));
        if (this.closing && normalized.message === "CDP connection closed.") {
          return;
        }
        for (const errorListener of this.errorListeners) {
          errorListener(normalized);
        }
      });
    }
  }

}

await main();
