import { DaemonAuthError } from "../api/errors";

export function DaemonLoadingState({ standalone = false }: { standalone?: boolean }) {
  return (
    <section aria-live="polite" className="daemon-state" role="status">
      <span aria-hidden="true" className="loading-mark" />
      <div>
        <h1>{standalone ? "Loading instance data" : "Connecting to daemon"}</h1>
        <p>Loading the current instance, workforce, workflows, and runs.</p>
      </div>
    </section>
  );
}

export function DaemonErrorState({
  error,
  retry,
  standalone = false,
}: {
  error: Error;
  retry: () => void;
  standalone?: boolean;
}) {
  // A 401/403 means the daemon (or its front door) is reachable and
  // answering — it is refusing this request's credentials, not "unavailable"
  // (#2916). Render that as its own state, distinct from the network/daemon
  // failure below, and keep the HTTP status visible.
  if (error instanceof DaemonAuthError) {
    const heading = error.status === 401 ? "Authentication required" : "Access denied";
    return (
      <section className="daemon-state daemon-state-auth" role="alert">
        <div>
          <h1>{heading}</h1>
          <p>
            {error.message} (HTTP {error.status})
          </p>
        </div>
        <button className="reconnect-button" onClick={retry} type="button">
          {standalone ? "Reload" : "Try again"}
        </button>
      </section>
    );
  }

  return (
    <section className="daemon-state daemon-state-error" role="alert">
      <div>
        <h1>{standalone ? "Instance data unavailable" : "Daemon unavailable"}</h1>
        <p>{error.message} No fixture data has been substituted.</p>
      </div>
      <button className="reconnect-button" onClick={retry} type="button">
        {standalone ? "Reload" : "Reconnect"}
      </button>
    </section>
  );
}
