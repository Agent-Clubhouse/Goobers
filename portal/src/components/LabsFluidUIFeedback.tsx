import { useCallback, useEffect, useState, type FormEvent } from "react";
import { DaemonApiError } from "../api/errors";
import type { DaemonClient, FluidUIRequest, FluidUIRequestStatus } from "../api/types";
import { formatRelativeTimestamp } from "../dateTime";
import { useFluidUI } from "../fluidUI";
import { routeHash } from "../routing";
import { Action } from "../ui/Action";

// Mirrors internal/fluidui.MaxPromptBytes; the daemon enforces the real bound.
const maxPromptLength = 2000;
const refreshIntervalMs = 5000;
// Mirrors internal/fluidui.MaxAttempts.
const maxAttempts = 5;

const statusLabels: Record<FluidUIRequestStatus, string> = {
  queued: "Queued",
  implementing: "Implementing",
  ready_to_preview: "Ready to preview",
  applied: "Applied",
  failed: "Failed",
  unsupported: "Unsupported",
  reverted: "Reverted",
};

function inFlight(request: FluidUIRequest) {
  return request.status === "queued" || request.status === "implementing";
}

function describeError(cause: unknown): string {
  if (cause instanceof DaemonApiError) {
    switch (cause.code) {
      case "triggers_unavailable":
        return "This connection cannot start workflows, so requests cannot be submitted here.";
      case "too_many_requests":
        return "You already have the maximum number of requests in progress. Wait for one to finish.";
      case "revert_stale":
        return "Your customization changed after this request was applied, so it cannot be reverted. Use Undo instead.";
    }
  }
  return cause instanceof Error ? cause.message : String(cause);
}

/**
 * Labs > Fluid UI > Feedback (#8417): describe a change in plain words, follow
 * the request's run, then preview and apply the validated candidate. Status is
 * derived from the run by the daemon, so "ready" is never shown early.
 */
