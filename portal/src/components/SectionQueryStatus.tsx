export function SectionQueryStatus({
  error,
  loading,
  message,
  retry,
}: {
  error?: boolean;
  loading?: boolean;
  message?: string;
  retry?: () => void;
}) {
  const state = error ? "error" : loading ? "loading" : "idle";
  return (
    <div
      aria-busy={loading || undefined}
      aria-live="polite"
      className={`section-query-status${error ? " section-query-status-error" : ""}`}
      data-state={state}
      role={error ? "alert" : "status"}
    >
      {loading && <span aria-hidden="true" className="section-query-spinner" />}
      {message && <span>{message}</span>}
      {error && retry && (
        <button className="text-button" onClick={retry} type="button">
          Retry
        </button>
      )}
    </div>
  );
}
