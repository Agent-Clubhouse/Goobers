import { useEffect, useState } from "react";
import { DaemonAuthError } from "../api/errors";

export function DaemonLoadingState({ standalone = false }: { standalone?: boolean }) {
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    const timer = window.setTimeout(() => setVisible(true), 200);
    return () => window.clearTimeout(timer);
  }, []);

  if (!visible) {
    return null;
  }

  return (
    <section aria-live="polite" className="daemon-state" role="status">
      <span aria-hidden="true" className="loading-mark" />
      <div>
        <h1>{standalone ? "Loading instance data" : "Connecting to Goobers Instance"}</h1>
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
          {standalone ? "Reload" : "Retry"}
        </button>
      </section>
    );
  }

  return (
    <section className="daemon-state daemon-state-error" role="alert">
      <div>
        <h1>{standalone ? "Couldn't load this instance" : "Couldn't load Goobers data"}</h1>
        <p>
          {standalone
            ? "Goobers couldn't read the local instance data. Reload to try again."
            : "The portal couldn't load data from the Goobers daemon. Reconnect to try again."}
        </p>
      </div>
      <button className="reconnect-button" onClick={retry} type="button">
        {standalone ? "Reload" : "Reconnect"}
      </button>
    </section>
  );
}