export function LabsFluidUIFeedback({ client }: { client: DaemonClient }) {
  const fluid = useFluidUI();
  const [requests, setRequests] = useState<FluidUIRequest[]>();
  const [loadError, setLoadError] = useState<string>();
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [message, setMessage] = useState<string>();
  const supported = typeof client.listLabsFluidUIRequests === "function";

  const load = useCallback(
    async (signal?: AbortSignal) => {
      if (!client.listLabsFluidUIRequests) return;
      try {
        const next = await client.listLabsFluidUIRequests({ signal });
        setRequests(next);
        setLoadError(undefined);
      } catch (cause) {
        if (signal?.aborted) return;
        setLoadError(describeError(cause));
      }
    },
    [client],
  );

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const pending = requests?.some(inFlight) ?? false;
  useEffect(() => {
    if (!pending) return;
    const timer = globalThis.setInterval(() => void load(), refreshIntervalMs);
    return () => globalThis.clearInterval(timer);
  }, [pending, load]);

  if (!supported) {
    return (
      <p className="inline-empty">Fluid UI requests are not available on this connection.</p>
    );
  }

  const act = async (label: string, write: () => Promise<void>) => {
    setBusy(true);
    setError(undefined);
    setMessage(undefined);
    try {
      await write();
      setMessage(label);
      await load();
    } catch (cause) {
      setError(describeError(cause));
    } finally {
      setBusy(false);
    }
  };

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const text = prompt.trim();
    if (!text || !client.submitLabsFluidUIRequest) return;
    void act("Request submitted. A workflow run is proposing a customization.", async () => {
      await client.submitLabsFluidUIRequest!(text);
      setPrompt("");
    });
  };

  const retry = (request: FluidUIRequest) =>
    act("Request resubmitted.", async () => {
      await client.retryLabsFluidUIRequest!(request.id);
    });

  const apply = (request: FluidUIRequest) =>
    act("Customization applied. Undo or Revert restores the previous one.", async () => {
      fluid.stopPreview();
      await client.applyLabsFluidUIRequest!(request.id);
      fluid.reload();
    });

  const revert = (request: FluidUIRequest) =>
    act("Customization reverted.", async () => {
      await client.revertLabsFluidUIRequest!(request.id);
      fluid.reload();
    });

  return (
    <div className="fluid-ui-feedback">
      <section className="content-section fluid-ui-form" aria-labelledby="fluid-ui-request">
        <h2 id="fluid-ui-request">Describe a change</h2>
        <p className="fluid-ui-meta">
          A workflow turns your words into a customization of this Portal&apos;s layout. Nothing changes until
          you preview and apply it. Only you can see your requests.
        </p>
        <form onSubmit={submit}>
          <label className="fluid-ui-prompt">
            Request
            <textarea
              aria-label="Request"
              maxLength={maxPromptLength}
              placeholder="Show failing runs first and hide the queue panel"
              rows={3}
              value={prompt}
              onChange={(event) => setPrompt(event.target.value)}
            />
          </label>
          <div className="fluid-ui-actions">
            <Action disabled={busy || !prompt.trim()} type="submit" variant="primary">
              Submit request
            </Action>
          </div>
        </form>
        {message && (
          <p className="fluid-ui-message" role="status">
            {message}
          </p>
        )}
        {error && (
          <p className="fluid-ui-error" role="alert">
            {error}
          </p>
        )}
      </section>

      <section className="content-section" aria-labelledby="fluid-ui-requests">
        <h2 id="fluid-ui-requests">Your requests</h2>
        {loadError && (
          <p className="fluid-ui-error" role="alert">
            Could not load your requests: {loadError}
          </p>
        )}
        {requests === undefined && !loadError && <p className="inline-empty">Loading your requests…</p>}
        {requests?.length === 0 && <p className="inline-empty">You have not made any requests yet.</p>}
        {requests && requests.length > 0 && (
          <ul className="fluid-ui-requests">
            {requests.map((request) => (
              <FluidUIRequestItem
                busy={busy}
                key={request.id}
                onApply={apply}
                onPreview={(candidate) => fluid.startPreview(candidate)}
                onRetry={retry}
                onRevert={revert}
                request={request}
              />
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

function FluidUIRequestItem({
  request,
  busy,
  onPreview,
  onApply,
  onRetry,
  onRevert,
}: {
  request: FluidUIRequest;
  busy: boolean;
  onPreview: (candidate: NonNullable<FluidUIRequest["candidate"]>) => void;
  onApply: (request: FluidUIRequest) => void;
  onRetry: (request: FluidUIRequest) => void;
  onRevert: (request: FluidUIRequest) => void;
}) {
  const candidate = request.candidate;
  return (
    <li className={`fluid-ui-request fluid-ui-request-${request.status}`}>
      <div className="fluid-ui-request-heading">
        <span className="fluid-ui-request-status">{statusLabels[request.status]}</span>
        <span className="fluid-ui-meta">
          {formatRelativeTimestamp(Date.parse(request.updatedAt))}
          {request.attempt > 1 && <> · attempt {request.attempt}</>}
          {request.runId && (
            <>
              {" · "}
              <a href={routeHash({ page: "run", id: request.runId })}>View run</a>
            </>
          )}
        </span>
      </div>
      <p className="fluid-ui-request-prompt">{request.prompt}</p>
      {request.status === "ready_to_preview" && candidate && (
        <div className="fluid-ui-request-notice" role="status">
          <strong>Your UX is ready.</strong> Preview it locally, then apply it if you like it.
          <div className="fluid-ui-actions">
            <Action disabled={busy} onClick={() => onPreview(candidate)} variant="secondary">
              Preview
            </Action>
            <Action disabled={busy} onClick={() => onApply(request)} variant="primary">
              Apply
            </Action>
          </div>
        </div>
      )}
      {request.status === "unsupported" && (
        <p className="fluid-ui-meta">
          Fluid UI cannot do this yet
          {request.unmetNeed && (
            <>
              {" "}
              (<span className="mono">{request.unmetNeed}</span>)
            </>
          )}
          . The request is recorded as feedback.
        </p>
      )}
      {request.status === "unsupported" && request.attempt < maxAttempts && (
        <div className="fluid-ui-actions">
          <Action disabled={busy} onClick={() => onRetry(request)} variant="secondary">
            Retry
          </Action>
        </div>
      )}
      {request.status === "failed" && (
        <div className="fluid-ui-request-problem" role="alert">
          <p>The workflow could not produce a valid customization{request.detail ? `: ${request.detail}` : "."}</p>
          {request.errors && request.errors.length > 0 && (
            <ul>
              {request.errors.map((problem) => (
                <li key={problem}>{problem}</li>
              ))}
            </ul>
          )}
          {request.attempt < maxAttempts && (
            <Action disabled={busy} onClick={() => onRetry(request)} variant="secondary">
              Retry
            </Action>
          )}
        </div>
      )}
      {request.status === "applied" && (
        <div className="fluid-ui-actions">
          <Action disabled={busy} onClick={() => onRevert(request)} variant="secondary">
            Revert
          </Action>
        </div>
      )}
    </li>
  );
}
