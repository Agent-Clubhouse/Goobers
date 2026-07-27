import { useCobrand } from "../cobrand";

interface SupportLink {
  label: string;
  url: string;
}

// SupportFooter renders the per-instance support channels (CBR) in the sidebar:
// the configured Docs / Get help / Chat links, followed by any custom links.
// Each entry is omitted when its URL is unset, and the footer renders nothing
// when no channel is configured — an unbranded instance shows no footer at all.
export function SupportFooter() {
  const { config } = useCobrand();
  const { support } = config;

  const links: SupportLink[] = [];
  if (support.docsUrl) links.push({ label: "Docs", url: support.docsUrl });
  if (support.issuesUrl) links.push({ label: "Get help", url: support.issuesUrl });
  if (support.chatUrl) links.push({ label: "Chat", url: support.chatUrl });
  for (const link of support.links) {
    if (link.label && link.url) links.push({ label: link.label, url: link.url });
  }

  if (links.length === 0) return null;

  return (
    <div className="support-footer">
      {links.map((link) => (
        <a
          key={`${link.label}:${link.url}`}
          href={link.url}
          target="_blank"
          rel="noreferrer"
        >
          {link.label}
        </a>
      ))}
    </div>
  );
}
