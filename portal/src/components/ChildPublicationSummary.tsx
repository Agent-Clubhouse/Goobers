import type { ChildPublicationHistory, ChildPublicationItem } from "../api/types";
import { ActionLink } from "../ui/Action";

function safePRLink(value: string): boolean {
  try {
    const link = new URL(value);
    return (link.protocol === "https:" || link.protocol === "http:") && !!link.hostname && !link.username && !link.password;
  } catch { return false; }
}

function validItem(item: ChildPublicationItem): boolean {
  if (!item || (item.action !== "branch" && item.action !== "pr") || !["prepared", "effect_pending", "confirmed"].includes(item.state)) return false;
  if (item.needsHuman !== (item.state === "effect_pending") || typeof item.head !== "string" || typeof item.base !== "string" || typeof item.commit !== "string") return false;
  if (item.action === "pr" && item.state === "confirmed") return typeof item.pullRequestUrl === "string" && safePRLink(item.pullRequestUrl) && Number.isSafeInteger(item.pullRequestNumber) && item.pullRequestNumber > 0;
  return item.pullRequestUrl === "" && item.pullRequestNumber === 0;
}

export function ChildPublicationSummary({ publication }: { publication?: ChildPublicationHistory }) {
  if (!publication) return null;
  if (publication.status === "expired") return null;
  if (publication.status !== "recorded" || !Array.isArray(publication.items) || publication.items.length > 2 || !publication.items.every(validItem) || new Set(publication.items.map((item) => item.action)).size !== publication.items.length) {
    return <p>Publication details unavailable.</p>;
  }
  if (publication.items.length === 0) return null;
  return <ul aria-label="Child publication">
    {publication.items.map((item) => <li key={item.action}>
      {item.state === "effect_pending" ? <span>{item.action === "pr" ? "PR" : "Branch"} publication needs a human. The provider outcome is unconfirmed.</span> :
        item.state === "prepared" ? <span>{item.action === "pr" ? "PR" : "Branch"} publication prepared; no provider effect has been recorded.</span> :
          item.action === "pr" ? <ActionLink href={item.pullRequestUrl} rel="noreferrer" target="_blank">Open child PR #{item.pullRequestNumber}</ActionLink> :
            <span>Published branch <code>{item.head}</code>.</span>}
    </li>)}
  </ul>;
}
