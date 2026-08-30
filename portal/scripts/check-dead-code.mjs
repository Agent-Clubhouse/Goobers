import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { findingsFromKnipReport, reviewFindings } from "./dead-code-ledger.mjs";

const exemptions = [
  {
    type: "files",
    file: "scripts/check-dead-code.mjs",
    symbol: "scripts/check-dead-code.mjs",
    reason: "CI entry point invoked from package.json rather than the production import graph.",
  },
  {
    type: "files",
    file: "scripts/dead-code-ledger.mjs",
    symbol: "scripts/dead-code-ledger.mjs",
    reason: "Ledger validator imported only by the CI entry point and its unit test.",
  },
  {
    type: "files",
    file: "src/api/fixtureClient.ts",
    symbol: "src/api/fixtureClient.ts",
    reason: "Test-only daemon client used by portal integration fixtures.",
  },
  {
    type: "files",
    file: "src/test/daemonFixtures.ts",
    symbol: "src/test/daemonFixtures.ts",
    reason: "Shared test fixture catalog; production must not import fixture data.",
  },
  {
    type: "files",
    file: "src/shell/DataFreshnessProbe.tsx",
    symbol: "src/shell/DataFreshnessProbe.tsx",
    reason: "Test-only probe that exposes portal freshness state without changing production UI.",
  },
  {
    type: "files",
    file: "src/api/wire.generated.ts",
    symbol: "src/api/wire.generated.ts",
    reason: "Generated Go/TypeScript contract fixture consumed by the contract and component tests.",
  },
  {
    type: "files",
    file: "src/test/setup.ts",
    symbol: "src/test/setup.ts",
    reason: "Vitest bootstrap loaded by vite.config.ts rather than the production import graph.",
  },
  {
    type: "files",
    file: "e2e/fixture-daemon.mjs",
    symbol: "e2e/fixture-daemon.mjs",
    reason: "Playwright web-server entry point invoked from playwright.config.ts.",
  },
];

const testOnlyExports = {
  "src/api/errors.ts": ["UnsupportedApiVersionError", "UnsupportedSchemaVersionError"],
  "src/api/queryFamily.ts": ["emptyStats", "positionOf", "comparePosition"],
  "src/api/surfaceActions.ts": ["uiSurfaceActions"],
  "src/attentionCollapse.ts": [
    "attentionCollapsedStorageKey",
    "readStoredAttentionCollapsed",
  ],
  "src/attentionDismissals.ts": [
    "attentionDismissalsStorageKey",
    "readStoredAttentionDismissals",
  ],
  "src/components/GaggleWorkflowExplorer.tsx": ["WorkflowPicker"],
  "src/components/RunStageInspector.tsx": ["TranscriptView"],
  "src/dataCache.ts": ["DATA_CACHE_TTL_MS"],
  "src/insightData.ts": [
    "insightTrendBuckets",
    "insightPreviousWindowFilters",
    "insightWindowFilters",
    "insightErrorSignatureFilters",
    "selectInsightCostTrendBuckets",
  ],
  "src/liveData.tsx": [
    "LiveDataController",
    "setReadStateSink",
    "deriveDataFreshness",
  ],
  "src/operationalData.ts": [
    "INVENTORY_CACHE_TTL_MS",
    "loadOperationalSnapshot",
    "loadOperationalOverview",
  ],
  "src/prototypeFixtures.ts": ["workflowWarnings"],
  "src/replay.ts": [
    "idleCompressionThresholdMs",
    "compressedIdleDelayMs",
    "orderedReplayEvents",
    "replayChapterKind",
  ],
  "src/runDetailData.ts": ["loadRunDetail", "isVerdictArtifact"],
  "src/runsHistory.ts": ["RUNS_PAGE_SIZE"],
  "src/shell/PortalShell.tsx": ["DataFreshnessIndicator"],
  "src/theme.ts": ["themeStorageKey", "readStoredTheme", "persistTheme"],
  "src/ui/Inspector.tsx": ["InspectorHeading"],
  "src/workflowDetailData.ts": ["loadWorkflowDetail"],
};

for (const [file, symbols] of Object.entries(testOnlyExports)) {
  for (const symbol of symbols) {
    exemptions.push({
      type: "exports",
      file,
      symbol,
      reason: "Exported as a focused unit-test seam; production uses it within this module.",
    });
  }
}

const portalRoot = fileURLToPath(new URL("../", import.meta.url));
const knip = fileURLToPath(new URL("../node_modules/knip/bin/knip.js", import.meta.url));
const result = spawnSync(
  process.execPath,
  [
    knip,
    "--production",
    "--include",
    "files,exports,nsExports",
    "--reporter",
    "json",
    "--no-progress",
  ],
  { cwd: portalRoot, encoding: "utf8" },
);

if (result.error) throw result.error;
if (result.status !== 0 && result.status !== 1) {
  process.stderr.write(result.stderr);
  process.exit(result.status ?? 1);
}

let report;
try {
  report = JSON.parse(result.stdout);
} catch (error) {
  process.stderr.write(result.stderr);
  throw new Error(`could not parse Knip output: ${error.message}`);
}

const findings = findingsFromKnipReport(report);
const { unexpected, stale } = reviewFindings(findings, exemptions);
const keyOf = ({ type, file, symbol }) => `${type}:${file}:${symbol}`;

if (unexpected.length > 0) {
  console.error("Unreviewed portal production dead code:");
  for (const finding of unexpected) console.error(`  ${keyOf(finding)}`);
}
if (stale.length > 0) {
  console.error("Stale portal dead-code exemptions:");
  for (const exemption of stale) console.error(`  ${keyOf(exemption)} (${exemption.reason})`);
}
if (unexpected.length > 0 || stale.length > 0) process.exit(1);
