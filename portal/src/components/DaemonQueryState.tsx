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
