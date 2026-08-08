import { useState } from "react";
import type { DaemonClient, RunEvent } from "../api/types";
import {
  eventHeading,
  eventStage,
  eventSummary,
  evidenceDecision,
  formatElapsed,
  keyMomentEvidence,
  keyMomentLabel,
  keyMoments,
  type KeyMoment,
} from "../runDetailData";
import { EvidencePayload } from "./RunStageInspector";

// KeyMomentsDigest is #2537's curated read of a run: only the events that
// carry a decision, a gate evaluation, or a handoff/escalation, ordered by
// significance rather than durable sequence — a companion to EventLedger's
// exhaustive chronological view, not a replacement for it. Selecting an
// entry both syncs the shared graph/inspector selection (so the topology
// still reflects what is being examined) and expands its state change and
// payload inline, so reaching the payload never requires first locating the
// corresponding node in the graph.
export function KeyMomentsDigest({
  client,
  events,
  onSelect,
  runId,
  runStartedAt,
  selectedSeq,
}: {
  client: DaemonClient;
  events: RunEvent[];
  onSelect: (event: RunEvent, revealInspector?: boolean) => void;
  runId: string;
  runStartedAt: string;
  selectedSeq: number;
}) {
  const [expandedKey, setExpandedKey] = useState<string>();
  const moments = keyMoments(events);

  const momentKey = (moment: KeyMoment) => `${moment.event.branch}-${moment.event.seq}`;

  const toggle = (moment: KeyMoment) => {
    const key = momentKey(moment);
    setExpandedKey((current) => (current === key ? undefined : key));
    onSelect(moment.event);
  };

  return (
    <section aria-labelledby="key-moments-title" className="key-moments-digest">
      <div className="panel-heading-row">
        <div>
          <p className="section-kicker">Digest</p>
          <h2 id="key-moments-title">Key moments</h2>
        </div>
        <span className="graph-legend">Ordered by significance</span>
      </div>
      {moments.length === 0 ? (
        <div className="empty-detail" role="status">
          <strong>No decisions, gate evaluations, or handoffs recorded yet</strong>
        </div>
      ) : (
        <ol>
          {moments.map((moment) => {
            const { event, kind } = moment;
            const key = momentKey(moment);
            const expanded = expandedKey === key;
            const selected = event.seq === selectedSeq;
            const heading = eventHeading(event);
            const summary = eventSummary(event, evidenceDecision(events, event, runId), runId);
            return (
              <li
                className={`key-moment-item ${selected ? "key-moment-item-active" : ""}`}
                key={key}
              >
                <button
                  aria-current={selected ? "true" : undefined}
                  aria-expanded={expanded}
                  aria-label={`${expanded ? "Collapse" : "Expand"} key moment sequence ${event.seq}: ${heading}. ${summary}`}
                  className="key-moment-button"
                  onClick={() => toggle(moment)}
                  type="button"
                >
                  <span className={`key-moment-kind key-moment-kind-${kind}`}>
                    {keyMomentLabel(kind)}
                  </span>
                  <span className="key-moment-copy">
                    <span className="key-moment-meta mono">
                      Seq {event.seq} · {eventStage(event)} · Elapsed{" "}
                      {formatElapsed(runStartedAt, event.time)}
                    </span>
                    <strong>{heading}</strong>
                    <span>{summary}</span>
                  </span>
                  <span aria-hidden="true" className="key-moment-disclosure">
                    {expanded ? "Hide" : "Show"}
                  </span>
                </button>
                {expanded && (
                  <KeyMomentPayload client={client} event={event} events={events} runId={runId} summary={summary} />
                )}
              </li>
            );
          })}
        </ol>
      )}
    </section>
  );
}

// Computing keyMomentEvidence walks and sorts the full event list, so it is
// only worth paying for the one moment a reader actually expands — not for
// every collapsed row on every render.
function KeyMomentPayload({
  client,
  event,
  events,
  runId,
  summary,
}: {
  client: DaemonClient;
  event: RunEvent;
  events: RunEvent[];
  runId: string;
  summary: string;
}) {
  const evidence = keyMomentEvidence(events, event, runId);
  return (
    <div className="key-moment-payload">
      <div className="repass-context">
        <span>State change</span>
        <strong>{summary}</strong>
      </div>
      {evidence ? (
        <EvidencePayload client={client} event={evidence} runId={runId} />
      ) : (
        <p className="artifact-access-note">No recorded payload for this moment.</p>
      )}
    </div>
  );
}
