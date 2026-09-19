import { useRef, useState } from "react";
import { createPortal } from "react-dom";
import type { BuildMetadata, DaemonClient, Instance } from "../api/types";
import { useCobrand } from "../cobrand";
import { UpdateNotice } from "../components/UpdateNotice";
import { dataCacheKey } from "../dataCache";
import {
  useLiveData,
  type DataFreshness,
  type LiveDataSSEFailure,
  type LiveFreshness,
  type LiveUpdateDetails,
} from "../liveData";
import { useLiveQuery } from "../liveQuery";
import { useGaggleList } from "../operationalData";
import { routeHash, type Navigate, type PrimaryArea } from "../routing";
import { hasScopeIdentity, type ScopeFilters } from "../scope";
import type { Theme } from "../theme";
import { useUpdateNotice } from "../updateNotice";
import { Icon } from "../ui/Icon";
import { SupportFooter } from "./SupportFooter";

interface HeaderIdentity {
  build?: BuildMetadata;
  instance: Pick<
    Instance,
    "computerName" | "environment" | "instanceRoot" | "name" | "rootIdentity"
  >;
}

export interface PortalHeaderHost {
  /** Stable, same-document container owned by the embedding application. */
  target: HTMLElement;
  /** Additional host controls; Portal branding, freshness and theme controls remain intact. */
  actions?: React.ReactNode;
}

interface PortalShellProps {
  activeArea: PrimaryArea;
  activeGaggle?: string;
  children: React.ReactNode;
  client: DaemonClient;
  currentScope: Pick<
    ScopeFilters,
    "gaggle" | "workflow" | "stage" | "since" | "until" | "window"
  >;
  hostContext: "daemon" | "fleet" | "standalone";
  headerHost?: PortalHeaderHost;
  navigate: Navigate;
  standalone: boolean;
  theme: Theme;
  toggleTheme: () => void;
}

