const QUERY_PARAMETER = "portal-diagnostics";
const LOG_PREFIX = "[goobers portal diagnostics]";

export type PortalRequestStatus = number | "cancelled" | "timeout" | "error";

export interface PortalRequestTrace {
  finish(status: PortalRequestStatus): void;
}

export interface PortalSSEEvent {
  event: "connect" | "disconnect" | "reconnect";
  cause: string;
  delayMs?: number;
}

export interface PortalDiagnostics {
  startRequest(request: { endpoint: string; method: string }): PortalRequestTrace;
  recordSSE(event: PortalSSEEvent): void;
}

interface PortalDiagnosticsOptions {
  search?: string;
  debug?: (...data: unknown[]) => void;
  now?: () => number;
  timestamp?: () => string;
  page?: () => string;
}

export function createPortalDiagnostics(
  options: PortalDiagnosticsOptions = {},
): PortalDiagnostics | undefined {
  const search = options.search ?? currentSearch();
  if (new URLSearchParams(search).get(QUERY_PARAMETER) !== "1") {
    return undefined;
  }
  return new ConsolePortalDiagnostics(
    options.debug ?? console.debug.bind(console),
    options.now ?? (() => performance.now()),
    options.timestamp ?? (() => new Date().toISOString()),
    options.page ?? currentPage,
  );
}

class ConsolePortalDiagnostics implements PortalDiagnostics {
  private activeRequests = 0;
  private burst:
    | {
        count: number;
        initiatedBy: string;
        startedAt: number;
      }
    | undefined;

  constructor(
    private readonly debug: (...data: unknown[]) => void,
    private readonly now: () => number,
    private readonly timestamp: () => string,
    private readonly page: () => string,
  ) {}

  startRequest(request: { endpoint: string; method: string }): PortalRequestTrace {
    const startedAt = this.now();
    const initiatedBy = this.page();
    if (this.activeRequests === 0) {
      this.burst = { count: 0, initiatedBy, startedAt };
    }
    this.activeRequests += 1;
    if (this.burst) {
      this.burst.count += 1;
    }

    let finished = false;
    return {
      finish: (status) => {
        if (finished) {
          return;
        }
        finished = true;
        const finishedAt = this.now();
        this.debug(LOG_PREFIX, {
          type: "request",
          timestamp: this.timestamp(),
          endpoint: request.endpoint,
          method: request.method,
          status,
          durationMs: milliseconds(finishedAt - startedAt),
          initiatedBy,
        });

        this.activeRequests -= 1;
        if (this.activeRequests === 0 && this.burst) {
          this.debug(LOG_PREFIX, {
            type: "request-burst",
            timestamp: this.timestamp(),
            count: this.burst.count,
            totalElapsedMs: milliseconds(finishedAt - this.burst.startedAt),
            initiatedBy: this.burst.initiatedBy,
          });
          this.burst = undefined;
        }
      },
    };
  }

  recordSSE(event: PortalSSEEvent): void {
    this.debug(LOG_PREFIX, {
      type: "sse",
      timestamp: this.timestamp(),
      ...event,
    });
  }
}

function currentSearch(): string {
  return typeof location === "undefined" ? "" : location.search;
}

function currentPage(): string {
  if (typeof location === "undefined") {
    return "unknown";
  }
  return location.hash || location.pathname;
}

function milliseconds(value: number): number {
  return Math.round(value * 100) / 100;
}
