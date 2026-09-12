import { useId, useState } from "react";
import { Icon } from "../ui/Icon";

export function DisclosureSection({
  children,
  count,
  defaultOpen = false,
  eyebrow,
  onOpenChange,
  open: controlledOpen,
  title,
}: {
  children: React.ReactNode;
  count?: number;
  defaultOpen?: boolean;
  eyebrow?: string;
  onOpenChange?: (open: boolean) => void;
  open?: boolean;
  title: string;
}) {
  const [uncontrolledOpen, setUncontrolledOpen] = useState(defaultOpen);
  const open = controlledOpen ?? uncontrolledOpen;
  const headingId = useId();
  const contentId = useId();
  const setOpen = (nextOpen: boolean) => {
    if (controlledOpen === undefined) {
      setUncontrolledOpen(nextOpen);
    }
    onOpenChange?.(nextOpen);
  };

  return (
    <section aria-labelledby={headingId} className="content-section disclosure-section">
      <div className="section-heading disclosure-section-heading">
        <button
          aria-controls={contentId}
          aria-expanded={open}
          className="disclosure-section-toggle"
          onClick={() => setOpen(!open)}
          type="button"
        >
          <span>
            {eyebrow && <span className="section-kicker">{eyebrow}</span>}
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