export function PortalShell({
  activeArea,
  activeGaggle,
  children,
  client,
  currentScope,
  hostContext,
  headerHost,
  navigate,
  standalone,
  theme,
  toggleTheme,
}: PortalShellProps) {
  // Runs, Insight, and Cost are peer views over the same identity and time
  // scope. Keep those fields together while dropping page-specific refinements.
  const scopedFilters =
    hasScopeIdentity(currentScope) ||
    currentScope.since ||
    currentScope.until ||
    currentScope.window
      ? currentScope
      : undefined;
  const { config } = useCobrand();
  const {
    admissionState,
    dataFreshness,
    freshness,
    lastSSEFailure,
    liveUpdateDetails,
  } = useLiveData();
  const updateNotice = useUpdateNotice();
  const mainContent = useRef<HTMLElement>(null);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);
  const headerIdentity = useLiveQuery<HeaderIdentity>({
    cacheKey: dataCacheKey("portal-header-identity"),
    dependencies: [{ model: "instance" }],
    models: ["instance"],
    load: async (signal) => {
      const [instance, health] = await Promise.all([
        client.getInstance({ signal }),
        client.getHealth({ signal }),
      ]);
      const { computerName, environment, instanceRoot, name, rootIdentity } = instance;
      return {
        build: health.build,
        instance: { computerName, environment, instanceRoot, name, rootIdentity },
      };
    },
    errorMessage: "Unable to load compact header instance identity.",
  });
  const headerData =
    headerIdentity.state.status === "ready" || headerIdentity.state.status === "stale"
      ? headerIdentity.state.data
      : undefined;
  const instanceIdentity = headerData?.instance;
  const build = headerData?.build;
  const connectionStatus = describeConnectionStatus(freshness, lastSSEFailure);

  const skipToMainContent = (event: React.MouseEvent<HTMLAnchorElement>) => {
    event.preventDefault();
    mainContent.current?.focus();
  };

  const header = (
      <header className={headerHost ? "topbar topbar-hosted" : "topbar"}>
        <div className="topbar-primary">
          <button
            aria-label="Go to overview"
            className="topbar-brand"
            onClick={() => navigate({ page: "overview" })}
            title={config.brand.tagline}
            type="button"
          >
            <img alt="" src={config.brand.logoUrl ?? "/goober-mascot.png"} />
            {headerHost ? (
              <span className="topbar-hosted-brand-copy">
                <strong>{config.brand.name}</strong>
                <small>{config.brand.tagline}</small>
              </span>
            ) : <strong>{config.brand.name}</strong>}
          </button>
          <span aria-hidden="true" className="topbar-divider" />
          <div className="topbar-instance-context" aria-label="Instance context">
            <span className="topbar-instance-name">
              {instanceIdentity?.name ?? "Loading instance"}
            </span>
            {instanceIdentity?.computerName && (
              <>
                <span aria-hidden="true" className="topbar-context-separator">•</span>
                <span className="topbar-computer-name">{instanceIdentity.computerName}</span>
              </>
            )}
            {instanceIdentity?.environment && (
              <>
                <span aria-hidden="true" className="topbar-context-separator">•</span>
                <span className="topbar-environment">
                  {instanceIdentity.environment}
                  {build?.commit && build.commit !== "none" ? ` (${build.commit})` : ""}
                </span>
              </>
            )}
            <span className="topbar-info-wrap">
              <button
                aria-describedby="portal-context-tooltip"
                aria-label="Show portal details"
                className="topbar-info"
                type="button"
              >
                <Icon name="info" size={17} />
              </button>
              <span className="topbar-info-tooltip" id="portal-context-tooltip" role="tooltip">
                <strong>{config.brand.tagline}</strong>
                <span className="topbar-tooltip-grid">
                  <span>Host</span>
                  <span>{hostContextLabel(hostContext)}</span>
                  <span>Instance</span>
                  <span>{instanceIdentity?.name ?? "Loading"}</span>
                  <span>Version</span>
                  <span>{build ? `${build.version} · ${build.commit || "none"}` : "Unavailable"}</span>
                  <span>Computer</span>
                  <span>{instanceIdentity?.computerName ?? "Unavailable"}</span>
                  <span>Environment</span>
                  <span>{instanceIdentity?.environment ?? "Unavailable"}</span>
                  <span>Instance root</span>
                  <span>{instanceIdentity?.instanceRoot ?? "Unavailable"}</span>
                  <span>Instance ID</span>
                  <span>{instanceIdentity?.rootIdentity?.id ?? "Unavailable"}</span>
                </span>
              </span>
            </span>
          </div>
        </div>
        <div className="topbar-actions">
          <LiveUpdatesIndicator
            connectionStatus={connectionStatus}
            details={liveUpdateDetails}
            failure={lastSSEFailure}
            freshness={freshness}
            state={dataFreshness}
          />
          <button
            aria-label={`Use ${theme === "light" ? "dark" : "light"} theme`}
            className="theme-button"
            onClick={toggleTheme}
            type="button"
          >
            <Icon name={theme === "light" ? "moon" : "sun"} size={17} />
          </button>
          {headerHost?.actions && (
            <div className="topbar-host-actions">{headerHost.actions}</div>
          )}
        </div>
      </header>
  );

  return (
    <div
      className={headerHost ? "portal-frame portal-frame-hosted-header" : "portal-frame"}
      data-host={hostContext}
    >
      <a className="skip-link" href="#main-content" onClick={skipToMainContent}>
        Skip to main content
      </a>
      {headerHost ? createPortal(header, headerHost.target) : header}
      <aside className="sidebar">
        <button
          aria-controls="portal-secondary-navigation"
          aria-expanded={mobileMenuOpen}
          aria-label="Show gaggles, status, and support links"
          className="mobile-navigation-button"
          onClick={() => setMobileMenuOpen((open) => !open)}
          type="button"
        >
          <Icon name="menu" />
          <span>{mobileMenuOpen ? "Close" : "More"}</span>
        </button>

        <nav className="primary-nav" aria-label="Primary">
          <button
            aria-current={activeArea === "overview" ? "page" : undefined}
            aria-label="Overview"
            className={activeArea === "overview" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "overview" })}
            type="button"
          >
            <Icon name="overview" />
            <span className="nav-label">Overview</span>
          </button>
          <button
            aria-current={activeArea === "workflows" ? "page" : undefined}
            aria-label="Workflows"
            className={activeArea === "workflows" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "workflows" })}
            type="button"
          >
            <Icon name="workflow" />
            <span className="nav-label">Workflows</span>
          </button>
          <button
            aria-current={activeArea === "goobers" ? "page" : undefined}
            aria-label="Goobers"
            className={activeArea === "goobers" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "goobers" })}
            type="button"
          >
            <Icon name="goober" />
            <span className="nav-label">Goobers</span>
          </button>
          <button
            aria-current={activeArea === "runs" ? "page" : undefined}
            aria-label="Runs"
            className={activeArea === "runs" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "runs", filters: scopedFilters })}
            type="button"
          >
            <Icon name="run" />
            <span className="nav-label">Runs</span>
          </button>
          <button
            aria-current={activeArea === "work-items" ? "page" : undefined}
            aria-label="Work Items"
            className={activeArea === "work-items" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "work-items" })}
            type="button"
          >
            <Icon name="work-item" />
            <span className="nav-label">Work Items</span>
          </button>
          <button
            aria-current={activeArea === "insight" ? "page" : undefined}
            aria-label="Insight"
            className={activeArea === "insight" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "insight", filters: scopedFilters })}
            type="button"
          >
            <Icon name="insight" />
            <span className="nav-label">Insight</span>
          </button>
          <button
            aria-current={activeArea === "cost" ? "page" : undefined}
            aria-label="Cost"
            className={activeArea === "cost" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "cost", filters: scopedFilters })}
            type="button"
          >
            <Icon name="cost" />
            <span className="nav-label">Cost</span>
          </button>
        </nav>

        <div
          className={`sidebar-secondary${mobileMenuOpen ? " sidebar-secondary-open" : ""}`}
          id="portal-secondary-navigation"
        >
          <GaggleNav activeGaggle={activeGaggle} client={client} navigate={navigate} />
          <SupportFooter />
        </div>
      </aside>

      <div className="portal-main">
        <main className="page-content" id="main-content" ref={mainContent} tabIndex={-1}>
          {admissionState && (
            <div className="admission-degraded" role="alert">
              <strong>Daemon is busy.</strong>{" "}
              Live refresh is backing off automatically. New navigation requests are not held
              behind unlimited retries.
            </div>
          )}
          {updateNotice.update && (
            <UpdateNotice onDismiss={updateNotice.dismiss} update={updateNotice.update} />
          )}
          {children}
        </main>
      </div>
    </div>
  );
}

