import type { RunSummary } from "../api/types";
import { runWorkTitle } from "../operationalData";

export function workItemHref(run: RunSummary): string | undefined {
  const raw = run.operator?.issue?.url;
  if (!raw || raw.length > 2048 || /[\u0000-\u001f\u007f]/.test(raw) ||
      !/^https?:\/\/[^/\\\s]/i.test(raw)) return undefined;
  try {
    const url = new URL(raw);
    if (
      (url.protocol === "https:" || url.protocol === "http:") &&
      url.hostname && !url.username && !url.password
    ) return raw;
  } catch {
    // Old or malformed metadata must not turn a readable title into a link.
  }
  return undefined;
}

export function RunWorkTitle({
  run,
  separator = " ",
  numberOnly = false,
}: {
  run: RunSummary;
  separator?: string;
  numberOnly?: boolean;
}) {
  const issue = run.operator?.issue;
  const title = numberOnly && issue ? `#${issue.number}` : runWorkTitle(run, separator) ?? run.id;
  const href = workItemHref(run);
  if (!issue || !href) return <>{title}</>;

  // Preserve a published display title verbatim and link only its exact ID,
  // never a prefix of a different ID or a number guessed from the title.
  const token = `#${issue.number}`;
  let start = title.indexOf(token);
  while (start !== -1) {
    const end = start + token.length;
    if (
      (start === 0 || !/[\w#]/.test(title[start - 1])) &&
      (end === title.length || !/[\w-]/.test(title[end]))
    ) {
      return (
        <>
          {title.slice(0, start)}
          <a className="work-item-link" href={href} target="_blank" rel="noopener noreferrer">
            {token}
          </a>
          {title.slice(end)}
        </>
      );
    }
    start = title.indexOf(token, start + token.length);
  }
  return <>{title}</>;
}
