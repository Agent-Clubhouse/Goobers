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
  return (
    <div
      aria-live="polite"
      className={`section-query-status${error ? " section-query-status-error" : ""}`}
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
