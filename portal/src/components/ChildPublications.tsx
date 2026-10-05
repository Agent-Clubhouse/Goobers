import { useRef, useState } from "react";
import { DaemonApiError, DaemonAuthError } from "../api/errors";
import type { ChildPublicationSummary, DaemonClient } from "../api/types";

export function ChildPublications({ client, runId, publications, available, reason, refresh }: {
  client: DaemonClient; runId: string; publications: ChildPublicationSummary[];
  available: boolean; reason?: string; refresh: () => void;
}) {
  if (publications.length === 0) return null;
  return <section aria-label="Child publications">
    <h3>Publication results</h3>
    <p>These records show whether the child’s branch or pull request was confirmed.</p>
    {!available && reason && <p>{reason}</p>}
    <ul className="child-workflows-list">{publications.map((publication) => <Publication key={`${runId}:${publication.action}:${publication.intentDigest}`} client={client} runId={runId} publication={publication} available={available} refresh={refresh} />)}</ul>
  </section>;
}

function Publication({ client, runId, publication, available, refresh }: {
  client: DaemonClient; runId: string; publication: ChildPublicationSummary; available: boolean; refresh: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState(false);
  const [notice, setNotice] = useState("");
  const key = useRef<string | undefined>(undefined);
  const [checked, setChecked] = useState<ChildPublicationSummary>();
  const current = publication.state === "confirmed" ? publication : checked ?? publication;
  const label = current.action === "branch" ? "Branch" : "Pull request";
  async function check() {
    setBusy(true); setNotice("");
    key.current ??= crypto.randomUUID();
    try {
      const result = await client.checkChildPublication(publication.sourceRunId || runId, key.current, { action: publication.action, expectedIntentDigest: publication.intentDigest });
      setChecked(result.publication); key.current = undefined; setPending(false);
      setNotice(result.publication.state === "confirmed"
        ? "Publication confirmed. This check did not restart the child."
        : result.publication.observation === "branch_changed"
          ? "The remote branch changed. Human review is still needed."
          : "The expected publication is not confirmed. It still needs attention.");
      refresh();
    } catch (error) {
      if (error instanceof DaemonAuthError || (error instanceof DaemonApiError && error.status >= 400 && error.status < 500 && error.status !== 429)) {
        key.current = undefined; setPending(false);
        setNotice("The check was refused. Refresh and review your access and the recorded publication.");
        refresh();
      } else {
        setPending(true);
        setNotice("The check’s outcome is unknown. Retry the same check to recover its recorded result.");
      }
    } finally { setBusy(false); }
  }
  const link = safePublicationURL(current.pullRequestUrl);
  return <li>
    <div><strong>{label}</strong><span>{current.state === "confirmed" ? "Confirmed" : current.state === "prepared" ? "Prepared; no effect begun" : "Needs confirmation"}</span></div>
    <p>{current.head} → {current.base}</p>
    {current.sourceRunId && <p>{current.executionEpoch > 0 ? `Requested by human restart ${current.executionEpoch}` : "Requested by original child execution"} · <a href={`#/run/${encodeURIComponent(current.sourceRunId)}`}>Open publishing execution</a></p>}
    {link && <a href={link} target="_blank" rel="noopener noreferrer">Open published PR{current.pullRequestNumber ? ` #${current.pullRequestNumber}` : ""}</a>}
    {current.needsHuman && <p>The publication request may have reached the provider. Its result is not yet confirmed.</p>}
    {current.needsHuman && available && <button type="button" disabled={busy} onClick={() => { void check(); }}>{busy ? "Checking publication…" : pending ? "Retry same publication check" : `Check ${label.toLowerCase()} publication`}</button>}
    {current.checkedAt && <small>Checked {new Date(current.checkedAt).toLocaleString()}</small>}
    {notice && <p role="status">{notice}</p>}
  </li>;
}

function safePublicationURL(value?: string): string | undefined {
  if (!value) return undefined;
  try {
    const url = new URL(value);
    return (url.protocol === "https:" || url.protocol === "http:") && !url.username && !url.password ? value : undefined;
  } catch { return undefined; }
}
