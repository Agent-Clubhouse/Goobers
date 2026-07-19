export function DaemonLoadingState() {
  return (
    <section aria-live="polite" className="daemon-state" role="status">
      <span aria-hidden="true" className="loading-mark" />
      <div>
        <h1>Connecting to daemon</h1>
        <p>Loading the current instance, workforce, workflows, and runs.</p>
      </div>
    </section>
  );
}

export function DaemonErrorState({
  error,
  retry,
}: {
  error: Error;
  retry: () => void;
}) {
  return (
    <section className="daemon-state daemon-state-error" role="alert">
      <div>
        <h1>Daemon unavailable</h1>
        <p>{error.message} No fixture data has been substituted.</p>
      </div>
      <button className="reconnect-button" onClick={retry} type="button">
        Reconnect
      </button>
    </section>
  );
}
