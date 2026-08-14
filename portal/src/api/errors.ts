import { API_VERSION, SCHEMA_VERSION } from "./types";

export type DaemonClientErrorKind =
  | "api"
  | "auth"
  | "cancelled"
  | "timeout"
  | "malformed-response"
  | "unavailable"
  | "unsupported-api-version"
  | "unsupported-schema-version";

export class DaemonClientError extends Error {
  constructor(
    message: string,
    readonly kind: DaemonClientErrorKind,
    options?: ErrorOptions,
  ) {
    super(message, options);
    this.name = new.target.name;
  }
}

export class DaemonApiError extends DaemonClientError {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message, "api");
  }
}

/**
 * Thrown when the daemon (or an intermediary in front of it, such as a
 * reverse proxy) rejects a request with HTTP 401 or 403 (#2916).
 *
 * This is deliberately its own error kind, distinct from
 * DaemonUnavailableError: the daemon (or its front door) is reachable and
 * answering, it is just refusing this request's credentials. The status is
 * classified before the response body is ever inspected, so a non-JSON body
 * (an HTML login page, plain text from a proxy, an empty body) is still
 * correctly identified as an auth failure rather than falling through to a
 * generic "malformed response" or "unavailable" error.
 */
export class DaemonAuthError extends DaemonClientError {
  constructor(readonly status: 401 | 403) {
    super(
      status === 401
        ? "Authentication is required to reach the daemon (HTTP 401)."
        : "This credential is not authorized to reach the daemon (HTTP 403).",
      "auth",
    );
  }
}

export class RequestCancelledError extends DaemonClientError {
  constructor(options?: ErrorOptions) {
    super("The daemon request was cancelled.", "cancelled", options);
  }
}

export class RequestTimeoutError extends DaemonClientError {
  constructor(
    readonly timeoutMs: number,
    options?: ErrorOptions,
  ) {
    super(`The daemon request timed out after ${timeoutMs}ms.`, "timeout", options);
  }
}

export class MalformedResponseError extends DaemonClientError {
  constructor(message = "The daemon returned a malformed response.", options?: ErrorOptions) {
    super(message, "malformed-response", options);
  }
}

export class DaemonUnavailableError extends DaemonClientError {
  constructor(options?: ErrorOptions) {
    super("The daemon is unavailable.", "unavailable", options);
  }
}

export class UnsupportedApiVersionError extends DaemonClientError {
  constructor(readonly received: string) {
    super(
      `The daemon API version ${JSON.stringify(received)} is unsupported; expected ${JSON.stringify(API_VERSION)}.`,
      "unsupported-api-version",
    );
  }
}

export class UnsupportedSchemaVersionError extends DaemonClientError {
  constructor(readonly received: string) {
    super(
      `The daemon schema version ${JSON.stringify(received)} is unsupported; expected ${JSON.stringify(SCHEMA_VERSION)}.`,
      "unsupported-schema-version",
    );
  }
}

export function assertSupportedContractVersion(value: unknown): void {
  if (!isRecord(value)) {
    throw new MalformedResponseError();
  }
  if (typeof value.apiVersion !== "string" || typeof value.schemaVersion !== "string") {
    throw new MalformedResponseError("The daemon response is missing contract version fields.");
  }
  if (value.apiVersion !== API_VERSION) {
    throw new UnsupportedApiVersionError(value.apiVersion);
  }
  if (value.schemaVersion !== SCHEMA_VERSION) {
    throw new UnsupportedSchemaVersionError(value.schemaVersion);
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
