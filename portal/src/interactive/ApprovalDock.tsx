import { useEffect, useState, type KeyboardEvent } from "react";
import { Action } from "../ui/Action";

// Composer-docked approvals and numbered provider options follow Clubhouse's
// ApprovalDock.tsx at 3309665c (MIT; see CLUBHOUSE-LICENSE.txt).
export interface PermissionOption { optionId: "allow_once" | "reject_once"; label: string }
interface RequestBase { requestId: string; turnId: string; sequence: number; failure?: string; pending?: boolean; responseSequence?: number }
export interface PermissionRequest extends RequestBase {
  kind: "permission"; title: string; description?: string; preview: string[]; options: PermissionOption[];
}
export interface Question {
  id: string; prompt: string; options: {id: string; label: string; description?: string}[];
  multiSelect?: boolean; allowFreeText?: boolean;
}
export interface QuestionRequest extends RequestBase { kind: "question"; questions: Question[] }
export type ApprovalRequest = PermissionRequest | QuestionRequest;
export type ApprovalAnswer =
  | {type: "permission.respond"; requestId: string; optionId: "allow_once" | "reject_once"; message?: string}
  | {type: "question.answer"; requestId: string; answers: Record<string, string[]>};

function optionKey(event: KeyboardEvent, count: number): number {
  if (event.target !== event.currentTarget || event.altKey || event.ctrlKey || event.metaKey) return -1;
  return /^[1-9]$/.test(event.key) && Number(event.key) <= count ? Number(event.key) - 1 : -1;
}
interface CardProps { disabled: boolean; denyMessage?: boolean; onAnswer: (answer: ApprovalAnswer) => Promise<boolean> }
export function PermissionCard({request, disabled, denyMessage, onAnswer}: CardProps & {request: PermissionRequest}) {
  const [status, setStatus] = useState<"idle" | "sending" | "accepted">("idle");
  const sent = status !== "idle";
  useEffect(() => { if (request.failure) setStatus("idle"); }, [request.failure, request.responseSequence]);
  const [feedback, setFeedback] = useState("");
  const reviewable = request.preview.some(text => text.trim());
  async function choose(option: PermissionOption) {
    if (disabled || request.pending || sent || (option.optionId === "allow_once" && !reviewable)) return;
    setStatus("sending");
    try { setStatus(await onAnswer({type: "permission.respond", requestId: request.requestId, optionId: option.optionId, ...(denyMessage && option.optionId === "reject_once" && feedback.trim() ? {message: feedback.trim()} : {})}) ? "accepted" : "idle"); }
    catch { setStatus("idle"); }
  }
  return <section className="ix-approval-card" aria-label="Permission request" tabIndex={0} onKeyDown={event => {
    const index = optionKey(event, request.options.length);
    if (index >= 0 && !feedback.trim()) { event.preventDefault(); void choose(request.options[index]); }
  }}>
    <h3>{request.title}</h3>
    {request.description && request.description !== request.title && <p>{request.description}</p>}
    <div className="ix-approval-preview">{request.preview.map((text, index) => <pre key={index}>{text}</pre>)}</div>
    {!reviewable && <p role="alert">The requested action has no preview. Approval is unavailable until its details are provided.</p>}
    {request.failure && <p role="alert">{request.failure}</p>}
    <div className="ix-approval-options">{request.options.map((option, index) => <Action key={option.optionId}
      disabled={disabled || request.pending || sent || (option.optionId === "allow_once" && !reviewable)} onClick={() => void choose(option)}>
      <span aria-hidden="true">{index + 1}</span>{option.label}
    </Action>)}</div>
    {denyMessage && request.options.some(option => option.optionId === "reject_once") && <label>Deny with a message<input aria-label="Denial message" value={feedback} disabled={disabled || request.pending || sent} onChange={event => setFeedback(event.target.value)} /></label>}
    {feedback.trim() && <p>Number shortcuts are paused while you have unsent text. Choose Deny to send your message.</p>}
    {status === "sending" && <p role="status">Sending response…</p>}
    {(status === "accepted" || request.pending) && <p role="status">Response accepted. Waiting for the driver to resolve this request.</p>}
  </section>;
}

