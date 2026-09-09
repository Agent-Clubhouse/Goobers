import { useId, useState } from "react";
import { Icon } from "../ui/Icon";

export function DisclosureSection({
  children,
  count,
  defaultOpen = false,
  eyebrow,
  title,
}: {
  children: React.ReactNode;
  count?: number;
  defaultOpen?: boolean;
  eyebrow: string;
  title: string;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const headingId = useId();
  const contentId = useId();

  return (
    <section aria-labelledby={headingId} className="content-section disclosure-section">
      <div className="section-heading disclosure-section-heading">
        <button
          aria-controls={contentId}
          aria-expanded={open}
          className="disclosure-section-toggle"
          onClick={() => setOpen((current) => !current)}
          type="button"
        >
          <span>
            <span className="section-kicker">{eyebrow}</span>
            <span className="disclosure-section-title" id={headingId} role="heading" aria-level={2}>
              {title}
            </span>
          </span>
          <span className="disclosure-section-meta">
            {count !== undefined && <span className="section-count">{count}</span>}
            <span aria-hidden="true" className="disclosure-section-chevron">
              <Icon name="chevron" size={16} />
            </span>
          </span>
        </button>
      </div>
      {open && (
        <div className="disclosure-section-content" id={contentId}>
          {children}
        </div>
      )}
    </section>
  );
}