/**
 * Persistent gaggle affordance (#2531): a gaggle must be reachable directly
 * from anywhere in the app, not only by drilling down through Workflows. It
 * doubles as the switcher on multi-gaggle instances since the sidebar
 * carries it on every page, including the gaggle view itself.
 */
function GaggleNav({
  activeGaggle,
  client,
  navigate,
}: {
  activeGaggle?: string;
  client: DaemonClient;
  navigate: Navigate;
}) {
  const { state } = useGaggleList(client);
  const gaggles =
    state.status === "ready" || state.status === "stale" ? state.data : undefined;

  if (!gaggles || gaggles.length === 0) {
    return null;
  }

  return (
    <nav aria-label="Gaggles" className="sidebar-gaggle-nav">
      <span className="sidebar-section-label">Gaggles</span>
      <ul>
        {gaggles.map((gaggle) => (
          <li key={gaggle.name}>
            <a
              aria-current={activeGaggle === gaggle.name ? "page" : undefined}
              aria-label={`Open gaggle ${gaggle.displayName}`}
              className={`${activeGaggle === gaggle.name ? "nav-item nav-item-active" : "nav-item"}${gaggle.enabled ? "" : " definition-disabled"}`}
              href={routeHash({ page: "gaggle", id: gaggle.name })}
              onClick={(event) => {
                event.preventDefault();
                navigate({ page: "gaggle", id: gaggle.name });
              }}
            >
              <Icon name="gaggle" />
              <span className="nav-label">{gaggle.displayName}</span>
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

const freshnessLabel: Record<LiveFreshness, string> = {
  connected: "Live updates connected",
  reconnecting: "Reconnecting",
  stale: "Data stale",
  offline: "Offline",
  "polling-fallback": "Polling fallback",
};

function hostContextLabel(hostContext: PortalShellProps["hostContext"]): string {
  switch (hostContext) {
    case "fleet":
      return "Goobers Fleet";
    case "standalone":
      return "Standalone dashboard";
    default:
      return "Daemon dashboard";
  }
}

function describeConnectionStatus(
  freshness: LiveFreshness,
  failure: LiveDataSSEFailure | undefined,
): string {
  if (freshness !== "polling-fallback" || !failure) {
    return freshnessLabel[freshness];
  }
  const causeChunk = failure.result ? `${failure.cause} (${failure.result})` : failure.cause;
  return `${freshnessLabel[freshness]} — ${causeChunk}`;
}

/**
 * Renders how current the data is.
 *
 * Accessibility: the state is never conveyed by colour alone (§11A). Each state
 * carries distinct TEXT, and the mark is decorative (`aria-hidden`) so a screen
 * reader announces the words rather than a dot. `role="status"` with
 * `aria-live="polite"` announces transitions without interrupting.
 */
export function DataFreshnessIndicator({ state }: { state: DataFreshness }) {
  if (state.kind === "unknown") {
    // No envelope: a standalone read, or a daemon with no read model attached.
    // Rendering "current" here would be a claim nobody made.
    return null;
  }
  return (
    <span
      aria-live="polite"
      className={`data-freshness data-freshness-${state.kind}`}
      data-state={state.kind}
      role="status"
    >
      <span aria-hidden="true" className={`data-mark data-mark-${state.kind}`} />
      {dataFreshnessLabel(state)}
    </span>
  );
}

function PollingFallbackIndicator({
  state,
}: {
  state: DataFreshness;
}) {
  const dataLabel = state.kind === "unknown" ? "Data current" : dataFreshnessLabel(state);
  return (
    <span
      aria-live="polite"
      className="freshness-status freshness-status-polling-fallback"
      data-state="polling-fallback"
      role="status"
    >
      <span aria-hidden="true" className="live-mark live-mark-polling-fallback" />
      {dataLabel} via polling
    </span>
  );
}

function LiveUpdatesIndicator({
  connectionStatus,
  details,
  failure,
  freshness,
  state,
}: {
  connectionStatus: string;
  details: LiveUpdateDetails;
  failure: LiveDataSSEFailure | undefined;
  freshness: LiveFreshness;
  state: DataFreshness;
}) {
  const tooltipId = "live-updates-tooltip";
  const dataLabel = state.kind === "unknown" ? "Unknown" : dataFreshnessTitle(state);
  return (
    <span className="live-status-wrap">
      <button
        aria-describedby={tooltipId}
        aria-label={`${connectionStatus}. Show live update details`}
        className="live-status-trigger"
        type="button"
      >
        {freshness === "polling-fallback" ? (
          <PollingFallbackIndicator state={state} />
        ) : (
          <>
            <DataFreshnessIndicator state={state} />
            <span
              aria-live="polite"
              className={`freshness-status freshness-status-${freshness}`}
              data-state={freshness}
              role="status"
            >
              <span aria-hidden="true" className={`live-mark live-mark-${freshness}`} />
              {connectionStatus}
            </span>
          </>
        )}
      </button>
      <span className="live-status-tooltip" id={tooltipId} role="tooltip">
        <strong>Live update diagnostics</strong>
        <span className="topbar-tooltip-grid">
          <span>Transport</span>
          <span>{describeTransport(details)}</span>
          <span>Status</span>
          <span>{describeDetailedStatus(freshness, failure, details)}</span>
          <span>Data</span>
          <span>{dataLabel}</span>
          <span>Last SSE message</span>
          <span>{formatTimestamp(details.lastMessageAt)}</span>
          <span>Last data event</span>
          <span>{formatTimestamp(details.lastDataEventAt)}</span>
          <span>Connected since</span>
          <span>{formatTimestamp(details.connectedAt)}</span>
          <span>Failures</span>
          <span>{details.consecutiveFailures} consecutive</span>
          <span>Last failure</span>
          <span>{describeFailure(details.lastFailure, details.lastFailureAt)}</span>
          <span>Next SSE retry</span>
          <span>{describeNextReconnect(details)}</span>
          <span>Last poll</span>
          <span>{describeLastPoll(details)}</span>
          <span>Next poll</span>
          <span>{formatTimestamp(details.nextPollAt)}</span>
        </span>
      </span>
    </span>
  );
}

function describeTransport(details: LiveUpdateDetails): string {
  if (details.transport === "polling") {
    return "Polling fallback; SSE retries continue";
  }
  if (details.transport === "none") {
    return "Paused";
  }
  return details.shared ? "SSE shared from another tab" : "SSE";
}

function describeDetailedStatus(
  freshness: LiveFreshness,
  failure: LiveDataSSEFailure | undefined,
  details: LiveUpdateDetails,
): string {
  if (details.connectStartedAt !== undefined) {
    return `Connecting; attempt ${details.consecutiveFailures + 1}`;
  }
  if (freshness === "reconnecting" && details.nextReconnectAt !== undefined) {
    return `Waiting to reconnect; attempt ${details.consecutiveFailures + 1}`;
  }
  if (failure) {
    const result = failure.result ? ` (${failure.result})` : "";
    return `${freshnessLabel[freshness]} — ${failure.cause}${result}`;
  }
  return freshnessLabel[freshness];
}

function describeFailure(
  failure: LiveDataSSEFailure | undefined,
  failedAt: number | undefined,
): string {
  if (!failure) {
    return "None";
  }
  const result = failure.result ? ` (${failure.result})` : "";
  return `${failure.cause}${result} on ${failure.endpoint}; ${formatTimestamp(failedAt)}`;
}

function describeNextReconnect(details: LiveUpdateDetails): string {
  if (details.connectStartedAt !== undefined) {
    return details.connectDeadlineAt === undefined
      ? "Connecting now"
      : `Connecting now; timeout ${formatTimestamp(details.connectDeadlineAt)}`;
  }
  return formatTimestamp(details.nextReconnectAt);
}

function describeLastPoll(details: LiveUpdateDetails): string {
  if (details.lastPollAt === undefined) {
    return "Never";
  }
  const result = details.lastPollSucceeded ? "succeeded" : "failed";
  return `${formatTimestamp(details.lastPollAt)}; ${result}`;
}

function formatTimestamp(timestamp: number | undefined): string {
  if (timestamp === undefined) {
    return "Never";
  }
  const difference = timestamp - Date.now();
  const absolute = new Date(timestamp).toLocaleTimeString([], {
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
  });
  const elapsed = Math.abs(difference);
  const amount =
    elapsed < 1_000
      ? "now"
      : elapsed < 60_000
        ? `${Math.round(elapsed / 1_000)}s`
        : elapsed < 3_600_000
          ? `${Math.round(elapsed / 60_000)}m`
          : `${Math.round(elapsed / 3_600_000)}h`;
  if (amount === "now") {
    return `${absolute} (now)`;
  }
  return `${absolute} (${difference > 0 ? `in ${amount}` : `${amount} ago`})`;
}

function dataFreshnessLabel(state: DataFreshness): string {
  switch (state.kind) {
    case "current":
      return "Data current";
    case "lagging":
      return state.lagSeconds > 0
        ? `Data stale by ${formatLag(state.lagSeconds)}`
        : "Data degraded";
    case "partial":
      return `Partial — ${state.missing.map((entry) => entry.name).join(", ")}`;
    default:
      return "";
  }
}

function dataFreshnessTitle(state: DataFreshness): string {
  switch (state.kind) {
    case "current":
      return `Read model is current (within ${formatLag(state.lagSeconds)})`;
    case "lagging":
      return state.degraded.length > 0
        ? `Behind by up to ${formatLag(state.lagSeconds)}: ${state.degraded.join(", ")}`
        : `Behind by up to ${formatLag(state.lagSeconds)}`;
    case "partial":
      // The expiry is the difference between a useful partial and wallpaper: it
      // tells the user whether to wait or to investigate.
      return state.missing
        .map((entry) => `${entry.name}: ${entry.reason} (expected by ${entry.expectedBy})`)
        .join("; ");
    default:
      return "";
  }
}

/** Whole seconds under a minute, whole minutes above — a lag readout does not
 *  need sub-second precision and reads worse with it. */
function formatLag(seconds: number): string {
  if (seconds < 60) {
    return `${Math.max(0, Math.round(seconds))}s`;
  }
  const minutes = Math.round(seconds / 60);
  return minutes < 60 ? `${minutes}m` : `${Math.round(minutes / 60)}h`;
}