export function QuestionCard({request, disabled, onAnswer}: CardProps & {request: QuestionRequest}) {
  const [answers, setAnswers] = useState<Record<string, string[]>>({});
  const [free, setFree] = useState<Record<string, string>>({});
  const [status, setStatus] = useState<"idle" | "sending" | "accepted">("idle");
  const sent = status !== "idle";
  useEffect(() => { if (request.failure) setStatus("idle"); }, [request.failure, request.responseSequence]);
  function choose(question: Question, id: string) {
    if (disabled || request.pending || sent) return;
    setFree(current => ({...current, [question.id]: ""}));
    setAnswers(current => {
      const prior = current[question.id] ?? [];
      return {...current, [question.id]: question.multiSelect ? prior.includes(id) ? prior.filter(value => value !== id) : [...prior, id] : [id]};
    });
  }
  const resolved = Object.fromEntries(request.questions.map(question => [question.id, free[question.id]?.trim() ? [free[question.id].trim()] : answers[question.id] ?? []]));
  const complete = request.questions.length > 0 && request.questions.every(question => resolved[question.id].length > 0);
  async function submit() {
    if (!complete || disabled || request.pending || sent) return;
    setStatus("sending");
    try { setStatus(await onAnswer({type: "question.answer", requestId: request.requestId, answers: resolved}) ? "accepted" : "idle"); }
    catch { setStatus("idle"); }
  }
  return <section className="ix-approval-card" aria-label="Question" tabIndex={0} onKeyDown={event => {
    const first = request.questions[0];
    const index = optionKey(event, first?.options.length ?? 0);
    if (index >= 0) { event.preventDefault(); choose(first, first.options[index].id); }
  }}>
    {!request.questions.length && <p role="alert">Question details are unavailable. No answer can be sent.</p>}
    {request.questions.map(question => <fieldset key={question.id} disabled={disabled || request.pending || sent}>
      <legend>{question.prompt}</legend>
      <div className="ix-approval-options">{question.options.map((option, index) => <button type="button" key={option.id}
        aria-pressed={!free[question.id]?.trim() && (answers[question.id] ?? []).includes(option.id)} onClick={() => choose(question, option.id)}>
        <span aria-hidden="true">{index + 1}</span><span>{option.label}{option.description && <small>{option.description}</small>}</span>
      </button>)}</div>
      {question.allowFreeText && <label>Written answer<input value={free[question.id] ?? ""} onChange={event => setFree(current => ({...current, [question.id]: event.target.value}))} aria-label={`Answer: ${question.prompt}`} /></label>}
    </fieldset>)}
    {request.failure && <p role="alert">{request.failure}</p>}
    <Action disabled={!complete || disabled || request.pending || sent} onClick={() => void submit()}>Send answer</Action>
    {status === "sending" && <p role="status">Sending answer…</p>}
    {(status === "accepted" || request.pending) && <p role="status">Answer accepted. Waiting for the driver to resolve this request.</p>}
  </section>;
}

export function ApprovalDock({requests, disabled, denyMessage, onAnswer}: CardProps & {requests: ApprovalRequest[]}) {
  const request = requests[0];
  if (!request) return null;
  return <div className="ix-approval-dock">
    {requests.length > 1 && <p className="ix-approval-count">1 of {requests.length} requests</p>}
    {request.kind === "permission" ? <PermissionCard key={`${request.turnId}:${request.requestId}`} request={request} disabled={disabled} denyMessage={denyMessage} onAnswer={onAnswer} /> :
      <QuestionCard key={`${request.turnId}:${request.requestId}`} request={request} disabled={disabled} onAnswer={onAnswer} />}
  </div>;
}
